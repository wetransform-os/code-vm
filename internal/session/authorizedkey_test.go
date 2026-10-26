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

func TestAuthorizedKeyInstalledComparesTrimmedContent(t *testing.T) {
	line := "ssh-ed25519 AAAA code-vm code-sandbox agent"
	for _, tc := range []struct {
		guest string
		want  bool
	}{{line + "\n", true}, {line, true}, {"", false}, {line + "\nssh-rsa BBBB planted\n", false}} {
		f := &fakeRunner{out: map[string][]byte{"authorized_keys": []byte(tc.guest)}} // what ReadFile returns
		d := testDeps(t, f)
		if got := AuthorizedKeyInstalled(context.Background(), d, line); got != tc.want {
			t.Errorf("guest %q: got %v, want %v", tc.guest, got, tc.want)
		}
	}
}
