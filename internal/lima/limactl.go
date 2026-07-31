package lima

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner executes limactl. It is an interface so command construction can be
// tested without a VM.
type Runner interface {
	Run(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the real limactl binary.
type ExecRunner struct {
	Bin    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
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

// Client drives the sandbox Lima instance.
type Client struct {
	R Runner
}

// NewClient returns a Client wired to the real limactl and this process's
// standard streams.
func NewClient() Client {
	return Client{R: ExecRunner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}}
}

// AgentArgs builds the argv that runs cmd as the agent user in workdir.
// sandbox-exec sources /etc/environment, drops from root to the agent user
// and only then changes into workdir. `limactl shell --workdir` is not used:
// it would cd as limaadmin before sudo, which fails for 0700 directories the
// agent owns (observed with mktemp -d workspaces).
func (c Client) AgentArgs(workdir string, cmd []string) []string {
	args := []string{"shell", InstanceName, "sudo", "/usr/local/bin/sandbox-exec", "--workdir", workdir}
	return append(args, cmd...)
}

// AdminArgs builds the argv that runs cmd as root via limaadmin's sudo.
func (c Client) AdminArgs(cmd []string) []string {
	args := []string{"shell", InstanceName, "sudo"}
	return append(args, cmd...)
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

// Status returns the instance status, or "" when the instance does not exist.
func (c Client) Status(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", InstanceName, "--format", "{{.Status}}")
	if err != nil {
		// A missing instance is not an error condition for callers.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Start creates or starts the instance from the rendered template.
func (c Client) Start(ctx context.Context, tplPath string) error {
	return c.R.Run(ctx, "start", "--tty=false", "--name", InstanceName, tplPath)
}

// StartExisting boots an already-created instance from its stored config.
// `limactl start --name <name> <file>` refuses to run when the instance
// exists, so updates go through ResolveConfigInto first.
func (c Client) StartExisting(ctx context.Context) error {
	return c.R.Run(ctx, "start", "--tty=false", InstanceName)
}

// InstanceDir returns the instance's data directory on the host.
func (c Client) InstanceDir(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", InstanceName, "--format", "{{.Dir}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
	return c.R.Run(ctx, "stop", InstanceName)
}

// Delete removes the instance and its disk.
func (c Client) Delete(ctx context.Context) error {
	return c.R.Run(ctx, "delete", "--force", InstanceName)
}

// Copy copies a host file into the guest.
func (c Client) Copy(ctx context.Context, localPath, guestPath string) error {
	return c.R.Run(ctx, "copy", localPath, InstanceName+":"+guestPath)
}

// Version returns limactl's reported version string.
func (c Client) Version(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "--version")
	return string(out), err
}
