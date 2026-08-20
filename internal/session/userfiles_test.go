package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// erroringRunner fails the Run call whose args join contains match; every
// other call is delegated to the embedded fakeRunner so it still records
// calls and behaves normally.
type erroringRunner struct {
	fakeRunner
	match string
	err   error
}

func (r *erroringRunner) Run(ctx context.Context, args ...string) error {
	if strings.Contains(strings.Join(args, " "), r.match) {
		r.calls = append(r.calls, args)
		return r.err
	}
	return r.fakeRunner.Run(ctx, args...)
}

// A VM booted from a pre-this-feature code-vm binary lacks
// install-user-file.sh, so PushUserFile's Admin call fails with an opaque
// exec error. The wrapped message must point at the fix: restarting the VM.
func TestPushUserFileWrapsAdminFailureWithUpgradeHint(t *testing.T) {
	r := &erroringRunner{match: "install-user-file.sh", err: errors.New("exec format error")}
	d := testDeps(t, r)
	err := PushUserFile(context.Background(), d, []byte("content"), ".gitconfig", "0644")
	if err == nil {
		t.Fatal("PushUserFile: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "code-vm stop") || !strings.Contains(err.Error(), "code-vm start") {
		t.Errorf("PushUserFile error = %v, want it to mention `code-vm stop && code-vm start`", err)
	}
	if !errors.Is(err, r.err) {
		t.Errorf("PushUserFile error = %v, want it to wrap the underlying error (%%w)", err)
	}
}

// When the relay Admin call fails — the supported old-VM/missing-script
// case — the staged copy must still be cleaned up: otherwise a rendered
// credential is left sitting in the admin-only staging dir indefinitely.
func TestPushUserFileCleansUpStagedFileOnRelayFailure(t *testing.T) {
	r := &erroringRunner{match: "install-user-file.sh", err: errors.New("exec format error")}
	d := testDeps(t, r)
	err := PushUserFile(context.Background(), d, []byte("content"), ".gitconfig", "0644")
	if err == nil {
		t.Fatal("PushUserFile: expected an error, got nil")
	}
	if !r.ranAny("rm -f") {
		t.Errorf("expected a best-effort staged-file cleanup after the relay failed, calls=%v", r.calls)
	}
}

func TestPushUserFileStagesAndRelays(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := PushUserFile(context.Background(), d, []byte("content"), ".m2/settings.xml", "0600"); err != nil {
		t.Fatalf("PushUserFile: %v", err)
	}
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("staged copies = %d, want 1", copies)
	}
	if !r.ranAny("/usr/local/lib/sandbox/install-user-file.sh") {
		t.Errorf("relay script not invoked: %v", r.calls)
	}
	if !r.ranAny(".m2/settings.xml") || !r.ranAny("0600") {
		t.Errorf("dst/mode not passed to the relay: %v", r.calls)
	}
	// The old direct-to-home root install must NOT happen for user files.
	if r.ranAny("install -D -m 0600") {
		t.Errorf("user files must not be root-installed into the home: %v", r.calls)
	}
}

func TestPushUserFileRejectsUnsafeRel(t *testing.T) {
	for _, rel := range []string{"../x", "/abs", "", "has space", "semi;colon", "dollar$sign"} {
		t.Run(rel, func(t *testing.T) {
			r := &fakeRunner{}
			d := testDeps(t, r)
			err := PushUserFile(context.Background(), d, []byte("content"), rel, "0600")
			if err == nil {
				t.Fatalf("PushUserFile(%q) = nil error, want a rejection", rel)
			}
			if len(r.calls) != 0 {
				t.Errorf("unsafe rel must not reach the guest, calls=%v", r.calls)
			}
		})
	}
}

func TestPushUserFileAcceptsCleanRel(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := PushUserFile(context.Background(), d, []byte("content"), ".m2/settings.xml", "0600"); err != nil {
		t.Fatalf("PushUserFile: %v", err)
	}
}

func TestGitIdentityUsesUserFilePush(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.Host = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("simon\n"), nil
	}
	if err := ApplyGitIdentity(context.Background(), d); err != nil {
		t.Fatalf("ApplyGitIdentity: %v", err)
	}
	if !r.ranAny("install-user-file.sh") || !r.ranAny(".gitconfig") {
		t.Errorf("git identity must go through the relay: %v", r.calls)
	}
	if r.ranAny("install -D -m 0644") {
		t.Errorf("git identity must no longer be root-installed: %v", r.calls)
	}
}
