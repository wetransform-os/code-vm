package session

import (
	"context"
	"strings"
	"testing"
)

// Staged files must not sit at a path the agent can predict and pre-create:
// the window between `limactl copy` and the root `install` is otherwise a race
// in which the agent can substitute its own content (e.g. extra Squid ACLs).
func TestInstallContentStagesOutsideAgentReach(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := installContent(context.Background(), d, []byte("body\n"),
		"/run/sandbox/squid-allow.d/10-host-config.conf", "0444", "root", "root"); err != nil {
		t.Fatalf("installContent: %v", err)
	}

	var staged string
	for _, call := range r.calls {
		if len(call) >= 3 && call[0] == "copy" {
			staged = call[2]
		}
	}
	if staged == "" {
		t.Fatal("expected a copy call staging the file into the guest")
	}
	guestPath := strings.TrimPrefix(staged, "code-sandbox:")
	if !strings.HasPrefix(guestPath, stageDir+"/") {
		t.Errorf("staged at %q, want a path under the admin-only staging dir %q", guestPath, stageDir)
	}
	if strings.HasPrefix(guestPath, "/tmp/") {
		t.Errorf("staged at %q: /tmp is agent-writable", guestPath)
	}

	joined := []string{}
	for _, call := range r.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "install -d -m 0700 -o "+adminUser) {
		t.Errorf("staging dir must be created 0700 and owned by the admin user, calls:\n%s", all)
	}
	if !strings.Contains(all, "install -D -m 0444 -o root -g root "+guestPath+" /run/sandbox/squid-allow.d/10-host-config.conf") {
		t.Errorf("expected a root install from the staged path, calls:\n%s", all)
	}
	if !strings.Contains(all, "rm -f "+guestPath) {
		t.Errorf("staged copy must be removed after install, calls:\n%s", all)
	}
}

func TestStagedNamesAreUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p, err := stagedPath()
		if err != nil {
			t.Fatalf("stagedPath: %v", err)
		}
		if seen[p] {
			t.Fatalf("stagedPath returned a repeated path %q", p)
		}
		seen[p] = true
	}
}
