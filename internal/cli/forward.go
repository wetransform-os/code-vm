package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// portForward maps a host loopback port to a guest loopback port.
type portForward struct {
	Host, Guest int
}

// parseForward accepts PORT or HOST_PORT:GUEST_PORT. A bind address is not
// part of the syntax: the host side is always 127.0.0.1, so a forward never
// exposes the guest to the network the host sits on.
func parseForward(spec string) (portForward, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 2 {
		return portForward{}, fmt.Errorf("invalid forward %q; want PORT or HOST_PORT:GUEST_PORT", spec)
	}
	ports := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return portForward{}, fmt.Errorf("invalid forward %q; ports must be numbers from 1 to 65535", spec)
		}
		ports[i] = n
	}
	if len(ports) == 1 {
		return portForward{Host: ports[0], Guest: ports[0]}, nil
	}
	return portForward{Host: ports[0], Guest: ports[1]}, nil
}

// forwardSSHArgs builds the ssh argv for the tunnel. The guest end is
// 127.0.0.1 too: sshd connects from inside the guest, which reaches servers
// bound to loopback as well as wildcard binds and Docker-published ports.
// Lima's ssh config multiplexes connections over a persistent master process
// (ControlMaster auto, ControlPersist yes). A forward requested through that
// master is owned by the master and stays open after the client exits, so
// the tunnel opens its own connection; then the listener lives in this ssh
// process and closes with it. ExitOnForwardFailure turns a host port already
// in use into an error instead of a tunnel that silently forwards nothing.
func forwardSSHArgs(sshConfig, host string, fwds []portForward) []string {
	args := []string{
		"-F", sshConfig, "-N",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ExitOnForwardFailure=yes",
	}
	for _, f := range fwds {
		args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f.Host, f.Guest))
	}
	return append(args, host)
}

// runSSH is a package variable so tests can substitute a fake. It runs ssh in
// the foreground until it exits. An interrupt or terminate signal ends the
// tunnel and counts as a normal stop. Ctrl-C reaches ssh through the
// terminal's process group, but a signal sent to code-vm alone (a script
// stopping a background forward) does not, so it is relayed explicitly.
var runSSH = func(ctx context.Context, args []string) error {
	// Notify rather than ignore: an ignored signal is inherited across exec,
	// which would leave ssh deaf to Ctrl-C.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ssh: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	select {
	case sig := <-sigs:
		_ = cmd.Process.Signal(sig)
		<-done
		return nil
	case err = <-done:
	}
	// A Ctrl-C that ssh exited on can land just after Wait returns.
	select {
	case <-sigs:
		return nil
	default:
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("ssh tunnel exited with status %d", ee.ExitCode())
	}
	return err
}

func parseForwards(specs []string) ([]portForward, error) {
	fwds := make([]portForward, 0, len(specs))
	seen := map[int]bool{}
	for _, spec := range specs {
		f, err := parseForward(spec)
		if err != nil {
			return nil, err
		}
		if seen[f.Host] {
			return nil, fmt.Errorf("host port %d is forwarded more than once", f.Host)
		}
		seen[f.Host] = true
		fwds = append(fwds, f)
	}
	return fwds, nil
}

func newForwardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forward PORT|HOST_PORT:GUEST_PORT...",
		Short: "Forward guest ports to localhost on the host until Ctrl-C",
		Long: "Forward guest ports to the host's loopback interface, so a server\n" +
			"running in the VM opens in a host browser. PORT forwards the same\n" +
			"port number; HOST_PORT:GUEST_PORT maps a different one. The forward\n" +
			"lasts until Ctrl-C and is never reachable from other machines.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fwds, err := parseForwards(args)
			if err != nil {
				return err
			}
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			cl := clientFor(c)
			status, err := cl.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status != "Running" {
				return fmt.Errorf("instance %s is not running; start it with `code-vm start` and the server inside it first", c.Instance)
			}
			sshConfig, err := cl.SSHConfigFile(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, f := range fwds {
				fmt.Fprintf(out, "http://localhost:%d -> guest port %d\n", f.Host, f.Guest)
			}
			fmt.Fprintln(out, "Press Ctrl-C to stop forwarding.")
			return runSSH(cmd.Context(), forwardSSHArgs(sshConfig, cl.SSHHost(), fwds))
		},
	}
}
