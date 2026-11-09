package agentssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseHostKeysKeepsTypeAndKeyOnly(t *testing.T) {
	raw := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbc root@lima-code-sandbox\n" +
		"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBAbc root@lima\n" +
		"\n" +
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDabc\n"
	keys, err := ParseHostKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbc",
		"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBAbc",
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDabc",
	}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", keys, want)
	}
}

// The lines land in a file ssh reads as known_hosts, so anything that is not
// a bare "<type> <base64>" could smuggle in a marker (@cert-authority), a
// wildcard host pattern or a second entry.
func TestParseHostKeysRejectsAnythingButBareKeys(t *testing.T) {
	for _, raw := range []string{
		"",
		"\n\n",
		"@cert-authority * ssh-ed25519 AAAA",
		"ssh-ed25519",
		"ssh-ed25519 AAAA;rm -rf /",
		"ssh-dss AAAAB3NzaC1kc3MAAACBAP",
		"ssh-ed25519 AAAA\n* ssh-ed25519 BBBB",
		"ssh-ed25519 AAAA\nhost.example ssh-ed25519 BBBB",
	} {
		if _, err := ParseHostKeys(raw); err == nil {
			t.Errorf("ParseHostKeys(%q) accepted", raw)
		}
	}
}

// One key of a type this build does not pin (DSA from an old image, a type
// newer than the list) must not cost the pin of the others.
func TestParseHostKeysSkipsUnsupportedTypes(t *testing.T) {
	keys, err := ParseHostKeys("ssh-dss AAAAB3NzaC1kc3MAAACBAP root@host\nssh-ed25519 AAAA root@host\nssh-pq-future@openssh.com BBBB\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "ssh-ed25519 AAAA" {
		t.Errorf("keys = %q, want only the ed25519 key", keys)
	}
}

func TestKnownHostsContentCoversEveryPortAndKey(t *testing.T) {
	got := KnownHostsContent([]int{60022, 0, 38421, 60022}, []string{"ssh-ed25519 AAAA", "ssh-rsa BBBB"})
	want := "[127.0.0.1]:60022 ssh-ed25519 AAAA\n" +
		"[127.0.0.1]:60022 ssh-rsa BBBB\n" +
		"[127.0.0.1]:38421 ssh-ed25519 AAAA\n" +
		"[127.0.0.1]:38421 ssh-rsa BBBB\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if KnownHostsContent(nil, []string{"ssh-ed25519 AAAA"}) != "" || KnownHostsContent([]int{1}, nil) != "" {
		t.Error("no ports or no keys must yield no content")
	}
}

func TestWriteKnownHostsReportsChangesAndIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	keyA := []string{"ssh-ed25519 AAAA"}
	changed, err := WriteKnownHosts(path, "[127.0.0.1]:60022 ssh-ed25519 AAAA\n", keyA)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a first write is not a change of trust")
	}
	changed, err = WriteKnownHosts(path, "[127.0.0.1]:60022 ssh-ed25519 AAAA\n", keyA)
	if err != nil || changed {
		t.Errorf("rewriting identical content: changed=%v err=%v", changed, err)
	}
	changed, err = WriteKnownHosts(path, "[127.0.0.1]:60022 ssh-ed25519 BBBB\n", []string{"ssh-ed25519 BBBB"})
	if err != nil || !changed {
		t.Errorf("a different key must report a change: changed=%v err=%v", changed, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "[127.0.0.1]:60022 ssh-ed25519 BBBB\n" {
		t.Errorf("content after rewrite: %q err=%v", b, err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestWriteKnownHostsRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks not supported here")
	}
	if _, err := WriteKnownHosts(path, "[127.0.0.1]:1 ssh-ed25519 AAAA\n", []string{"ssh-ed25519 AAAA"}); err == nil {
		t.Error("writing through a symlink must be refused")
	}
	if b, _ := os.ReadFile(target); string(b) != "x\n" {
		t.Error("the symlink target was modified")
	}
}

func TestWriteKnownHostsRefusesADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Skip("symlinks not supported here")
	}
	if _, err := WriteKnownHosts(path, "[127.0.0.1]:1 ssh-ed25519 AAAA\n", []string{"ssh-ed25519 AAAA"}); err == nil {
		t.Error("a dangling symlink must be refused, not replaced")
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: fi=%v err=%v", fi, err)
	}
}

func TestWriteKnownHostsRefusesASymlinkEvenWhenContentMatches(t *testing.T) {
	dir := t.TempDir()
	content := "[127.0.0.1]:1 ssh-ed25519 AAAA\n"
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks not supported here")
	}
	if _, err := WriteKnownHosts(path, content, []string{"ssh-ed25519 AAAA"}); err == nil {
		t.Error("a symlink must be refused even when its target already matches")
	}
}

// The note about a changed key is for a changed key. The same key under a
// port Lima picked afresh, hashed by a client, or next to a comment is the
// same trust, and a file without keys never trusted anything.
func TestWriteKnownHostsReportsOnlyKeyChanges(t *testing.T) {
	keyA := []string{"ssh-ed25519 AAAA"}
	for _, tc := range []struct {
		name, old string
		changed   bool
	}{
		{"new port", "[127.0.0.1]:60122 ssh-ed25519 AAAA\n", false},
		{"hashed by a client", "|1|c2FsdA==|aGFzaA== ssh-ed25519 AAAA\n", false},
		{"comment and blank line", "# pinned\n\n[127.0.0.1]:60022 ssh-ed25519 AAAA\n", false},
		{"no keys", "# nothing here\n", false},
		{"other key", "[127.0.0.1]:60022 ssh-ed25519 BBBB\n", true},
		{"extra key", "[127.0.0.1]:60022 ssh-ed25519 AAAA\n[127.0.0.1]:60022 ssh-rsa CCCC\n", true},
	} {
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(path, []byte(tc.old), 0o600); err != nil {
			t.Fatal(err)
		}
		changed, err := WriteKnownHosts(path, "[127.0.0.1]:60022 ssh-ed25519 AAAA\n", keyA)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if changed != tc.changed {
			t.Errorf("%s: changed = %v, want %v", tc.name, changed, tc.changed)
		}
	}
}
