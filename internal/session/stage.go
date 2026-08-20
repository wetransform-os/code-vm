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

// stageFile writes content to a local temp file and copies it into the
// guest's admin-only staging directory, returning the staged guest path.
// Shared by installContent (root destinations) and PushUserFile (agent-home
// destinations, which relay the staged copy onward instead of root-installing
// it directly — see userfiles.go).
func stageFile(ctx context.Context, d Deps, content []byte) (string, error) {
	tmp, err := os.CreateTemp("", "code-vm-stage-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	staged, err := stagedPath()
	if err != nil {
		return "", err
	}
	if err := d.Client.Admin(ctx, []string{
		"install", "-d", "-m", "0700", "-o", adminUser, "-g", adminUser, stageDir,
	}); err != nil {
		return "", err
	}
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return "", err
	}
	return staged, nil
}

// installContent writes content into the guest at dst, owned by owner:group
// with the given mode, as root. Only correct for root-owned destinations —
// the allowlist fragment and profile tree — where there is no agent-owned
// home path for a planted symlink to redirect the write into; for anything
// landing in the agent's home, use PushUserFile instead so root never writes
// there directly.
func installContent(ctx context.Context, d Deps, content []byte, dst, mode, owner, group string) error {
	staged, err := stageFile(ctx, d, content)
	if err != nil {
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
