package session

import (
	"context"
	"testing"
)

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
