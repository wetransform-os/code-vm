package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative --config must be resolved to an absolute path before it reaches
// MountsExclude/MountsExcludeTree: both compare it against absolute mounts,
// so a relative path never matches and both guards silently pass. This test
// pins the fix by using a projectsRoot that covers the config file's
// directory — loadConfig must refuse that regardless of how --config was
// spelled.
func TestLoadConfigCanonicalizesRelativeConfigPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// projectsRoot covers the directory containing the config file itself.
	if err := os.WriteFile(cfgPath, []byte("projectsRoot: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)
	old := configPath
	configPath = "./config.yaml"
	t.Cleanup(func() { configPath = old })

	_, _, err := loadConfig()
	if err == nil {
		t.Fatal("expected loadConfig to refuse a relative --config whose projectsRoot covers it")
	}
	if !strings.Contains(err.Error(), "expose the code-vm config") {
		t.Errorf("loadConfig error = %v, want a MountsExclude refusal", err)
	}
}

// A symlinked config directory must be refused even though its lexical path
// never shares a prefix with the mount: loadConfig canonicalizes both sides
// before comparing, so an alias that resolves inside projectsRoot cannot
// sail through as a config location "outside every mount".
func TestLoadConfigCanonicalizesSymlinkedConfigDir(t *testing.T) {
	base := t.TempDir()
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	projectsRoot := filepath.Join(real, "projects")
	if err := os.MkdirAll(projectsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	realConfigDir := filepath.Join(projectsRoot, "real-config-dir")
	if err := os.MkdirAll(realConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasConfigDir := filepath.Join(real, "alias-config-dir")
	if err := os.Symlink(realConfigDir, aliasConfigDir); err != nil {
		t.Skipf("os.Symlink unsupported here: %v", err)
	}

	cfgPath := filepath.Join(aliasConfigDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("projectsRoot: "+projectsRoot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := configPath
	configPath = cfgPath
	t.Cleanup(func() { configPath = old })

	_, _, err = loadConfig()
	if err == nil {
		t.Fatal("expected loadConfig to refuse a symlinked config dir that resolves inside projectsRoot")
	}
	if !strings.Contains(err.Error(), "expose the code-vm config") {
		t.Errorf("loadConfig error = %v, want a MountsExclude refusal", err)
	}
}
