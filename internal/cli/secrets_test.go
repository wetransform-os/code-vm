package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsListsMappedAndUnmapped(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.yaml"), []byte(
		"secrets:\n  mapped-one:\n    description: has a mapping\n  missing-one:\n    suggest: gopass show -o x\nvars:\n  url: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "templates", ".npmrc"), []byte("${secret:mapped-one}${secret:missing-one}${var:url}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("secrets:\n  mapped-one:\n    value: v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, "profiles:\n  - p\n")

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"secrets"})
	if err := root.Execute(); err != nil {
		t.Fatalf("secrets: %v", err)
	}
	s := out.String()
	for _, want := range []string{"mapped-one", "missing-one", "UNMAPPED", "url",
		`command: "gopass show -o x"`} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "v\n") && strings.Contains(s, "value") {
		t.Errorf("secret values must never be printed:\n%s", s)
	}
}

// TestSecretsExitsZeroWithNoProfiles guards the "report, not a gate" contract:
// an empty union (no active profiles declare anything) is not an error.
func TestSecretsExitsZeroWithNoProfiles(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"secrets"})
	if err := root.Execute(); err != nil {
		t.Fatalf("secrets with nothing declared should exit 0: %v", err)
	}
}
