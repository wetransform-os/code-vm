package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const secretsYAML = `
secrets:
  NEXUS_USER:
    source: printf ci-user
  NEXUS_PASS:
    source: printf 'ci-pass\n'
targets:
  - template: gradle-properties
    dest: /home/devuser/.gradle/gradle.properties
    secrets:
      - name: NEXUS_USER
        as: nexusUser
      - name: NEXUS_PASS
        as: nexusPassword
  - template: dotenv
    dest: /workspace/.env.sandbox
    secrets:
      - NEXUS_USER
`

func writeSecrets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-secrets.yaml"), []byte(secretsYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

func TestParseSecretsFileAcceptsBothSecretForms(t *testing.T) {
	dir := writeSecrets(t)
	sf, ok, err := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	if err != nil || !ok {
		t.Fatalf("ParseSecretsFile: ok=%v err=%v", ok, err)
	}
	if len(sf.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(sf.Targets))
	}
	if got := sf.Targets[0].Secrets[0]; got.Name != "NEXUS_USER" || got.As != "nexusUser" {
		t.Errorf("object form parsed as %+v", got)
	}
	// Shorthand form: the alias defaults to the name.
	if got := sf.Targets[1].Secrets[0]; got.Name != "NEXUS_USER" || got.As != "NEXUS_USER" {
		t.Errorf("scalar form parsed as %+v", got)
	}
}

func TestParseSecretsFileMissingIsNotAnError(t *testing.T) {
	_, ok, err := ParseSecretsFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("ParseSecretsFile: %v", err)
	}
	if ok {
		t.Error("ok must be false for a missing file")
	}
}

func TestResolveSecretsTrimsNewlines(t *testing.T) {
	dir := writeSecrets(t)
	sf, _, err := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	if err != nil {
		t.Fatalf("ParseSecretsFile: %v", err)
	}
	host := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		// args is ["-c", "<source command>"]; return a value with a newline.
		if len(args) == 2 && args[1] == "printf ci-user" {
			return []byte("ci-user\n"), nil
		}
		return []byte("ci-pass\n"), nil
	}
	got, err := ResolveSecrets(context.Background(), host, sf)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if got["NEXUS_USER"] != "ci-user" || got["NEXUS_PASS"] != "ci-pass" {
		t.Errorf("ResolveSecrets = %v, want newline-trimmed values", got)
	}
}

func TestResolveSecretsReportsTheFailingSecret(t *testing.T) {
	dir := writeSecrets(t)
	sf, _, _ := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	host := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	_, err := ResolveSecrets(context.Background(), host, sf)
	if err == nil {
		t.Fatal("expected an error when a source command fails")
	}
	if !strings.Contains(err.Error(), "NEXUS_") {
		t.Errorf("error must name the failing secret, got %q", err)
	}
}

func TestDenyRulesCoverReadAndShellPaths(t *testing.T) {
	rules := DenyRules([]Target{{Dest: "/home/devuser/.netrc"}})
	want := []string{
		"Bash(cat /home/devuser/.netrc*)",
		"Bash(grep * /home/devuser/.netrc*)",
		"Bash(head * /home/devuser/.netrc*)",
		"Bash(python * /home/devuser/.netrc*)",
		"Bash(python3 * /home/devuser/.netrc*)",
		"Bash(tail * /home/devuser/.netrc*)",
		"Read(/home/devuser/.netrc)",
	}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("DenyRules =\n%v\nwant (sorted, de-duplicated)\n%v", rules, want)
	}
}

func TestBuildPayloadShape(t *testing.T) {
	body, err := BuildPayload("/home/st/projects/repo",
		map[string]string{"A": "1"},
		[]Target{{Template: "dotenv", Dest: "/tmp/x", Secrets: []SecretRef{{Name: "A", As: "alias"}}}})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got struct {
		Workspace string            `json:"workspace"`
		Secrets   map[string]string `json:"secrets"`
		Targets   []struct {
			Template string `json:"template"`
			Dest     string `json:"dest"`
			Secrets  []struct {
				Name string `json:"name"`
				As   string `json:"as"`
			} `json:"secrets"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got.Workspace != "/home/st/projects/repo" {
		t.Errorf("workspace = %q; the guest needs it to resolve custom templates", got.Workspace)
	}
	if got.Secrets["A"] != "1" || len(got.Targets) != 1 || got.Targets[0].Secrets[0].As != "alias" {
		t.Errorf("unexpected payload: %s", body)
	}
}

func TestBuildPayloadOmitsEmptySecretsList(t *testing.T) {
	body, err := BuildPayload("/ws", map[string]string{"A": "1"}, []Target{{Template: "dotenv", Dest: "/tmp/x"}})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	targets := got["targets"].([]any)
	target := targets[0].(map[string]any)
	if v, present := target["secrets"]; present && v != nil {
		t.Errorf("an empty secrets list must be omitted so the guest renders all secrets, got %v", v)
	}
}
