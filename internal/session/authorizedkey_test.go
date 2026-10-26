package session

import (
	"context"
	"testing"
)

func TestPushAuthorizedKeyGoesThroughTheRelay(t *testing.T) {
	f := &fakeRunner{}
	d := testDeps(t, f)
	if err := PushAuthorizedKey(context.Background(), d, "ssh-ed25519 AAAA code-vm code-sandbox agent"); err != nil {
		t.Fatal(err)
	}
	if !f.ranAny("/usr/local/lib/sandbox/install-user-file.sh") || !f.ranAny(".ssh/authorized_keys 0600") {
		t.Errorf("expected a relay install of .ssh/authorized_keys with mode 0600, calls=%q", f.calls)
	}
}
