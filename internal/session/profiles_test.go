package session

import (
	"context"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

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
	if !r.ranAny("rm -rf " + profile.GuestRoot) {
		t.Error("the old tree must be removed so deactivated profiles disappear")
	}
	if !r.ranAny("install -d -m 0755 " + profile.GuestRoot) {
		t.Error("the tree root must be recreated")
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
