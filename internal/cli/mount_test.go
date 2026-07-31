package cli

import (
	"os"
	"path/filepath"
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
