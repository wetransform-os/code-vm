package profile

import (
	"reflect"
	"strings"
	"testing"
)

func fixtureProfiles() []Profile {
	return []Profile{
		{
			Name: "fish-shell",
			Manifest: Manifest{
				Packages: []string{"fish", "git"},
				Shell:    "/usr/bin/fish",
				Domains:  []string{"raw.githubusercontent.com"},
				Hook:     "hook.sh",
			},
			Files: []File{
				{Rel: ".config/fish/config.fish", Content: []byte("set -g fish_greeting\n")},
				{Rel: "bin/tool", Content: []byte("#!/bin/sh\n"), Executable: true},
			},
			Hook: []byte("#!/bin/bash\nfisher install x\n"),
		},
		{
			Name:     "wetf-claude",
			Manifest: Manifest{Packages: []string{"git"}},
			Files:    []File{{Rel: ".claude/CLAUDE.md", Content: []byte("# rules\n")}},
		},
	}
}

func TestGuestFilesLayout(t *testing.T) {
	files := GuestFiles(fixtureProfiles())
	byPath := map[string]string{}
	perms := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Content
		perms[f.Path] = f.Permissions
	}

	manifest := byPath[ManifestPath]
	for _, want := range []string{
		`PROFILES="fish-shell wetf-claude"`,
		`PROFILE_PACKAGES="fish git"`, // union, order preserved, deduped
		`PROFILE_SHELL="/usr/bin/fish"`,
		`PROFILE_HOOKS="fish-shell"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest.env missing %q, got:\n%s", want, manifest)
		}
	}

	if byPath[GuestRoot+"/fish-shell/files/.config/fish/config.fish"] == "" {
		t.Error("profile file not rendered at its guest path")
	}
	if perms[GuestRoot+"/fish-shell/files/bin/tool"] != "0555" {
		t.Error("executable files must be delivered 0555")
	}
	if perms[GuestRoot+"/fish-shell/files/.config/fish/config.fish"] != "0444" {
		t.Error("regular files must be delivered 0444")
	}
	if byPath[GuestRoot+"/fish-shell/files.list"] != ".config/fish/config.fish\nbin/tool\n" {
		t.Errorf("files.list = %q", byPath[GuestRoot+"/fish-shell/files.list"])
	}
	if perms[GuestRoot+"/fish-shell/hook"] != "0555" {
		t.Error("hook must be delivered 0555 under the normalized name")
	}
	if _, ok := byPath[GuestRoot+"/wetf-claude/hook"]; ok {
		t.Error("a profile without a hook must not render one")
	}
}

// A profile with zero files must still emit an (empty) files.list. Boot-path
// delivery (mode:data) cannot delete a stale non-empty files.list from a
// previous version, so a profile version that drops its last file needs an
// empty files.list to make the applier stop reinstalling it.
func TestGuestFilesEmitsEmptyFilesListForFilesLessProfile(t *testing.T) {
	profiles := []Profile{{
		Name:     "no-files",
		Manifest: Manifest{Packages: []string{"git"}},
	}}
	files := GuestFiles(profiles)
	byPath := map[string]string{}
	perms := map[string]string{}
	found := false
	for _, f := range files {
		byPath[f.Path] = f.Content
		perms[f.Path] = f.Permissions
		if f.Path == GuestRoot+"/no-files/files.list" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %s/no-files/files.list to be rendered even with zero files, got %+v", GuestRoot, files)
	}
	if byPath[GuestRoot+"/no-files/files.list"] != "" {
		t.Errorf("files.list content = %q, want empty", byPath[GuestRoot+"/no-files/files.list"])
	}
	if perms[GuestRoot+"/no-files/files.list"] != "0444" {
		t.Errorf("files.list permissions = %q, want 0444", perms[GuestRoot+"/no-files/files.list"])
	}
}

func TestGuestFilesAlwaysIncludesManifest(t *testing.T) {
	files := GuestFiles(nil)
	if len(files) != 1 || files[0].Path != ManifestPath {
		t.Fatalf("zero profiles must still render exactly manifest.env, got %+v", files)
	}
	for _, want := range []string{`PROFILES=""`, `PROFILE_PACKAGES=""`, `PROFILE_SHELL=""`, `PROFILE_HOOKS=""`} {
		if !strings.Contains(files[0].Content, want) {
			t.Errorf("empty manifest.env missing %q", want)
		}
	}
}

func TestAllowDomains(t *testing.T) {
	got := AllowDomains(
		[]string{"registry.example.com", "raw.githubusercontent.com"},
		fixtureProfiles(),
	)
	// extra first, then profile domains, duplicates dropped.
	want := []string{"registry.example.com", "raw.githubusercontent.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllowDomains = %v, want %v", got, want)
	}
	if got := AllowDomains(nil, nil); len(got) != 0 {
		t.Errorf("no inputs must yield no domains, got %v", got)
	}
}
