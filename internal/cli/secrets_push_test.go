package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/profile"
)

func TestPushRenderedTemplatesNoOpWithoutDeclarations(t *testing.T) {
	r := &recordingRunner{statusOut: "Running"}
	c := testCfg(t)
	profiles := []profile.Profile{{Name: "plain", Manifest: profile.Manifest{Packages: []string{"git"}}}}
	if err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, filepath.Join(t.TempDir(), "config.yaml"), io.Discard); err != nil {
		t.Fatalf("pushRenderedTemplates: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("no declarations must mean no guest traffic, got %v", r.calls)
	}
}

func TestPushRenderedTemplatesResolvesAndPushes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("secrets:\n  tok:\n    value: sekrit\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := &recordingRunner{statusOut: "Running"}
	c := testCfg(t)
	c.Vars = map[string]string{"url": "https://x"}
	profiles := []profile.Profile{{
		Name: "p",
		Manifest: profile.Manifest{
			Secrets: map[string]profile.SecretSpec{"tok": {}},
			Vars:    map[string]profile.VarSpec{"url": {}},
		},
		Templates: []profile.File{{Rel: ".npmrc", Content: []byte("t=${secret:tok};u=${var:url}")}},
	}}
	if err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, cfgPath, io.Discard); err != nil {
		t.Fatalf("pushRenderedTemplates: %v", err)
	}
	if !ranAny(r.calls, "install-user-file.sh") || !ranAny(r.calls, ".npmrc") || !ranAny(r.calls, "0600") {
		t.Errorf("expected a relay push of .npmrc at 0600, got %v", r.calls)
	}
}

func TestPushRenderedTemplatesMissingMappingFails(t *testing.T) {
	r := &recordingRunner{}
	c := testCfg(t)
	profiles := []profile.Profile{{
		Name:      "p",
		Manifest:  profile.Manifest{Secrets: map[string]profile.SecretSpec{"tok": {Suggest: "gopass show -o t"}}},
		Templates: []profile.File{{Rel: ".npmrc", Content: []byte("${secret:tok}")}},
	}}
	err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, filepath.Join(t.TempDir(), "config.yaml"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Errorf("missing mapping must fail with the snippet, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may reach the guest on resolution failure, got %v", r.calls)
	}
}
