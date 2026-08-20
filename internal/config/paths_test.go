package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty stays empty", "", "", false},
		{"tilde alone", "~", home, false},
		{"tilde slash", "~/projects", filepath.Join(home, "projects"), false},
		{"absolute cleaned", "/home/x/../x/projects/", "/home/x/projects", false},
		{"relative rejected", "projects", "", true},
		{"tilde user rejected", "~other/projects", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ExpandPath(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandPath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCoveringMount(t *testing.T) {
	mounts := []string{"/home/st/projects", "/home/st/work/other"}
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"exact match", "/home/st/projects", "/home/st/projects", true},
		{"nested subpath", "/home/st/projects/repo/sub", "/home/st/projects", true},
		{"sibling prefix not covered", "/home/st/projects2/repo", "", false},
		{"parent not covered", "/home/st", "", false},
		{"unrelated", "/etc", "", false},
		{"second mount", "/home/st/work/other/x", "/home/st/work/other", true},
		{"trailing slash normalised", "/home/st/projects/repo/", "/home/st/projects", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CoveringMount(mounts, tc.path)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("CoveringMount(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCoveringMountPrefersLongestMatch(t *testing.T) {
	mounts := []string{"/home/st", "/home/st/projects"}
	got, ok := CoveringMount(mounts, "/home/st/projects/repo")
	if !ok || got != "/home/st/projects" {
		t.Errorf("got (%q, %v), want (\"/home/st/projects\", true)", got, ok)
	}
}

func TestProfilesDirFor(t *testing.T) {
	got := ProfilesDirFor("/home/st/.config/code-vm/config.yaml")
	if got != "/home/st/.config/code-vm/profiles" {
		t.Errorf("ProfilesDirFor = %q", got)
	}
}

func TestCanonicalizeExisting(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}

	realTarget := filepath.Join(real, "real")
	if err := os.MkdirAll(realTarget, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(real, "alias")
	if err := os.Symlink(realTarget, alias); err != nil {
		t.Skipf("os.Symlink unsupported here: %v", err)
	}

	t.Run("existing symlink resolves", func(t *testing.T) {
		got := CanonicalizeExisting(alias)
		if got != realTarget {
			t.Errorf("CanonicalizeExisting(%q) = %q, want %q", alias, got, realTarget)
		}
	})

	t.Run("nonexistent tail under a symlinked existing parent", func(t *testing.T) {
		want := filepath.Join(realTarget, "nested", "config.yaml")
		got := CanonicalizeExisting(filepath.Join(alias, "nested", "config.yaml"))
		if got != want {
			t.Errorf("CanonicalizeExisting = %q, want %q", got, want)
		}
	})

	t.Run("wholly nonexistent path still resolves through the root", func(t *testing.T) {
		want := filepath.Join(real, "does", "not", "exist")
		got := CanonicalizeExisting(want)
		if got != want {
			t.Errorf("CanonicalizeExisting = %q, want %q", got, want)
		}
	})
}
