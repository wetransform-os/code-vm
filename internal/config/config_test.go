package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultValues(t *testing.T) {
	d := Default()
	if d.CPUs != 4 || d.Memory != "12GiB" || d.Disk != "100GiB" {
		t.Errorf("unexpected defaults: %+v", d)
	}
	if d.ContainerProxy {
		t.Error("containerProxy must default to false")
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CPUs != Default().CPUs {
		t.Errorf("CPUs = %d, want %d", got.CPUs, Default().CPUs)
	}
}

func TestLoadOverridesAndExpands(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "projectsRoot: ~/code\nextraMounts:\n  - ~/work/thing\ncpus: 8\nextraDomains:\n  - registry.example.com\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProjectsRoot != filepath.Join(home, "code") {
		t.Errorf("ProjectsRoot = %q", got.ProjectsRoot)
	}
	if got.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8", got.CPUs)
	}
	if got.Memory != Default().Memory {
		t.Errorf("Memory = %q, want default %q", got.Memory, Default().Memory)
	}
	mounts := got.Mounts()
	if len(mounts) != 2 || mounts[0] != filepath.Join(home, "code") || mounts[1] != filepath.Join(home, "work/thing") {
		t.Errorf("Mounts() = %v", mounts)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults expanded are valid", func(c *Config) { c.ProjectsRoot = "/home/st/projects" }, false},
		{"empty projectsRoot", func(c *Config) { c.ProjectsRoot = "" }, true},
		{"relative projectsRoot", func(c *Config) { c.ProjectsRoot = "projects" }, true},
		{"zero cpus", func(c *Config) { c.ProjectsRoot = "/p"; c.CPUs = 0 }, true},
		{"bad memory", func(c *Config) { c.ProjectsRoot = "/p"; c.Memory = "lots" }, true},
		{"bad disk", func(c *Config) { c.ProjectsRoot = "/p"; c.Disk = "12" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	c := Default()
	c.ProjectsRoot = "/home/st/projects"
	c.ExtraMounts = []string{"/home/st/other"}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProjectsRoot != c.ProjectsRoot || len(got.ExtraMounts) != 1 || got.ExtraMounts[0] != "/home/st/other" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}
