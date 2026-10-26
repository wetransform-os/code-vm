package agentssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// fakeKeygen writes a deterministic pair so tests need no ssh-keygen.
func fakeKeygen(t *testing.T) {
	t.Helper()
	old := Keygen
	Keygen = func(_ context.Context, path, comment string) error {
		if err := os.WriteFile(path, []byte("PRIVATE\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAATEST "+comment+"\n"), 0o644)
	}
	t.Cleanup(func() { Keygen = old })
}

func TestEnsureGeneratesOnceAndReportsPaths(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	k, err := Ensure(context.Background(), dir, "code-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if k.PrivatePath != filepath.Join(dir, "agent_ed25519") || k.PublicPath != k.PrivatePath+".pub" || k.KnownHostsPath != filepath.Join(dir, "known_hosts") {
		t.Errorf("paths = %+v", k)
	}
	if k.AuthorizedLine != "ssh-ed25519 AAAATEST code-vm code-sandbox agent" {
		t.Errorf("AuthorizedLine = %q", k.AuthorizedLine)
	}
	Keygen = func(context.Context, string, string) error {
		t.Fatal("Keygen called again for an existing key")
		return nil
	}
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureTightensPermissions(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "agent_ed25519"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode = %o, want 700", m)
	}
	if m := mode(t, filepath.Join(dir, "agent_ed25519")); m != 0o600 {
		t.Errorf("key mode = %o, want 600", m)
	}
}

func TestEnsureRefusesHalfKeypair(t *testing.T) {
	fakeKeygen(t)
	for _, remove := range []string{"agent_ed25519", "agent_ed25519.pub"} {
		dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
		if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, remove))
		_, err := Ensure(context.Background(), dir, "code-sandbox")
		if err == nil || !strings.Contains(err.Error(), dir) {
			t.Errorf("after removing %s: err = %v, want an error naming %s", remove, err, dir)
		}
	}
}

func TestEnsureRejectsForeignPublicKey(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent_ed25519.pub"), []byte("ssh-rsa AAAA x\nssh-ed25519 BBBB y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err == nil {
		t.Error("a multi-line or non-ed25519 .pub must be refused, not pushed into the guest")
	}
}

func TestEnsureWithRealSSHKeygen(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH")
	}
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	k, err := Ensure(context.Background(), dir, "code-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.AuthorizedLine, "ssh-ed25519 ") || strings.Contains(k.AuthorizedLine, "\n") {
		t.Errorf("AuthorizedLine = %q", k.AuthorizedLine)
	}
}

func TestEnsureRefusesASymlinkedInstanceDir(t *testing.T) {
	fakeKeygen(t)
	root := t.TempDir()
	target := filepath.Join(root, "shared")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "ssh", "code-sandbox")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(context.Background(), dir, "code-sandbox")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected a symlink refusal, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, PrivateKeyName)); !os.IsNotExist(statErr) {
		t.Errorf("no key may be written through the symlink: %v", statErr)
	}
}

func TestEnsureRefusesASymlinkedKeyFile(t *testing.T) {
	fakeKeygen(t)
	root := t.TempDir()
	dir := filepath.Join(root, "ssh", "code-sandbox")
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	priv := filepath.Join(dir, PrivateKeyName)
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Rename(priv, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, priv); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(context.Background(), dir, "code-sandbox")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected a symlink refusal, got %v", err)
	}
}
