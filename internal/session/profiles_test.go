package session

import (
	"context"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

// findCallIndex locates the index in calls of the first call containing substr.
// Returns -1 if not found.
func findCallIndex(calls [][]string, substr string) int {
	for i, c := range calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return i
		}
	}
	return -1
}

func TestPushProfilesReplacesTheGuestTree(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: profile.GuestRoot + "/manifest.env", Permissions: "0444", Content: "PROFILES=\"\"\n"},
		{Path: profile.GuestRoot + "/p/files/.claude/CLAUDE.md", Permissions: "0444", Content: "# rules\n"},
		{Path: profile.GuestRoot + "/p/hook", Permissions: "0555", Content: "#!/bin/bash\n"},
	}
	if err := PushProfiles(context.Background(), d, files); err != nil {
		t.Fatalf("PushProfiles: %v", err)
	}

	// Verify the semantic guarantee: remove old tree BEFORE recreating and
	// installing. A reordering to recreate-then-remove would silently break
	// 'deactivated profiles disappear', so ordering is critical.
	rmIdx := findCallIndex(r.calls, "rm -rf "+profile.GuestRoot)
	mkdirIdx := findCallIndex(r.calls, "install -d -m 0755 "+profile.GuestRoot)
	copyIdx := findCallIndex(r.calls, "copy")

	if rmIdx < 0 {
		t.Error("the old tree must be removed so deactivated profiles disappear")
	}
	if mkdirIdx < 0 {
		t.Error("the tree root must be recreated")
	}
	if copyIdx < 0 {
		t.Error("files must be staged and installed")
	}
	if rmIdx >= 0 && mkdirIdx >= 0 && rmIdx >= mkdirIdx {
		t.Errorf("rm must happen before mkdir: rm at index %d, mkdir at index %d", rmIdx, mkdirIdx)
	}
	if mkdirIdx >= 0 && copyIdx >= 0 && mkdirIdx >= copyIdx {
		t.Errorf("mkdir must happen before file install: mkdir at index %d, copy at index %d", mkdirIdx, copyIdx)
	}

	// installContent stages via `limactl copy` then root-installs; -D creates
	// the nested per-profile parents.
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != len(files) {
		t.Errorf("staged %d files, want %d", copies, len(files))
	}
	if !r.ranAny("install -D -m 0444 -o root -g root") {
		t.Errorf("files must be installed root-owned with their permissions, got %v", r.calls)
	}
	if !r.ranAny("install -D -m 0555 -o root -g root") {
		t.Error("the hook must be installed with the executable permission set")
	}
}

// applier is a stand-in for the embedded apply-profiles.sh.
var applier = guest.DataFile{Path: ApplierScript, Permissions: "0755", Content: "#!/bin/bash\necho apply\n"}

func TestApplyProfilesRunsStrict(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(applier)}}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d, applier); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	if !r.ranAny("env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh") {
		t.Errorf("the applier must run in strict mode on the apply path, got %v", r.calls)
	}
}

// Steady state is "same build, same VM": when the guest's applier already
// matches, nothing is staged or copied — one hash round-trip, then the run.
func TestApplyProfilesSkipsReinstallWhenApplierMatches(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(applier)}}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d, applier); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	if r.ranAny("copy") || r.ranAny("mv -T") {
		t.Errorf("a matching applier must not be reinstalled, got %v", r.calls)
	}
}

// A VM booted from an older build keeps the applier that boot delivered;
// apply must bring this build's applier in before running it — otherwise a
// new applier step never runs until the next restart even though the pushed
// tree already carries its inputs. The replacement goes live by a rename
// over the canonical path so a concurrent exec never sees it missing.
func TestApplyProfilesReinstallsAnOlderApplierBeforeRunning(t *testing.T) {
	stale := guest.DataFile{Path: ApplierScript, Permissions: "0755", Content: "#!/bin/bash\necho old\n"}
	r := &fakeRunner{out: map[string][]byte{"sha256sum": guestHashOutput(stale)}}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d, applier); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	copyIdx := findCallIndex(r.calls, "copy")
	swapIdx := findCallIndex(r.calls, scriptSwapScript)
	runIdx := findCallIndex(r.calls, "env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh")
	if copyIdx < 0 || swapIdx < 0 {
		t.Fatalf("the applier must be staged and swapped in by the guest-side script, got %v", r.calls)
	}
	// The swap script's positional args: live path, mode, staged copy,
	// sibling — the sibling, not the live path, is what install writes.
	swap := r.calls[swapIdx]
	n := len(swap)
	if swap[n-4] != ApplierScript || swap[n-3] != "0755" || !strings.HasPrefix(swap[n-1], ApplierScript+".new-") {
		t.Errorf("swap args must be (live, mode, staged, sibling), got %v", swap[n-4:])
	}
	if copyIdx >= swapIdx || swapIdx >= runIdx {
		t.Errorf("order must be stage (%d) < swap (%d) < run (%d)", copyIdx, swapIdx, runIdx)
	}
}

// A guest with no applier at all (or an unreadable one) must read as a
// mismatch and be reinstalled, not fail the apply on sha256sum's exit code.
func TestApplyProfilesReinstallsAMissingApplier(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{"sha256sum": []byte("missing\n")}}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d, applier); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	if !r.ranAny(scriptSwapScript) {
		t.Errorf("a missing applier must be reinstalled, got %v", r.calls)
	}
	// The probe itself must not let sha256sum's exit status escape.
	probe := r.calls[findCallIndex(r.calls, "sha256sum")]
	if !strings.Contains(strings.Join(probe, " "), `test -f "$1" && exec sha256sum`) {
		t.Errorf("the hash probe must guard the file's existence, got %v", probe)
	}
}

func TestRequireBootCompleteRefusesWhileBooting(t *testing.T) {
	r := &fakeRunner{failOn: "test -f " + bootCompleteMarker}
	d := testDeps(t, r)
	err := RequireBootComplete(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "not finished booting") {
		t.Fatalf("expected a boot-in-progress refusal, got %v", err)
	}
}

// Only this binary's apply-profiles.sh may be run as the applier; a caller
// passing some other guest file must be refused before the guest is touched.
func TestApplyProfilesRefusesOtherScripts(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	err := ApplyProfiles(context.Background(), d, lockScript)
	if err == nil || len(r.calls) != 0 {
		t.Fatalf("expected a refusal with no guest calls, got err=%v calls=%v", err, r.calls)
	}
}
