// Package agentssh owns the host-side identity that lets an IDE SSH into the
// guest as the agent user.
//
// The keypair lives under the config directory, one directory per instance:
// <config dir>/ssh/<instance>/agent_ed25519, agent_ed25519.pub and
// known_hosts. Only the public line is ever pushed into the guest; the
// private key never enters the guest or the Lima template, and the directory
// is kept out of every shared mount (config.SSHDirFor with
// Config.MountsExcludeTree). An agent that could read the private key could
// SSH back in from outside the sandbox with whatever it can reach.
//
// The directory is 0700 and the private key 0600 because ssh refuses a
// group- or world-readable identity, and because other host users must not be
// able to read it. Both are re-applied on every Ensure, so a loosened mode
// heals instead of surfacing later as an opaque ssh failure.
//
// Half a keypair is refused rather than repaired: regenerating would silently
// change the identity the guest already trusts, and a lone .pub cannot be
// proven to match a private key that no longer exists. Deleting the
// directory is the explicit, user-visible way to rotate.
package agentssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PrivateKeyName is the file name of the private key inside an instance directory.
const PrivateKeyName = "agent_ed25519"

// Key describes an ensured keypair on the host.
type Key struct {
	PrivatePath, PublicPath, KnownHostsPath string
	// AuthorizedLine is the one line that goes into authorized_keys, no newline.
	AuthorizedLine string
}

// Keygen creates an unencrypted ed25519 keypair at privatePath (and
// privatePath+".pub"). It is a variable so unit tests need no ssh-keygen.
var Keygen = func(ctx context.Context, privatePath, comment string) error {
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", privatePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// InstanceDir returns the directory holding one instance's key material.
func InstanceDir(sshDir, instance string) string {
	return filepath.Join(sshDir, instance)
}

// Ensure makes dir hold a usable keypair, generating one only when neither
// half exists, and tightens permissions on every call.
func Ensure(ctx context.Context, dir, instance string) (Key, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("create %s: %w", dir, err)
	}
	// The mount guard canonicalizes <config dir>/ssh but not the instance
	// directory below it, so a symlink here could redirect the private key
	// into a shared tree. Refuse it instead of following it.
	if err := refuseSymlink(dir); err != nil {
		return Key{}, err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("chmod %s: %w", dir, err)
	}
	// Serialize the check-generate-read sequence across processes: a
	// concurrent first run would otherwise catch ssh-keygen between its two
	// files and be told to delete a keypair that is being generated.
	unlock, err := lockDir(dir)
	if err != nil {
		return Key{}, err
	}
	defer unlock()
	k := Key{
		PrivatePath:    filepath.Join(dir, PrivateKeyName),
		PublicPath:     filepath.Join(dir, PrivateKeyName+".pub"),
		KnownHostsPath: filepath.Join(dir, "known_hosts"),
	}
	hasPriv, err := exists(k.PrivatePath)
	if err != nil {
		return Key{}, err
	}
	hasPub, err := exists(k.PublicPath)
	if err != nil {
		return Key{}, err
	}
	switch {
	case !hasPriv && !hasPub:
		if err := Keygen(ctx, k.PrivatePath, "code-vm "+instance+" agent"); err != nil {
			return Key{}, err
		}
	case !hasPriv:
		return Key{}, halfKeypair(dir, PrivateKeyName)
	case !hasPub:
		return Key{}, halfKeypair(dir, PrivateKeyName+".pub")
	}
	if err := os.Chmod(k.PrivatePath, 0o600); err != nil {
		return Key{}, fmt.Errorf("chmod %s: %w", k.PrivatePath, err)
	}
	pub, err := os.ReadFile(k.PublicPath)
	if err != nil {
		return Key{}, fmt.Errorf("read %s: %w", k.PublicPath, err)
	}
	line := strings.TrimSpace(string(pub))
	// Written into authorized_keys verbatim, so anything but one ed25519 line
	// could smuggle in extra keys or options.
	if strings.Contains(line, "\n") || !strings.HasPrefix(line, "ssh-ed25519 ") {
		return Key{}, fmt.Errorf("%s is not a single ed25519 public key line", k.PublicPath)
	}
	k.AuthorizedLine = line
	return k, nil
}

func halfKeypair(dir, missing string) error {
	return fmt.Errorf("%s holds half a keypair (%s is missing); delete the directory to generate a fresh one", dir, missing)
}

// exists reports whether path is present; a symlink counts as an error, not
// a key, for the same reason the instance directory refuses one.
func exists(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return false, symlinkError(path)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

func refuseSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return symlinkError(path)
	}
	return nil
}

func symlinkError(path string) error {
	return fmt.Errorf("%s is a symlink; the agent SSH key material must be a real file under the config directory", path)
}
