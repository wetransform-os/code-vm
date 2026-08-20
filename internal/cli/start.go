package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/profile"
	"github.com/wetransform/code-vm/internal/session"
)

// newClient is a package variable so tests can substitute a fake runner.
var newClient = lima.NewClient

// agentUser is the guest account the agent runs as. Its UID and GID mirror
// the host user's so virtiofs-shared files are owned by it.
const agentUser = "devuser"

// clientFor returns a limactl client bound to the instance this config names.
// Every command goes through it: the instance is what keeps a throwaway test VM
// from acting on the one in daily use, so no command may default it silently.
func clientFor(c config.Config) lima.Client {
	cl := newClient()
	cl.Instance = c.Instance
	return cl
}

// agentDeps builds the session dependencies from one place, so the exec path
// and `code-vm allow` cannot drift apart on what identity the guest work runs
// under. The numeric ids mirror what provisioning gave the guest account.
func agentDeps(cl lima.Client, c config.Config, profiles []profile.Profile) session.Deps {
	return session.Deps{
		Client:       cl,
		Config:       c,
		AgentUser:    agentUser,
		AgentUID:     os.Getuid(),
		AgentGID:     os.Getgid(),
		AllowDomains: profile.AllowDomains(c.ExtraDomains, profiles),
	}
}

// renderParams gathers the host-derived values the Lima template needs.
// The config may leave the hypervisor and which driver is accelerated unset
// (QEMU/KVM or vz/HVF) as it is a property of the host, not the config.
func renderParams(c config.Config, profiles []profile.Profile) (lima.RenderParams, error) {
	files, err := guest.DataFiles()
	if err != nil {
		return lima.RenderParams{}, err
	}
	files = append(files, profile.GuestFiles(profiles)...)
	vmType, err := config.ResolveVMType(c.VMType, runtime.GOOS)
	if err != nil {
		return lima.RenderParams{}, err
	}
	return lima.RenderParams{
		AgentUser:    agentUser,
		AgentUID:     os.Getuid(),
		AgentGID:     os.Getgid(),
		VMType:       vmType,
		DataFiles:    files,
		AllowDomains: profile.AllowDomains(c.ExtraDomains, profiles),
	}, nil
}

// renderInstanceFile writes the rendered Lima instance to a temp file and
// returns its path. The caller is responsible for removing it.
func renderInstanceFile(c config.Config, profiles []profile.Profile) (string, error) {
	p, err := renderParams(c, profiles)
	if err != nil {
		return "", err
	}
	body, err := lima.Render(c, p)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "code-sandbox-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp instance file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod temp instance file: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		return "", fmt.Errorf("write temp instance file: %w", err)
	}
	return f.Name(), nil
}

// ensureRunning brings the sandbox VM up if it is not already running.
// The rendered config is applied on every start, so a code-vm upgrade or a
// config change is picked up without a separate migration step: the
// mode:data guest files carry overwrite: true. An absent instance is created
// from the rendered template; an existing one cannot be started with a
// template argument (limactl refuses), so its stored config is replaced with
// a freshly resolved render first.
//
// started reports whether this call actually booted the VM (status was not
// "Running" on entry). Callers use it to gate work that must run once per
// boot but never on a plain invocation against an already-running VM.
func ensureRunning(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	status, err := cl.Status(ctx)
	if err != nil {
		return false, err
	}
	if status == "Running" {
		return false, nil
	}
	path, err := renderInstanceFile(c, profiles)
	if err != nil {
		return false, err
	}
	defer os.Remove(path)
	if status == "" {
		return true, cl.Start(ctx, path)
	}
	dir, err := cl.InstanceDir(ctx)
	if err != nil {
		return false, err
	}
	if dir == "" {
		return false, fmt.Errorf("cannot locate the %s instance directory", lima.InstanceName)
	}
	if err := cl.ResolveConfigInto(ctx, path, filepath.Join(dir, "lima.yaml")); err != nil {
		return false, err
	}
	return true, cl.StartExisting(ctx)
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the sandbox VM (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, profiles, cfgPath, err := loadConfigWithProfiles()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			// Resolve (and render) before touching the guest at all: resolution
			// needs nothing from the guest, so an unmapped secret must abort
			// before ensureRunning boots anything. Unlike shell's fast path,
			// start always resolves — it is an explicit command, not the
			// per-invocation hot path, so resolving even against an
			// already-running VM is the right cost/behavior tradeoff.
			rendered, err := resolveRendered(ctx, c, profiles, cfgPath, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			cl := clientFor(c)
			if _, err := ensureRunning(ctx, cl, c, profiles); err != nil {
				return err
			}
			return pushRendered(ctx, cl, c, profiles, rendered, cmd.OutOrStdout())
		},
	}
}

// resolveRendered resolves secrets/vars and renders every active profile's
// templates, without touching the guest at all. Split out from
// pushRenderedTemplates so callers that must not mutate any guest state
// before resolution can fail (profile apply: PushProfiles/ApplyAllowlist
// stage a new profile tree and make its domains live, and must not run
// ahead of an unmapped-secret failure) can resolve first and push later.
func resolveRendered(ctx context.Context, c config.Config, profiles []profile.Profile, cfgPath string, out io.Writer) ([]profile.Rendered, error) {
	secretsDecl := profile.DeclaredSecrets(profiles)
	varsDecl := profile.DeclaredVars(profiles)
	templated := false
	for _, p := range profiles {
		if len(p.Templates) > 0 {
			templated = true
		}
	}
	if !templated && len(secretsDecl) == 0 && len(varsDecl) == 0 {
		return nil, nil
	}
	sources, warnings, err := config.LoadSecrets(config.SecretsPathFor(cfgPath))
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	secrets, err := profile.ResolveSecrets(ctx, secretsDecl, sources, hostCommand)
	if err != nil {
		return nil, err
	}
	vars, err := profile.ResolveVars(varsDecl, c.Vars)
	if err != nil {
		return nil, err
	}
	return profile.RenderTemplates(profiles, secrets, vars), nil
}

// pushRendered pushes already-resolved rendered templates into the agent
// home. Callers that already called resolveRendered use this directly, so
// resolution never runs twice.
func pushRendered(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile, rendered []profile.Rendered, out io.Writer) error {
	d := agentDeps(cl, c, profiles)
	for _, r := range rendered {
		if err := session.PushUserFile(ctx, d, r.Content, r.Rel, "0600"); err != nil {
			return err
		}
	}
	if len(rendered) > 0 {
		fmt.Fprintf(out, "Rendered %d template(s) into the sandbox.\n", len(rendered))
	}
	return nil
}

// pushRenderedTemplates resolves secrets/vars and pushes rendered templates
// into the agent home. Callers gate it to start, apply, and boot-causing
// invocations only: resolution may invoke the user's secret manager
// (pinentry), so it must never run on every command.
func pushRenderedTemplates(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile, cfgPath string, out io.Writer) error {
	rendered, err := resolveRendered(ctx, c, profiles, cfgPath, out)
	if err != nil {
		return err
	}
	return pushRendered(ctx, cl, c, profiles, rendered, out)
}

// hostCommand runs a secrets.yaml command through the user's shell on the
// host. The value is stdout ONLY — stderr chatter from a succeeding command
// (gpg warnings, deprecation notices) must never leak into a credential.
// On failure, the captured stderr is returned as display text for the error.
// Neither stdin nor a tty is wired up: a command that needs interactive
// pinentry (rather than an already-cached agent/keyring) will hang or fail
// rather than prompt.
func hostCommand(ctx context.Context, command string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.Stderr, err
		}
		return nil, err
	}
	return out, nil
}
