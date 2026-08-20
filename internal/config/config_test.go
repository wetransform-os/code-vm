package config

import (
	"os"
	"path/filepath"
	"strings"
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

// extraDomains is now the only way to widen the allowlist, and each entry is
// interpolated straight into a Squid `acl ... dstdomain <entry>` line, so a
// malformed entry could split into two ACLs or append directives.
func TestValidateRejectsMalformedDomains(t *testing.T) {
	good := []string{".example.com", "example.com", "proxy.golang.org", ".sub.example.co.uk", "xn--bcher-kva.example"}
	for _, d := range good {
		c := Default()
		c.ProjectsRoot = "/p"
		c.ExtraDomains = []string{d}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected valid domain %q: %v", d, err)
		}
	}
	bad := []string{
		"",
		"two domains",
		"has\nnewline",
		"http://example.com",
		"example.com/path",
		"example.com:443",
		"all\" ; http_access allow all",
		"-leading-hyphen.com",
		"..double.dot",
		"trailing-.com",
	}
	for _, d := range bad {
		c := Default()
		c.ProjectsRoot = "/p"
		c.ExtraDomains = []string{d}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate() accepted malformed domain %q", d)
		}
	}
}

// The host config is the only trusted input to the allowlist. That holds only
// as long as it is not itself reachable from inside the guest.
func TestMountsExcludeRejectsSharedConfig(t *testing.T) {
	cfgPath := "/home/st/.config/code-vm/config.yaml"
	tests := []struct {
		name    string
		mounts  []string
		wantErr bool
	}{
		{"unrelated projects root", []string{"/home/st/projects"}, false},
		{"home itself shared", []string{"/home/st"}, true},
		{"config dir shared", []string{"/home/st/.config"}, true},
		{"exact config dir shared", []string{"/home/st/.config/code-vm"}, true},
		{"sibling dotdir is fine", []string{"/home/st/.cache"}, false},
		{"extra mount covers it", []string{"/home/st/projects", "/home/st"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.ProjectsRoot = tc.mounts[0]
			c.ExtraMounts = tc.mounts[1:]
			err := c.MountsExclude(cfgPath)
			if (err != nil) != tc.wantErr {
				t.Errorf("MountsExclude() error = %v, wantErr %v", err, tc.wantErr)
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

func TestValidateProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profiles []string
		wantErr  string
	}{
		{"valid names", []string{"fish-shell", "wetf-claude"}, ""},
		{"empty list", nil, ""},
		{"bad name", []string{"has/slash"}, "profiles[0]"},
		{"empty name", []string{""}, "profiles[0]"},
		{"duplicate", []string{"a", "a"}, "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.ProjectsRoot = "/home/st/projects"
			c.Profiles = tt.profiles
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// A symlink outside every mount that resolves inside one must be refused
// just like the real path would be: an agent that can write through the
// alias can edit the config or profile sources the mount guard exists to
// protect, and a lexical comparison alone would never notice.
func TestMountsExcludeRejectsSymlinkedConfigParent(t *testing.T) {
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	mount := filepath.Join(real, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	realConfigDir := filepath.Join(mount, "real-config-dir")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	alias := filepath.Join(real, "alias-config-dir")
	if err := os.Symlink(realConfigDir, alias); err != nil {
		t.Skipf("os.Symlink unsupported here: %v", err)
	}

	c := Default()
	c.ProjectsRoot = mount

	// The alias directory itself lies outside every mount lexically, so the
	// unfixed guard would let this through even though it resolves inside
	// the mount.
	cfgPath := filepath.Join(alias, "config.yaml")
	if err := c.MountsExclude(cfgPath); err == nil {
		t.Errorf("MountsExclude(%q) = nil, want a refusal (alias resolves into %q)", cfgPath, mount)
	}
}

// Same shape as above, but for the profiles-tree guard, and with the
// profiles directory not yet created — the common case before the first
// `profile add` — to confirm a nonexistent path under a symlinked existing
// parent still canonicalizes rather than erroring or silently passing.
func TestMountsExcludeTreeRejectsSymlinkedNonexistentProfilesDir(t *testing.T) {
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	mount := filepath.Join(real, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The config directory itself exists (EvalSymlinks needs a real target
	// to resolve through), but the profiles subdirectory under it does not:
	// that is the common case before the first `profile add`.
	realConfigDir := filepath.Join(mount, "real-config-dir")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	alias := filepath.Join(real, "alias-config-dir")
	if err := os.Symlink(realConfigDir, alias); err != nil {
		t.Skipf("os.Symlink unsupported here: %v", err)
	}

	c := Default()
	c.ProjectsRoot = mount

	profilesDir := filepath.Join(alias, "profiles")
	if err := c.MountsExcludeTree(profilesDir); err == nil {
		t.Errorf("MountsExcludeTree(%q) = nil, want a refusal (alias resolves into %q)", profilesDir, mount)
	}
}

func TestMountsExcludeTree(t *testing.T) {
	dir := "/home/st/.config/code-vm/profiles"
	tests := []struct {
		name    string
		mount   string
		wantErr bool
	}{
		{"unrelated mount", "/home/st/projects", false},
		{"sibling with shared prefix", "/home/st/.config/code-vm-other", false},
		{"mount above the tree", "/home/st/.config", true},
		{"the tree itself", dir, true},
		{"mount inside the tree", dir + "/one-profile", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.ProjectsRoot = tt.mount
			err := c.MountsExcludeTree(dir)
			if (err != nil) != tt.wantErr {
				t.Errorf("MountsExcludeTree(%q) with mount %q = %v, wantErr %v", dir, tt.mount, err, tt.wantErr)
			}
		})
	}
}
