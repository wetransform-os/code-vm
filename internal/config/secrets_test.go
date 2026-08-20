package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsPathFor(t *testing.T) {
	if got := SecretsPathFor("/home/st/.config/code-vm/config.yaml"); got != "/home/st/.config/code-vm/secrets.yaml" {
		t.Errorf("SecretsPathFor = %q", got)
	}
}

func TestLoadSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets.yaml")
	content := "secrets:\n  a:\n    command: gopass show -o x\n  b:\n    value: literal\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, warnings, err := LoadSecrets(p)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for 0600", warnings)
	}
	if sources["a"].Command != "gopass show -o x" || sources["b"].Value != "literal" {
		t.Errorf("sources = %+v", sources)
	}
}

func TestLoadSecretsMissingFileIsEmpty(t *testing.T) {
	sources, warnings, err := LoadSecrets(filepath.Join(t.TempDir(), "secrets.yaml"))
	if err != nil || len(sources) != 0 || len(warnings) != 0 {
		t.Errorf("missing file must load empty: %v %v %v", sources, warnings, err)
	}
}

func TestLoadSecretsRejectsBadEntries(t *testing.T) {
	tests := []struct{ name, content, wantErr string }{
		{"both command and value", "secrets:\n  a:\n    command: c\n    value: v\n", "exactly one"},
		{"neither", "secrets:\n  a: {}\n", "exactly one"},
		{"bad name", "secrets:\n  'has space':\n    value: v\n", "secret name"},
		{"unknown key", "secrets:\n  a:\n    comand: typo\n", "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "secrets.yaml")
			if err := os.WriteFile(p, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := LoadSecrets(p)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadSecrets = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadSecretsWarnsOnLoosePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(p, []byte("secrets:\n  a:\n    value: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := LoadSecrets(p)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "0600") {
		t.Errorf("want a permissions warning recommending 0600, got %v %v", warnings, err)
	}
}
