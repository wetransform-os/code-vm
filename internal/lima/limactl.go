package lima

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Runner executes limactl. It is an interface so command construction can be
// tested without a VM.
type Runner interface {
	Run(ctx context.Context, args ...string) error
	// RunDetached is Run for commands that start the VM: they must outlive the
	// terminal or pane that invoked them.
	RunDetached(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the real limactl binary.
type ExecRunner struct {
	Bin    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Detach returns the argv prefix that moves a boot out of the caller's
	// cgroup, or nil to run it as is. Nil Detach means no prefix.
	Detach func(ctx context.Context) []string
}

func (e ExecRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	bin := e.Bin
	if bin == "" {
		bin = "limactl"
	}
	return exec.CommandContext(ctx, bin, args...)
}

// Run streams the command's output to the configured writers.
func (e ExecRunner) Run(ctx context.Context, args ...string) error {
	cmd := e.command(ctx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// RunDetached runs limactl like Run, wrapped in Detach's prefix when there is
// one, so the VM survives its terminal being torn down.
func (e ExecRunner) RunDetached(ctx context.Context, args ...string) error {
	var prefix []string
	if e.Detach != nil {
		prefix = e.Detach(ctx)
	}
	if len(prefix) == 0 {
		return e.Run(ctx, args...)
	}
	bin := e.Bin
	if bin == "" {
		bin = "limactl"
	}
	name, full := detachedArgv(prefix, bin, args)
	cmd := exec.CommandContext(ctx, name, full...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl %s (via systemd-run): %w", strings.Join(args, " "), err)
	}
	return nil
}

// Output captures stdout. Stderr is streamed so Lima's progress and errors
// stay visible.
func (e ExecRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := e.command(ctx, args...)
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, &out, e.Stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("limactl %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// Client drives one Lima instance.
type Client struct {
	R Runner
	// Instance names the Lima instance to act on. Empty means InstanceName, so
	// a zero Client still targets the default and existing callers keep working.
	Instance string
}

// instance resolves the instance this Client acts on.
func (c Client) instance() string {
	if c.Instance != "" {
		return c.Instance
	}
	return InstanceName
}

// NewClient returns a Client for the default instance wired to the real
// limactl and this process's standard streams.
func NewClient() Client { return NewClientFor(InstanceName) }

// NewClientFor is NewClient for a named instance; the name also labels the
// boot's systemd scope so concurrent VMs are told apart in systemctl.
func NewClientFor(instance string) Client {
	return Client{Instance: instance, R: ExecRunner{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Detach: detachFor(instance, runtime.GOOS, exec.LookPath, ProbeUserManager),
	}}
}

// AgentArgs builds the argv that runs cmd as the agent user in workdir.
// sandbox-exec sources /etc/environment, drops from root to the agent user
// and only then changes into workdir. `limactl shell --workdir` is not used:
// it would cd as limaadmin before sudo, which fails for 0700 directories the
// agent owns (observed with mktemp -d workspaces).
func (c Client) AgentArgs(workdir string, cmd []string) []string {
	args := []string{"shell", c.instance(), "sudo", "/usr/local/bin/sandbox-exec", "--workdir", workdir}
	return append(args, cmd...)
}

// AdminArgs builds the argv that runs cmd as root via limaadmin's sudo.
func (c Client) AdminArgs(cmd []string) []string {
	args := []string{"shell", c.instance(), "sudo"}
	return append(args, cmd...)
}

// ShellArgs builds the argv that enters the guest as limaadmin itself: the
// login shell with no cmd, otherwise cmd. No sudo and no sandbox-exec — this
// is the operator's door, not the agent's.
func (c Client) ShellArgs(cmd []string) []string {
	args := []string{"shell", c.instance()}
	return append(args, cmd...)
}

// Shell enters the guest as limaadmin, interactively when cmd is empty.
func (c Client) Shell(ctx context.Context, cmd []string) error {
	return c.R.Run(ctx, c.ShellArgs(cmd)...)
}

// Agent runs cmd as the agent user in workdir.
func (c Client) Agent(ctx context.Context, workdir string, cmd []string) error {
	return c.R.Run(ctx, c.AgentArgs(workdir, cmd)...)
}

// Admin runs cmd as root in the guest.
func (c Client) Admin(ctx context.Context, cmd []string) error {
	return c.R.Run(ctx, c.AdminArgs(cmd)...)
}

// AdminOutput runs cmd as root in the guest and captures stdout.
func (c Client) AdminOutput(ctx context.Context, cmd []string) ([]byte, error) {
	return c.R.Output(ctx, c.AdminArgs(cmd)...)
}

// ReadFile returns the contents of a guest file, and empty content when it does
// not exist or cannot be read.
//
// Callers use this to probe guest state — is a fragment installed, what mode did
// the firewall record — where absence is an ordinary answer. `cat` writes "No
// such file or directory" to stderr, and AdminOutput streams stderr to the
// caller's terminal, so probing with it directly printed that line into the
// user's own command output on every invocation.
//
// The path travels as an argument rather than inside the command string, so it
// cannot be interpreted as shell syntax.
func (c Client) ReadFile(ctx context.Context, path string) []byte {
	out, err := c.AdminOutput(ctx, []string{"sh", "-c", `cat -- "$0" 2>/dev/null`, path})
	if err != nil {
		return nil
	}
	return out
}

// Status returns the instance status, or "" when the instance does not exist.
func (c Client) Status(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", c.instance(), "--format", "{{.Status}}")
	if err != nil {
		// A missing instance is not an error condition for callers.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Start creates or starts the instance from the rendered template.
func (c Client) Start(ctx context.Context, tplPath string) error {
	return c.R.RunDetached(ctx, "start", "--tty=false", "--name", c.instance(), tplPath)
}

// StartExisting boots an already-created instance from its stored config.
// `limactl start --name <name> <file>` refuses to run when the instance
// exists, so updates go through ResolveConfigInto first.
func (c Client) StartExisting(ctx context.Context) error {
	return c.R.RunDetached(ctx, "start", "--tty=false", c.instance())
}

// InstanceDir returns the instance's data directory on the host.
func (c Client) InstanceDir(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", c.instance(), "--format", "{{.Dir}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SSHConfigFile returns the path of the ssh config Lima writes for the
// instance, which authenticates as limaadmin to the instance's forwarded sshd.
func (c Client) SSHConfigFile(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", c.instance(), "--format", "{{.SSHConfigFile}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SSHHost is the host alias the instance's ssh config defines.
func (c Client) SSHHost() string {
	return "lima-" + c.instance()
}

// ResolveConfigInto resolves a rendered template into a self-contained
// instance config at dst. Instance configs must have `base` resolved into
// concrete fields (observed: a stored config with `base:` fails validation
// with "field base must be empty"), which --embed-all performs.
func (c Client) ResolveConfigInto(ctx context.Context, src, dst string) error {
	return c.R.Run(ctx, "template", "copy", "--embed-all", src, dst)
}

// Stop shuts the instance down.
func (c Client) Stop(ctx context.Context) error {
	return c.R.Run(ctx, "stop", c.instance())
}

// Delete removes the instance and its disk.
func (c Client) Delete(ctx context.Context) error {
	return c.R.Run(ctx, "delete", "--force", c.instance())
}

// Copy copies a host file into the guest.
func (c Client) Copy(ctx context.Context, localPath, guestPath string) error {
	return c.R.Run(ctx, "copy", localPath, c.instance()+":"+guestPath)
}

// Version returns limactl's reported version string.
func (c Client) Version(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "--version")
	return string(out), err
}
