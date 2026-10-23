package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
)

// The live path must leave the guest in the state a boot would: the canonical
// tree replaced wholesale (a file dropped from the binary must not linger and
// get re-locked into the home) and lock-settings.sh run afterwards, which is
// what actually rewrites and locks the agent's settings.
// lockScript is a stand-in for the embedded lock-settings.sh.
var lockScript = guest.DataFile{Path: "/usr/local/lib/sandbox/lock-settings.sh", Permissions: "0755", Content: "#!/bin/bash\necho locked\n"}

// guestHashOutput mimics `sha256sum <path>` for content.
func guestHashOutput(f guest.DataFile) []byte {
	sum := sha256.Sum256([]byte(f.Content))
	return []byte(hex.EncodeToString(sum[:]) + "  " + f.Path + "\n")
}

func TestPushConfigReplacesTreeThenLocks(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(lockScript)}}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/.claude/settings.json", Permissions: "0444", Content: "{}\n"},
	}
	if err := PushConfig(context.Background(), d, files, lockScript); err != nil {
		t.Fatalf("PushConfig: %v", err)
	}

	rmIdx := findCallIndex(r.calls, "rm -rf "+guest.ConfigRoot)
	installIdx := findCallIndex(r.calls, "install -D -m 0444 -o root -g root")
	lockIdx := findCallIndex(r.calls, "sudo /usr/local/lib/sandbox/lock-settings.sh")
	if rmIdx < 0 {
		t.Error("the old tree must be removed so files dropped from the binary disappear")
	}
	if installIdx < 0 {
		t.Errorf("settings.json must be installed root-owned and read-only, calls=%v", r.calls)
	}
	if lockIdx < 0 {
		t.Error("lock-settings.sh must run so the agent's home picks up the new tree")
	}
	if rmIdx >= 0 && installIdx >= 0 && rmIdx >= installIdx {
		t.Errorf("rm (%d) must precede install (%d)", rmIdx, installIdx)
	}
	if installIdx >= 0 && lockIdx >= 0 && installIdx >= lockIdx {
		t.Errorf("install (%d) must precede lock-settings.sh (%d)", installIdx, lockIdx)
	}
	if !r.ranAny(guest.ConfigRoot + "/.claude/settings.json") {
		t.Errorf("settings.json must land under %s, calls=%v", guest.ConfigRoot, r.calls)
	}
}

func TestPushConfigRejectsPathsOutsideConfigRoot(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: "/usr/local/lib/sandbox/init-firewall.sh", Permissions: "0755", Content: "#!/bin/bash\n"},
	}
	if err := PushConfig(context.Background(), d, files, lockScript); err == nil {
		t.Fatal("PushConfig accepted a file outside the config tree")
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may touch the guest for a rejected set, calls=%v", r.calls)
	}
}

// `config apply` delivers the config tree but not the scripts, so a guest
// still booted with an older lock-settings.sh would run that older script
// against a live agent. The guest's copy must match what this binary ships,
// or the user is told to restart (which re-provisions the scripts) instead.
func TestPushConfigRefusesStaleGuestLockScript(t *testing.T) {
	stale := guest.DataFile{Path: lockScript.Path, Content: "#!/bin/bash\necho old\n"}
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(stale)}}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/.claude/settings.json", Permissions: "0444", Content: "{}\n"},
	}
	err := PushConfig(context.Background(), d, files, lockScript)
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("PushConfig with a stale guest script = %v, want an error pointing at a restart", err)
	}
	if r.ranAny("rm -rf") || r.ranAny("copy") || findCallIndex(r.calls, "sudo "+lockScript.Path) >= 0 {
		t.Errorf("nothing may be pushed or locked when the guest script is stale, calls=%v", r.calls)
	}
}
