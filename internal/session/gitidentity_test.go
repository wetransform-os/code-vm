package session

import (
	"context"
	"strings"
	"testing"
)

func TestGitConfigContent(t *testing.T) {
	got := GitConfigContent("Ada Lovelace", "ada@example.com")
	for _, want := range []string{"[user]", "name = Ada Lovelace", "email = ada@example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("GitConfigContent missing %q, got:\n%s", want, got)
		}
	}
}

func TestGitConfigContentOmitsMissingFields(t *testing.T) {
	got := GitConfigContent("", "ada@example.com")
	if strings.Contains(got, "name =") {
		t.Errorf("empty name must be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "email = ada@example.com") {
		t.Error("email should still be written")
	}
}

// The gitconfig is delivered through the agent-privilege relay, not a root
// install: ownership then falls out of which user runs the final install,
// not an explicit -o/-g pair, so there is no numeric-vs-named-group pitfall
// left here (see userfiles_test.go for the relay assertions).
func TestApplyGitIdentityDeliversToDotfile(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.AgentUID, d.AgentGID = 1000, 100
	d.Host = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "user.email" {
			return []byte("ada@example.com\n"), nil
		}
		return []byte("Ada Lovelace\n"), nil
	}
	if err := ApplyGitIdentity(context.Background(), d); err != nil {
		t.Fatalf("ApplyGitIdentity: %v", err)
	}
	if !r.ranAny(".gitconfig") || !r.ranAny("0644") {
		t.Errorf("gitconfig must be relayed with its home-relative path and mode, got %v", r.calls)
	}
}

func TestApplyGitIdentitySkipsWhenHostHasNoIdentity(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.Host = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, context.Canceled // simulate `git config --get` exiting non-zero
	}
	if err := ApplyGitIdentity(context.Background(), d); err != nil {
		t.Fatalf("ApplyGitIdentity: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no guest calls when the host has no git identity, got %v", r.calls)
	}
}
