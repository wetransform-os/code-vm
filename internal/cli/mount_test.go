package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestAddMountAppendsNewDirectory(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	c := config.Default()
	c.ProjectsRoot = root

	got, changed, err := addMount(c, other)
	if err != nil {
		t.Fatalf("addMount: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a new directory")
	}
	if len(got.ExtraMounts) != 1 || got.ExtraMounts[0] != other {
		t.Errorf("ExtraMounts = %v, want [%s]", got.ExtraMounts, other)
	}
}

func TestAddMountIsNoOpForAlreadyCoveredPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "repo")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	c := config.Default()
	c.ProjectsRoot = root

	got, changed, err := addMount(c, sub)
	if err != nil {
		t.Fatalf("addMount: %v", err)
	}
	if changed {
		t.Error("a path already under the projects root needs no new mount")
	}
	if len(got.ExtraMounts) != 0 {
		t.Errorf("ExtraMounts = %v, want empty", got.ExtraMounts)
	}
}

func TestAddMountRejectsMissingOrNonDirectory(t *testing.T) {
	root := t.TempDir()
	c := config.Default()
	c.ProjectsRoot = root

	if _, _, err := addMount(c, filepath.Join(root, "absent")); err == nil {
		t.Error("expected an error for a path that does not exist")
	}

	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := addMount(c, file); err == nil {
		t.Error("expected an error for a path that is not a directory")
	}
}

// The mount command builds an updated config directly (addMount) rather than
// going through loadConfig, so it never ran MountsExclude/MountsExcludeTree
// against what it was about to save — `code-vm mount ~/.config/code-vm/profiles`
// would happily mount the profiles source into the guest and only fail on the
// *next* invocation. This pins the fix: the same two guards loadConfig runs
// must run again on the updated config before it is saved or the VM touched.
func TestMountRefusesProfilesDirectoryWithoutSavingOrRestarting(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profilesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	r := installFakeClient(t, "Running")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"mount", profilesRoot})
	err = root.Execute()
	if err == nil {
		t.Fatalf("expected mount to refuse the profiles directory; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "expose the code-vm profiles") {
		t.Errorf("error = %v, want a profiles-guard refusal", err)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config file was modified despite the refused mount:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no guest interaction before the guard runs, got %v", r.calls)
	}
}

// Resolution needs nothing from the guest, so an unmapped secret must fail
// `code-vm mount` before the running VM is stopped for the restart — leaving
// it running rather than stopped-and-not-restarted.
func TestMountFailsBeforeStoppingOnUnmappedSecret(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.yaml"),
		[]byte("secrets:\n  tok:\n    suggest: gopass show -o t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "templates", ".npmrc"), []byte("${secret:tok}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, "profiles:\n  - p\n")
	// No secrets.yaml: "tok" is unmapped.

	other := t.TempDir()
	r := installFakeClient(t, "Running")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"mount", other})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("mount = %v, want an unmapped-secret error with the suggest snippet; output:\n%s", err, out.String())
	}
	if ranAny(r.calls, "stop") {
		t.Errorf("VM must not be stopped before resolution succeeds, calls=%v", r.calls)
	}
	if ranAny(r.calls, "copy") {
		t.Errorf("no file may be staged into the guest before resolution succeeds, calls=%v", r.calls)
	}
}
