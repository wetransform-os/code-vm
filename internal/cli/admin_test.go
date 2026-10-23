package cli

import (
	"reflect"
	"strings"
	"testing"
)

// The instance comes from the config (withScratchConfig names a test one), so
// an admin shell can never land in the daily VM while a test VM is targeted.
func TestAdminOpensLimaadminShell(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"admin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("admin: %v", err)
	}
	last := r.calls[len(r.calls)-1]
	if want := []string{"shell", "code-sandbox-clitest"}; !reflect.DeepEqual(last, want) {
		t.Errorf("admin ran %v, want %v (a plain limaadmin shell, no sudo, no sandbox-exec)", last, want)
	}
}

// Everything after `--` goes to the guest verbatim, flags included, so
// `code-vm admin -- journalctl -u sandbox-boot` reads as it would locally.
func TestAdminForwardsCommandAfterDoubleDash(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"admin", "--", "journalctl", "-u", "sandbox-boot"})
	if err := root.Execute(); err != nil {
		t.Fatalf("admin -- cmd: %v", err)
	}
	last := r.calls[len(r.calls)-1]
	want := []string{"shell", "code-sandbox-clitest", "journalctl", "-u", "sandbox-boot"}
	if !reflect.DeepEqual(last, want) {
		t.Errorf("admin ran %v, want %v", last, want)
	}
}

func TestAdminRequiresRunningVM(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Stopped")
	root.SetArgs([]string{"admin"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("admin on a stopped VM = %v, want a not-running error", err)
	}
	if ranAny(r.calls, "shell") {
		t.Errorf("no shell may be opened into a stopped VM, calls=%v", r.calls)
	}
}
