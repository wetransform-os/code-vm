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

// Group ownership must be set by numeric GID. The guest group carrying the
// host's GID is often a stock group with a different name — a host user with
// GID 100 lands in "users" — so `install -g devuser` fails outright there.
func TestApplyGitIdentityInstallsByNumericIDs(t *testing.T) {
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
	if !r.ranAny("install -m 0644 -o 1000 -g 100") {
		t.Errorf("gitconfig must be installed with numeric owner/group, got %v", r.calls)
	}
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), "-g devuser") {
			t.Errorf("must not set the group by name: %v", c)
		}
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
