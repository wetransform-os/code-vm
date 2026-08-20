package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfile lays out a profile directory under dir. files maps a path
// relative to the profile dir (e.g. "files/.claude/CLAUDE.md") to content.
func writeProfile(t *testing.T, dir, name, manifest string, files map[string]string) {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(root, "profile.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadValidProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "fish-shell", `
description: fish everywhere
packages: [fish]
shell: /usr/bin/fish
domains: [raw.githubusercontent.com]
hook: hook.sh
`, map[string]string{
		"files/.config/fish/config.fish": "set -g fish_greeting\n",
		"files/.claude/CLAUDE.md":        "# rules\n",
		"hook.sh":                        "#!/bin/bash\necho hi\n",
	})

	p, err := Load(dir, "fish-shell")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Manifest.Shell != "/usr/bin/fish" {
		t.Errorf("Shell = %q", p.Manifest.Shell)
	}
	if len(p.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(p.Files))
	}
	// Sorted by Rel, forward-slash relative paths.
	if p.Files[0].Rel != ".claude/CLAUDE.md" || p.Files[1].Rel != ".config/fish/config.fish" {
		t.Errorf("Files sorted wrong: %q, %q", p.Files[0].Rel, p.Files[1].Rel)
	}
	if p.Hook == nil || !strings.Contains(string(p.Hook), "echo hi") {
		t.Error("hook content not loaded")
	}
}

func TestLoadExecutableBit(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{"files/bin/tool": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(dir, "p", "files", "bin", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !p.Files[0].Executable {
		t.Error("executable bit not detected")
	}
}

// A file cloned from git (or copied by some other tool) can end up
// executable only for group or other, without the owner bit set. Detection
// must check &0o111 (any of owner/group/other), not just the owner bit.
func TestLoadExecutableBitGroupOrOtherOnly(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{
		"files/group-exec": "x\n",
		"files/not-exec":   "x\n",
	})
	// rwxr-x--- : owner has no exec bit, only group does.
	if err := os.Chmod(filepath.Join(dir, "p", "files", "group-exec"), 0o650); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "p", "files", "not-exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	byRel := map[string]File{}
	for _, f := range p.Files {
		byRel[f.Rel] = f
	}
	if !byRel["group-exec"].Executable {
		t.Error("group-only exec bit (0650) not detected as executable")
	}
	if byRel["not-exec"].Executable {
		t.Error("0644 must not be detected as executable")
	}
}

func TestLoadRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		files    map[string]string
		wantErr  string
	}{
		{"bad name is rejected before disk IO", "", nil, "profile name"},
		{"missing manifest", "", map[string]string{"files/a": "x"}, "read manifest"},
		{"unknown manifest key", "descriptoin: typo\n", nil, "field descriptoin not found"},
		{"bad package name", "packages: ['fish; rm -rf /']\n", nil, "not a Debian package name"},
		{"relative shell", "shell: usr/bin/fish\n", nil, "shell must be an absolute path"},
		{"shell with spaces", "shell: '/usr/bin/fi sh'\n", nil, "shell must be an absolute path"},
		{"bad domain", "domains: ['evil.com whitespace']\n", nil, "not a valid domain"},
		{"hook with path separator", "hook: ../outside.sh\n", nil, "hook must be a plain file name"},
		{"missing hook file", "hook: hook.sh\n", nil, "read hook"},
		{"empty profile", "description: only words\n", nil, "declares nothing"},
		{"settings.json smuggling", "description: x\n",
			map[string]string{"files/.claude/settings.json": "{}"}, "locked Claude settings"},
		{"settings.local.json smuggling", "description: x\n",
			map[string]string{"files/.claude/settings.local.json": "{}"}, "locked Claude settings"},
		{"file path with spaces", "description: x\n",
			map[string]string{"files/has space": "x"}, "file path"},
		{"files is a regular file, not a directory", "description: x\n", nil, "files must be a real directory"},
		{"empty file content", "description: x\n",
			map[string]string{"files/blank": ""}, "must not be blank"},
		{"newline-only file content", "description: x\n",
			map[string]string{"files/blank": "\n"}, "must not be blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "p"
			switch tt.name {
			case "bad name is rejected before disk IO":
				name = "no/slashes"
			case "files is a regular file, not a directory":
				writeProfile(t, dir, name, tt.manifest, nil)
				if err := os.WriteFile(filepath.Join(dir, name, "files"), []byte("not a dir"), 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				writeProfile(t, dir, name, tt.manifest, tt.files)
			}
			_, err := Load(dir, name)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsSymlinkInFiles(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{"files/real": "x"})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "p", "files", "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "regular files") {
		t.Errorf("Load error = %v, want symlink rejection", err)
	}
}

// A symlinked profile directory could point anywhere on the host, including
// back into an agent-writable mounted workspace: an agent could then rewrite
// the "profile" a later host invocation trusts.
func TestLoadRejectsSymlinkProfileDir(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	writeProfile(t, dir, "real", "description: x\npackages: [git]\n", nil)
	if err := os.Symlink(real, filepath.Join(dir, "p")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "symlinks are rejected") {
		t.Errorf("Load error = %v, want symlinked profile dir rejection", err)
	}
}

// A symlinked profile.yaml could be swapped out between install-time
// validation and a later load, feeding agent-authored manifest content
// (domains, packages, shell) back into the trusted host path.
func TestLoadRejectsSymlinkManifest(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(target, []byte("description: x\npackages: [git]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "p", "profile.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "symlinks are rejected") {
		t.Errorf("Load error = %v, want symlinked manifest rejection", err)
	}
}

// A symlinked files/ root would let WalkDir walk wherever it points,
// including a directory an agent controls.
func TestLoadRejectsSymlinkFilesRoot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "p", "description: x\n", nil)
	if err := os.Symlink(target, filepath.Join(dir, "p", "files")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "symlinks are rejected") {
		t.Errorf("Load error = %v, want symlinked files root rejection", err)
	}
}

// A hostile bundle could symlink its manifest-named hook at a file the
// installing user can read but the profile author cannot see, which
// os.ReadFile would then follow and deliver world-readable into the guest.
func TestLoadRejectsSymlinkHook(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "p", "description: x\nhook: hook.sh\n", nil)
	if err := os.Symlink(target, filepath.Join(dir, "p", "hook.sh")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("Load error = %v, want symlink hook rejection", err)
	}
}

// A hook that is empty or contains only blank lines renders a YAML literal
// block scalar with no non-blank line at all. That is valid per the YAML
// spec and parses fine with gopkg.in/yaml.v3, but was confirmed (against
// limactl 2.2.0's `limactl validate`) to fail outright regardless of
// chomping indicator — "could not find multi-line content" / "mapping value
// is not allowed in this context". Rather than ship a profile that boots
// the guest with a broken instance file, this is rejected at load time.
func TestLoadRejectsBlankHookContent(t *testing.T) {
	for _, content := range []string{"", "\n", "\n\n"} {
		t.Run(fmt.Sprintf("%q", content), func(t *testing.T) {
			dir := t.TempDir()
			writeProfile(t, dir, "p", "description: x\nhook: hook.sh\n", map[string]string{"hook.sh": content})
			if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "must not be blank") {
				t.Errorf("Load error = %v, want blank-hook rejection", err)
			}
		})
	}
}

func TestLoadAllPreservesOrderAndRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a", "description: a\npackages: [git]\n", nil)
	writeProfile(t, dir, "b", "description: b\npackages: [jq]\n", nil)

	ps, err := LoadAll(dir, []string{"b", "a"})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(ps) != 2 || ps[0].Name != "b" || ps[1].Name != "a" {
		t.Errorf("order not preserved: %+v", ps)
	}

	if _, err := LoadAll(dir, []string{"a", "a"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate names must be rejected, got %v", err)
	}
	if _, err := LoadAll(dir, []string{"missing"}); err == nil {
		t.Error("a missing profile must be an error")
	}
	if ps, err := LoadAll(dir, nil); err != nil || len(ps) != 0 {
		t.Errorf("no names must load cleanly to an empty slice, got %v, %v", ps, err)
	}
}
