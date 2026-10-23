package cli

import (
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
)

func TestConfigApplyPushesCanonicalTreeAndLocks(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"config", "apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config apply: %v", err)
	}
	for _, want := range []string{
		"rm -rf " + guest.ConfigRoot,
		"install -D -m 0444 -o root -g root",
		guest.ConfigRoot + "/.claude/settings.json",
		"/usr/local/lib/sandbox/lock-settings.sh",
	} {
		if !ranAny(r.calls, want) {
			t.Errorf("missing guest command %q in %v", want, r.calls)
		}
	}
	// Only the config tree travels: no boot script or unit may be re-pushed
	// into a live system by this command.
	for _, unwanted := range []string{"/usr/local/lib/sandbox/init-firewall.sh", "/etc/systemd/system/"} {
		if ranAny(r.calls, unwanted) {
			t.Errorf("%q must not be delivered by config apply, calls=%v", unwanted, r.calls)
		}
	}
}

func TestConfigApplyRequiresRunningVM(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Stopped")
	root.SetArgs([]string{"config", "apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("config apply on a stopped VM = %v, want a not-running error", err)
	}
	if ranAny(r.calls, "rm -rf") || ranAny(r.calls, "lock-settings.sh") {
		t.Errorf("nothing may be pushed to a stopped VM, calls=%v", r.calls)
	}
}
