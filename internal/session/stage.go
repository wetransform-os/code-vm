package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// adminUser is Lima's own guest user, the account `limactl copy` and
// `limactl shell` authenticate as. It matches `user.name` in the Lima template.
const adminUser = "limaadmin"

// stageDir is the admin user's private staging area for files on their way
// into the guest. `limactl copy` runs as the admin user, so the destination has
// to be writable by it — which rules out a root-only directory — and 0700
// ownership keeps the agent out. A predictable path under /tmp would let the
// agent pre-create or rewrite the file in the window between the copy and the
// root install, substituting its own content.
const stageDir = "/home/" + adminUser + "/.code-vm-staging"

// stagedPath returns an unpredictable path inside the staging directory, so a
// staged file cannot be squatted even by something that knows the scheme.
func stagedPath() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate staging name: %w", err)
	}
	return stageDir + "/stage-" + hex.EncodeToString(b[:]), nil
}

// installContent writes content into the guest at dst, owned by owner:group
// with the given mode. The content travels through the admin user's staging
// directory and the staged copy is removed afterwards, so it is never readable
// at a path the agent can reach.
func installContent(ctx context.Context, d Deps, content []byte, dst, mode, owner, group string) error {
	tmp, err := os.CreateTemp("", "code-vm-stage-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	staged, err := stagedPath()
	if err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{
		"install", "-d", "-m", "0700", "-o", adminUser, "-g", adminUser, stageDir,
	}); err != nil {
		return err
	}
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return err
	}
	// -D creates root-owned parents for nested per-profile paths; a no-op for
	// existing flat destinations (fragment dir, home).
	if err := d.Client.Admin(ctx, []string{
		"install", "-D", "-m", mode, "-o", owner, "-g", group, staged, dst,
	}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"rm", "-f", staged})
}
