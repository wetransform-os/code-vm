package profile

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestFindRefs(t *testing.T) {
	content := []byte(`user=${secret:repo-user} url=${var:base-url}
again=${secret:repo-user} passthrough=${env.FOO} ${prop} $secret:no ${secret:BAD NAME}`)
	got := FindRefs(content)
	want := []Ref{{Kind: "secret", Name: "repo-user"}, {Kind: "var", Name: "base-url"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindRefs = %v, want %v", got, want)
	}
}

func TestRenderTemplatesSubstitutesAndPassesThrough(t *testing.T) {
	profiles := []Profile{{
		Name: "a",
		Templates: []File{{Rel: ".m2/settings.xml", Content: []byte(
			"<u>${secret:repo-user}</u><url>${var:base-url}</url><keep>${env.HOME}</keep>")}},
	}}
	out := RenderTemplates(profiles,
		map[string]string{"repo-user": "simon"},
		map[string]string{"base-url": "https://x.example"})
	if len(out) != 1 {
		t.Fatalf("Rendered = %+v", out)
	}
	want := "<u>simon</u><url>https://x.example</url><keep>${env.HOME}</keep>"
	if string(out[0].Content) != want {
		t.Errorf("Content = %q, want %q", out[0].Content, want)
	}
}

func TestRenderTemplatesLaterProfileWins(t *testing.T) {
	profiles := []Profile{
		{Name: "a", Templates: []File{{Rel: ".npmrc", Content: []byte("from-a")}}},
		{Name: "b", Templates: []File{{Rel: ".npmrc", Content: []byte("from-b")}}},
	}
	out := RenderTemplates(profiles, nil, nil)
	if len(out) != 1 || string(out[0].Content) != "from-b" {
		t.Errorf("collision must resolve to the later profile, got %+v", out)
	}
}

func TestDeclaredSecretsMergesAcrossProfiles(t *testing.T) {
	profiles := []Profile{
		{Name: "a", Manifest: Manifest{Secrets: map[string]SecretSpec{
			"tok": {Description: "token", Suggest: "gopass show -o t"}}}},
		{Name: "b", Manifest: Manifest{Secrets: map[string]SecretSpec{"tok": {}}}},
	}
	got := DeclaredSecrets(profiles)
	if len(got) != 1 || got[0].Name != "tok" || got[0].Suggest != "gopass show -o t" ||
		!reflect.DeepEqual(got[0].Profiles, []string{"a", "b"}) {
		t.Errorf("DeclaredSecrets = %+v", got)
	}
}

func TestResolveSecrets(t *testing.T) {
	declared := []DeclaredSecret{
		{Name: "from-cmd", Profiles: []string{"p"}},
		{Name: "from-val", Profiles: []string{"p"}},
	}
	sources := map[string]config.SecretSource{
		"from-cmd": {Command: "get-it"},
		"from-val": {Value: "literal"},
	}
	calls := 0
	run := func(_ context.Context, command string) ([]byte, error) {
		calls++
		if command != "get-it" {
			t.Errorf("command = %q", command)
		}
		return []byte("resolved\n"), nil
	}
	got, err := ResolveSecrets(context.Background(), declared, sources, run)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	// Exactly one trailing newline stripped; command runs once per secret.
	if got["from-cmd"] != "resolved" || got["from-val"] != "literal" || calls != 1 {
		t.Errorf("got %v, calls=%d", got, calls)
	}
}

func TestResolveSecretsMissingMappingHasSnippet(t *testing.T) {
	declared := []DeclaredSecret{{
		Name: "repo-user", Profiles: []string{"maven"},
		Description: "Artifactory user", Suggest: "gopass show -o wetf/user",
	}}
	_, err := ResolveSecrets(context.Background(), declared, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an unmapped secret")
	}
	for _, want := range []string{"repo-user", "maven", "Artifactory user",
		"secrets:", "command: gopass show -o wetf/user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestResolveSecretsCommandFailure(t *testing.T) {
	declared := []DeclaredSecret{{Name: "tok", Profiles: []string{"p"}}}
	sources := map[string]config.SecretSource{"tok": {Command: "boom"}}
	run := func(context.Context, string) ([]byte, error) {
		return []byte("stderr text"), errors.New("exit status 1")
	}
	_, err := ResolveSecrets(context.Background(), declared, sources, run)
	if err == nil || !strings.Contains(err.Error(), "tok") || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("ResolveSecrets error = %v", err)
	}
}

func TestResolveVars(t *testing.T) {
	declared := []DeclaredVar{{Name: "url", Profiles: []string{"p"}, Description: "Base URL"}}
	got, err := ResolveVars(declared, map[string]string{"url": "https://x"})
	if err != nil || got["url"] != "https://x" {
		t.Errorf("ResolveVars = %v, %v", got, err)
	}
	_, err = ResolveVars(declared, nil)
	if err == nil || !strings.Contains(err.Error(), "vars:") || !strings.Contains(err.Error(), "url") {
		t.Errorf("missing var must produce a config.yaml snippet, got %v", err)
	}
}

// Load must reject a template referencing an undeclared name (wired in this task).
func TestLoadRejectsUndeclaredPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "secrets:\n  known: {}\n", map[string]string{
		"templates/.npmrc": "a=${secret:known} b=${var:never-declared}\n",
	})
	_, err := Load(dir, "p")
	if err == nil || !strings.Contains(err.Error(), "never-declared") {
		t.Errorf("Load = %v, want undeclared-placeholder rejection", err)
	}
}
