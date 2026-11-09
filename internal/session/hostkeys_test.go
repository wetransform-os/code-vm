package session

import (
	"context"
	"strings"
	"testing"
)

func TestHostKeysReadsTheGuestHostKeysAsRoot(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{
		"/etc/ssh/ssh_host_": []byte("ssh-ed25519 AAAAHOST root@lima\nssh-rsa AAAARSA root@lima\n"),
	}}
	keys, err := HostKeys(context.Background(), testDeps(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, "|") != "ssh-ed25519 AAAAHOST|ssh-rsa AAAARSA" {
		t.Errorf("keys = %q", keys)
	}
	if !f.ranAny("sudo") || !f.ranAny("/etc/ssh/ssh_host_") {
		t.Errorf("host keys must be read as root from /etc/ssh, calls=%q", f.calls)
	}
}

func TestHostKeysFailsOnNothingUsable(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{"/etc/ssh/ssh_host_": []byte("")}}
	if _, err := HostKeys(context.Background(), testDeps(t, f)); err == nil {
		t.Error("an empty answer must be an error, not an empty trust file")
	}
}
