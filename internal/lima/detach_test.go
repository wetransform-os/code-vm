package lima

import (
	"context"
	"errors"
	"reflect"
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
