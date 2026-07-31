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

func TestApplyGitIdentitySkipsWhenHostHasNoIdentity(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r, t.TempDir())
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
