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

func TestApplyProfilesRunsStrict(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	if !r.ranAny("env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh") {
		t.Errorf("the applier must run in strict mode on the apply path, got %v", r.calls)
	}
}
