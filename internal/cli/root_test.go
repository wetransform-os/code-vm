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

// The root command takes arbitrary args because a bare `code-vm -- <cmd>`
// forwards them to the guest. That must not extend to args with no `--` in
// front of them: cobra would hand any unrecognized word to the guest, so a
// typo — or a stale binary that predates a subcommand the caller expects —
// surfaced as the guest shell's `exec: profile: not found` and exit 127
// instead of a host-side "unknown command". Nothing may reach the VM.
func TestRootRejectsUnknownSubcommandBeforeTouchingTheVM(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "")
	root.SetArgs([]string{"profile-typo", "add", "git@example.com:x/y.git"})
	err := root.Execute()
	if err == nil {
		t.Fatal("unknown subcommand = nil error, want a host-side refusal")
	}
	if !strings.Contains(err.Error(), `unknown command "profile-typo"`) {
		t.Errorf("error = %v, want it to name the unknown command", err)
	}
	if !strings.Contains(err.Error(), "--") {
		t.Errorf("error = %v, want it to point at `--` for sandbox passthrough", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may reach the VM for an unknown command, calls=%v", r.calls)
	}
}

// The counterpart: an explicit `--` still forwards everything after it to the
// guest verbatim, including words that collide with host subcommand names.
func TestRootPassesArgsAfterDoubleDashToTheGuest(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"--", "claude", "login"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`-- claude login` = %v, want passthrough", err)
	}
	if !ranAny(r.calls, "claude login") {
		t.Errorf("guest command not forwarded, calls=%v", r.calls)
	}
}
