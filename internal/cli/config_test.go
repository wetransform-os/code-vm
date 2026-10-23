package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
)

// guestLockScriptHash answers `sha256sum` for the embedded lock-settings.sh,
// as a guest provisioned by this binary would.
func guestLockScriptHash(t *testing.T) string {
	t.Helper()
	files, err := guest.DataFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "/usr/local/lib/sandbox/lock-settings.sh" {
			sum := sha256.Sum256([]byte(f.Content))
			return hex.EncodeToString(sum[:]) + "  " + f.Path + "\n"
		}
	}
	t.Fatal("lock-settings.sh not among the embedded data files")
	return ""
}

func TestConfigApplyPushesCanonicalTreeAndLocks(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	r.outputs = map[string]string{"sha256sum": guestLockScriptHash(t)}
	root.SetArgs([]string{"config", "apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config apply: %v", err)
	}
	for _, want := range []string{
		"install -D -m 0444 -o root -g root",
		guest.ConfigRoot + ".new-",
		"mv -T --exchange",
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

func TestConfigApplyRefusesGuestWithOlderLockScript(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	r.outputs = map[string]string{"sha256sum": "0000000000000000000000000000000000000000000000000000000000000000  /usr/local/lib/sandbox/lock-settings.sh\n"}
	root.SetArgs([]string{"config", "apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("config apply against an older guest script = %v, want an error pointing at a restart", err)
	}
	// The lock step only ever runs inside the exchange command, so its
	// absence is what proves nothing was locked; the script path itself also
	// appears in the sha256sum probe that produced the refusal.
	if ranAny(r.calls, "copy") || ranAny(r.calls, "--exchange") {
		t.Errorf("nothing may be pushed or locked against an older guest script, calls=%v", r.calls)
	}
}

// Pushing the config needs nothing from the profiles, so a profile that is
// listed but broken (or not installed yet) must not block it — the lock
// settings are the one thing that should always be deliverable.
func TestConfigApplyDoesNotDependOnProfiles(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	appendConfig(t, "profiles:\n  - not-installed\n")
	r := installFakeClient(t, "Running")
	r.outputs = map[string]string{"sha256sum": guestLockScriptHash(t)}
	root.SetArgs([]string{"config", "apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config apply with an uninstalled profile listed = %v, want success", err)
	}
	if !ranAny(r.calls, "/usr/local/lib/sandbox/lock-settings.sh") {
		t.Errorf("lock-settings.sh must have run, calls=%v", r.calls)
	}
}
