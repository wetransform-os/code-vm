package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
)

// lockScript is a stand-in for the embedded lock-settings.sh.
var lockScript = guest.DataFile{Path: "/usr/local/lib/sandbox/lock-settings.sh", Permissions: "0755", Content: "#!/bin/bash\necho locked\n"}

// guestHashOutput mimics `sha256sum <path>` for content.
func guestHashOutput(f guest.DataFile) []byte {
	sum := sha256.Sum256([]byte(f.Content))
	return []byte(hex.EncodeToString(sum[:]) + "  " + f.Path + "\n")
}

// removedLiveTree reports whether any call was `rm -rf <ConfigRoot>` itself
// (as opposed to a staging sibling such as ConfigRoot.new).
func removedLiveTree(calls [][]string) bool {
	for _, c := range calls {
		n := len(c)
		if n >= 3 && c[n-3] == "rm" && c[n-2] == "-rf" && c[n-1] == guest.ConfigRoot {
			return true
		}
	}
	return false
}

// The live path must leave the guest in the state a boot would: the canonical
// tree replaced wholesale (a file dropped from the binary must not linger and
// get re-locked into the home) and lock-settings.sh run afterwards, which is
// what actually rewrites and locks the agent's settings. The replacement is
// built beside the live tree and swapped in only once every file is there:
// a failed or interrupted push must leave the last good tree in place, not
// an empty directory the lock step would then faithfully apply.
func TestPushConfigBuildsTreeBesideLiveOneThenSwapsAndLocks(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(lockScript)}}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/.claude/settings.json", Permissions: "0444", Content: "{}\n"},
	}
	if err := PushConfig(context.Background(), d, files, lockScript); err != nil {
		t.Fatalf("PushConfig: %v", err)
	}

	if removedLiveTree(r.calls) {
		t.Errorf("the live tree must never be removed outright, calls=%v", r.calls)
	}
	installIdx := findCallIndex(r.calls, "install -D -m 0444 -o root -g root")
	if installIdx < 0 {
		t.Fatalf("settings.json must be installed root-owned and read-only, calls=%v", r.calls)
	}
	installDst := r.calls[installIdx][len(r.calls[installIdx])-1]
	stageRe := regexp.MustCompile("^" + regexp.QuoteMeta(guest.ConfigRoot) + `\.new-[0-9a-f]+/\.claude/settings\.json$`)
	if !stageRe.MatchString(installDst) {
		t.Errorf("files must be installed into a per-invocation staging tree, got %s", installDst)
	}
	// The swap and the lock step travel in one guest command, under an
	// exclusive lock so two applies cannot interleave, and the swap is an
	// exchange: the canonical path is never absent, not even between two
	// renames.
	swapIdx := findCallIndex(r.calls, "mv -T --exchange")
	if swapIdx < 0 {
		t.Fatalf("the staging tree must be exchanged into place, calls=%v", r.calls)
	}
	swap := strings.Join(r.calls[swapIdx], " ")
	if !strings.Contains(swap, "flock ") {
		t.Errorf("the swap must run under an exclusive lock: %s", swap)
	}
	if !strings.Contains(swap, lockScript.Path) {
		t.Errorf("the lock step must run in the same guarded command as the swap: %s", swap)
	}
	if strings.Index(swap, "--exchange") > strings.Index(swap, lockScript.Path) {
		t.Errorf("the exchange must precede the lock step: %s", swap)
	}
	if installIdx >= swapIdx {
		t.Errorf("install (%d) must precede the swap (%d)", installIdx, swapIdx)
	}
	if removedLiveTree(r.calls) {
		t.Errorf("the live tree must never be removed outright, calls=%v", r.calls)
	}
}

func TestPushConfigLeavesLiveTreeAloneWhenAnUploadFails(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(lockScript)}, failOn: "copy"}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/.claude/settings.json", Permissions: "0444", Content: "{}\n"},
	}
	if err := PushConfig(context.Background(), d, files, lockScript); err == nil {
		t.Fatal("PushConfig succeeded although the upload failed")
	}
	if removedLiveTree(r.calls) || r.ranAny("--exchange") {
		t.Errorf("the live tree must be untouched after a failed upload, calls=%v", r.calls)
	}
	if r.ranAny(lockScript.Path+" ") || strings.HasSuffix(strings.Join(r.calls[len(r.calls)-1], " "), lockScript.Path) {
		t.Errorf("lock-settings.sh must not run after a failed upload, calls=%v", r.calls)
	}
	// The abandoned staging tree is removed, so a failed apply leaves no
	// stray root-owned directory beside the canonical one.
	last := strings.Join(r.calls[len(r.calls)-1], " ")
	if !regexp.MustCompile(`rm -rf ` + regexp.QuoteMeta(guest.ConfigRoot) + `\.new-[0-9a-f]+$`).MatchString(last) {
		t.Errorf("the staging tree must be cleaned up after a failed upload, last call = %s", last)
	}
}

// The paths come from this binary's own embedded tree, but the guard that
// keeps the push inside the config tree must not be fooled by a prefix
// alone: a traversal component would escape the staging tree on the way
// to the live one.
func TestPushConfigRejectsTraversalInsideThePrefix(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(lockScript)}}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: guest.ConfigRoot + "/../sandbox-profiles/x", Permissions: "0444", Content: "x\n"},
	}
	if err := PushConfig(context.Background(), d, files, lockScript); err == nil {
		t.Fatal("PushConfig accepted a path that leaves the config tree through ..")
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may touch the guest for a rejected set, calls=%v", r.calls)
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
