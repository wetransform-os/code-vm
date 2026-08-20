package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Resolution needs nothing from the guest, so an unmapped secret must fail
// `code-vm recreate` before the VM is deleted — Delete is irreversible (guest
// disk, Claude auth, Docker image cache), so a resolvable-beforehand error
// must never destroy the user's VM.
func TestRecreateFailsBeforeDeletingOnUnmappedSecret(t *testing.T) {
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

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"recreate", "--yes"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("recreate = %v, want an unmapped-secret error with the suggest snippet", err)
	}
	if ranAny(r.calls, "delete") {
		t.Errorf("VM must not be deleted before resolution succeeds, calls=%v", r.calls)
	}
	if ranAny(r.calls, "copy") {
		t.Errorf("no file may be staged into the guest before resolution succeeds, calls=%v", r.calls)
	}
}
