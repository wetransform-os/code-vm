package session

import (
	"context"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
)

// The live path must leave the guest in the state a boot would: the canonical
// tree replaced wholesale (a file dropped from the binary must not linger and
// get re-locked into the home) and lock-settings.sh run afterwards, which is
// what actually rewrites and locks the agent's settings.
func TestPushConfigReplacesTreeThenLocks(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/.claude/settings.json", Permissions: "0444", Content: "{}\n"},
	}
	if err := PushConfig(context.Background(), d, files); err != nil {
		t.Fatalf("PushConfig: %v", err)
	}

	rmIdx := findCallIndex(r.calls, "rm -rf "+guest.ConfigRoot)
	installIdx := findCallIndex(r.calls, "install -D -m 0444 -o root -g root")
	lockIdx := findCallIndex(r.calls, "/usr/local/lib/sandbox/lock-settings.sh")
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
	if err := PushConfig(context.Background(), d, files); err == nil {
		t.Fatal("PushConfig accepted a file outside the config tree")
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may touch the guest for a rejected set, calls=%v", r.calls)
	}
}
