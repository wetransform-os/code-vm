package lima

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDetachPrefixOnlyOnLinuxWithUserManager(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	yes := func(context.Context) bool { return true }
	no := func(context.Context) bool { return false }
	ctx := context.Background()
	if got := DetachPrefix(ctx, "darwin", found, yes, "code-sandbox"); got != nil {
		t.Errorf("darwin: got %v, want nil", got)
	}
	if got := DetachPrefix(ctx, "linux", missing, yes, "code-sandbox"); got != nil {
		t.Errorf("no systemd-run: got %v, want nil", got)
	}
	if got := DetachPrefix(ctx, "linux", found, no, "code-sandbox"); got != nil {
		t.Errorf("probe failed: got %v, want nil", got)
	}
	want := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--description=code-vm VM boot (code-sandbox)", "--"}
	if got := DetachPrefix(ctx, "linux", found, yes, "code-sandbox"); !reflect.DeepEqual(got, want) {
		t.Errorf("linux with user manager: got %q, want %q", got, want)
	}
}

func TestDetachedArgvWrapsLimactl(t *testing.T) {
	bin, args := detachedArgv([]string{"systemd-run", "--user", "--"}, "limactl", []string{"start", "--tty=false", "code-sandbox"})
	if bin != "systemd-run" {
		t.Errorf("bin = %q, want systemd-run", bin)
	}
	want := []string{"--user", "--", "limactl", "start", "--tty=false", "code-sandbox"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// A probe that lies is the only way to exercise the exec path without
// systemd: "env --" is a wrapper with the same shape as systemd-run's prefix.
func TestExecRunnerRunDetachedUsesThePrefix(t *testing.T) {
	r := ExecRunner{Bin: "true", Detach: func(context.Context) []string { return []string{"env", "--"} }}
	if err := r.RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached via env: %v", err)
	}
	plain := ExecRunner{Bin: "true", Detach: func(context.Context) []string { return nil }}
	if err := plain.RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached without prefix: %v", err)
	}
	if err := (ExecRunner{Bin: "true"}).RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached with nil Detach: %v", err)
	}
}

func TestDetachForNamesTheConfiguredInstance(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	yes := func(context.Context) bool { return true }
	got := detachFor("code-sandbox-test", "linux", found, yes)(context.Background())
	var desc string
	for _, a := range got {
		if strings.HasPrefix(a, "--description=") {
			desc = a
		}
	}
	if desc != "--description=code-vm VM boot (code-sandbox-test)" {
		t.Errorf("prefix %q must carry the configured instance", got)
	}
}

// Orca ends a pane by SIGKILLing the process groups of everything that shares
// the pane's session and controlling terminal, whatever cgroup it lives in. A
// scope does not help against that, so the boot must also leave the session.
func TestRunDetachedStartsItsOwnSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads the session id from /proc")
	}
	probe := `echo $$; cut -d' ' -f6 /proc/$$/stat`
	cases := map[string]func(context.Context) []string{
		"without a scope prefix": nil,
		"with a scope prefix":    func(context.Context) []string { return []string{"env", "--"} },
	}
	for name, detach := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			r := ExecRunner{Bin: "sh", Stdout: &out, Detach: detach}
			if err := r.RunDetached(context.Background(), "-c", probe); err != nil {
				t.Fatal(err)
			}
			var pid, sid int
			if _, err := fmt.Sscan(out.String(), &pid, &sid); err != nil {
				t.Fatalf("parsing %q: %v", out.String(), err)
			}
			if own := ownSessionID(t); sid == own {
				t.Errorf("boot stayed in the caller's session %d", own)
			}
			if sid != pid {
				t.Errorf("boot is in session %d, want it to lead its own (pid %d)", sid, pid)
			}
		})
	}
}

// ownSessionID reads the test process's session id: field 6 of
// /proc/self/stat, counted after the parenthesised command name.
func ownSessionID(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	rest := string(b[bytes.LastIndexByte(b, ')')+1:])
	var state string
	var ppid, pgrp, sid int
	if _, err := fmt.Sscan(rest, &state, &ppid, &pgrp, &sid); err != nil {
		t.Fatalf("parsing %q: %v", rest, err)
	}
	return sid
}
