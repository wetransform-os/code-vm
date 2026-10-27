package lima

import (
	"context"
	"os/exec"
	"time"
)

// Lima's host agent and QEMU are children of the `limactl start` that spawned
// them and stay in the caller's cgroup. A terminal or Orca pane that systemd
// stops by control group (KillMode=control-group) would then kill the VM with
// it, so on Linux the boot runs inside its own transient scope instead.
// A scope does not cover terminals that kill by session or process group
// (Orca's pane teardown sweeps every process on the pane's tty); ExecRunner
// also starts the boot in a session of its own for that.

// DetachPrefix returns the argv prefix that runs a command in its own
// transient user scope, or nil when the host cannot do that (not Linux, no
// systemd-run, or no reachable user manager) and the command must run as is.
func DetachPrefix(ctx context.Context, goos string, lookPath func(string) (string, error), probe func(context.Context) bool, instance string) []string {
	if goos != "linux" {
		return nil
	}
	if _, err := lookPath("systemd-run"); err != nil {
		return nil
	}
	if !probe(ctx) {
		return nil
	}
	return []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--description=code-vm VM boot (" + instance + ")", "--"}
}

// detachFor binds DetachPrefix's host probes and the instance name into the
// closure ExecRunner calls at boot time.
func detachFor(instance, goos string, lookPath func(string) (string, error), probe func(context.Context) bool) func(context.Context) []string {
	return func(ctx context.Context) []string {
		return DetachPrefix(ctx, goos, lookPath, probe, instance)
	}
}

// ProbeUserManager reports whether systemd-run can start a user scope here.
// systemd-run being installed is not enough: over a bare ssh login or in a
// container there may be no user manager to talk to.
func ProbeUserManager(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "--quiet", "--collect", "--", "true").Run() == nil
}

// detachedArgv prepends the scope wrapper to a command. It builds fresh
// slices so the caller's prefix and args are never aliased or mutated.
func detachedArgv(prefix []string, bin string, args []string) (string, []string) {
	out := make([]string, 0, len(prefix)-1+1+len(args))
	out = append(out, prefix[1:]...)
	out = append(out, bin)
	out = append(out, args...)
	return prefix[0], out
}
