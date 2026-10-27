package guest

import (
	"sort"
	"strings"
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
		{"/etc/systemd/system/sandbox-egress-early.service", "0644"},
		{"/usr/local/lib/sandbox/early-egress.sh", "0755"},
		{"/usr/local/lib/sandbox/agent-login-gate.sh", "0755"},
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

func TestInstallUserFileScriptIsDelivered(t *testing.T) {
	files, err := DataFiles()
	if err != nil {
		t.Fatalf("DataFiles: %v", err)
	}
	for _, f := range files {
		if f.Path == "/usr/local/lib/sandbox/install-user-file.sh" {
			if f.Permissions != "0755" {
				t.Errorf("install-user-file.sh permissions = %s, want 0755", f.Permissions)
			}
			return
		}
	}
	t.Error("install-user-file.sh is not delivered to the guest")
}

// ConfigFiles feeds the live re-apply path (`code-vm config apply`), which
// must deliver exactly what lock-settings.sh consumes at boot: the canonical
// config tree, and nothing that only the boot path may touch (scripts, units).
func TestConfigFilesSelectsOnlyTheCanonicalConfigTree(t *testing.T) {
	files, err := ConfigFiles()
	if err != nil {
		t.Fatalf("ConfigFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("ConfigFiles returned nothing")
	}
	found := false
	for _, f := range files {
		if !strings.HasPrefix(f.Path, ConfigRoot+"/") {
			t.Errorf("%s is outside the config tree %s", f.Path, ConfigRoot)
		}
		if f.Path == ConfigRoot+"/.claude/settings.json" {
			found = true
			if f.Permissions != "0444" {
				t.Errorf("settings.json permissions = %s, want 0444", f.Permissions)
			}
		}
	}
	if !found {
		t.Errorf("settings.json missing from %v", files)
	}
}
