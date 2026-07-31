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
