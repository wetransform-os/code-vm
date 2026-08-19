package guest

import (
	"sort"
	"testing"
)

func TestDataFilesMapsGuestPaths(t *testing.T) {
	files, err := DataFiles()
	if err != nil {
		t.Fatalf("DataFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("DataFiles returned nothing")
	}
	byPath := map[string]DataFile{}
	paths := []string{}
	for _, f := range files {
		byPath[f.Path] = f
		paths = append(paths, f.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Errorf("DataFiles must be sorted by Path, got %v", paths)
	}
	for _, want := range []struct {
		path  string
		perms string
	}{
		{"/usr/local/bin/sandbox-exec", "0755"},
		{"/usr/local/lib/sandbox/provision-system.sh", "0755"},
		{"/etc/systemd/system/sandbox-boot.service", "0644"},
		{"/usr/local/share/sandbox-config/.claude/settings.json", "0444"},
	} {
		f, ok := byPath[want.path]
		if !ok {
			t.Errorf("missing guest path %s", want.path)
			continue
		}
		if f.Permissions != want.perms {
			t.Errorf("%s permissions = %s, want %s", want.path, f.Permissions, want.perms)
		}
		if f.Content == "" {
			t.Errorf("%s has empty content", want.path)
		}
	}
}

func TestLimaTemplateIsPresent(t *testing.T) {
	tpl, err := LimaTemplate()
	if err != nil {
		t.Fatalf("LimaTemplate: %v", err)
	}
	if tpl == "" {
		t.Fatal("LimaTemplate is empty")
	}
}

func TestApplyProfilesScriptIsDelivered(t *testing.T) {
	files, err := DataFiles()
	if err != nil {
		t.Fatalf("DataFiles: %v", err)
	}
	for _, f := range files {
		if f.Path == "/usr/local/lib/sandbox/apply-profiles.sh" {
			if f.Permissions != "0755" {
				t.Errorf("apply-profiles.sh permissions = %s, want 0755", f.Permissions)
			}
			return
		}
	}
	t.Error("apply-profiles.sh is not delivered to the guest")
}
