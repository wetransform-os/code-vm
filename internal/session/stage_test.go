package session

import (
	"context"
	"errors"
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

// copyFailingRunner fails every `limactl copy` invocation while otherwise
// behaving like fakeRunner, so tests can exercise stageFile's cleanup path
// without a real guest.
type copyFailingRunner struct {
	fakeRunner
}

func (r *copyFailingRunner) Run(ctx context.Context, args ...string) error {
	if err := r.fakeRunner.Run(ctx, args...); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "copy" {
		return errors.New("simulated copy failure")
	}
	return nil
}

// A Copy failure can leave a partial file behind at the staged guest path.
// stageFile must attempt a best-effort removal of it before returning the
// error, rather than leaving rendered credential bytes sitting in the
// admin-only staging dir — the same posture PushUserFile's deferred cleanup
// takes on its own failure paths.
func TestStageFileCleansUpPartialFileOnCopyFailure(t *testing.T) {
	r := &copyFailingRunner{}
	d := testDeps(t, r)
	_, err := stageFile(context.Background(), d, []byte("secret\n"))
	if err == nil {
		t.Fatal("expected stageFile to return the Copy error")
	}
	if !strings.Contains(err.Error(), "simulated copy failure") {
		t.Errorf("stageFile error = %v, want it to wrap the Copy failure", err)
	}

	var stagedGuestPath string
	for _, call := range r.calls {
		if len(call) >= 2 && call[0] == "copy" {
			// copy <local> <instance>:<guestPath>
			dst := call[2]
			stagedGuestPath = strings.TrimPrefix(dst, "code-sandbox:")
		}
	}
	if stagedGuestPath == "" {
		t.Fatal("expected a copy call attempting to stage the file")
	}
	if !r.ranAny("rm -f " + stagedGuestPath) {
		t.Errorf("expected a best-effort cleanup of the partial staged file %q, calls=%v", stagedGuestPath, r.calls)
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
