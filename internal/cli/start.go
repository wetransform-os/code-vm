package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/lima"
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
func agentDeps(cl lima.Client, c config.Config) session.Deps {
	return session.Deps{
		Client:    cl,
		Config:    c,
		AgentUser: agentUser,
		AgentUID:  os.Getuid(),
		AgentGID:  os.Getgid(),
	}
}

// renderParams gathers the host-derived values the Lima template needs.
// The config may leave the hypervisor and which driver is accelerated unset
// (QEMU/KVM or vz/HVF) as it is a property of the host, not the config.
func renderParams(c config.Config) (lima.RenderParams, error) {
	files, err := guest.DataFiles()
	if err != nil {
		return lima.RenderParams{}, err
	}
	vmType, err := config.ResolveVMType(c.VMType, runtime.GOOS)
	if err != nil {
		return lima.RenderParams{}, err
	}
	return lima.RenderParams{
		AgentUser: agentUser,
		AgentUID:  os.Getuid(),
		AgentGID:  os.Getgid(),
		VMType:    vmType,
		DataFiles: files,
	}, nil
}

// renderInstanceFile writes the rendered Lima instance to a temp file and
// returns its path. The caller is responsible for removing it.
func renderInstanceFile(c config.Config) (string, error) {
	p, err := renderParams(c)
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
func ensureRunning(ctx context.Context, cl lima.Client, c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	status, err := cl.Status(ctx)
	if err != nil {
		return err
	}
	if status == "Running" {
		return nil
	}
	path, err := renderInstanceFile(c)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if status == "" {
		return cl.Start(ctx, path)
	}
	dir, err := cl.InstanceDir(ctx)
	if err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("cannot locate the %s instance directory", lima.InstanceName)
	}
	if err := cl.ResolveConfigInto(ctx, path, filepath.Join(dir, "lima.yaml")); err != nil {
		return err
	}
	return cl.StartExisting(ctx)
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the sandbox VM (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), clientFor(c), c)
		},
	}
}
