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
