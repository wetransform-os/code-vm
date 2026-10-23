package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/wetransform/code-vm/internal/guest"
)

// LockSettingsScript is the boot-time step that copies the canonical config
// into the agent's home and locks it. Re-running it is what makes a pushed
// config take effect without a reboot.
const LockSettingsScript = "/usr/local/lib/sandbox/lock-settings.sh"

// verifyGuestScript checks that the guest's copy of script matches what this
// binary ships. Scripts only reach a guest through Lima's provisioning at
// boot, so a guest booted by an older binary still runs the older script —
// and this binary's expectations of it (above all, that it is safe to run
// against a live agent) would not hold.
func verifyGuestScript(ctx context.Context, d Deps, script guest.DataFile) error {
	out, err := d.Client.AdminOutput(ctx, []string{"sha256sum", script.Path})
	if err != nil {
		return fmt.Errorf("inspect %s in the guest: %w", script.Path, err)
	}
	got, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	sum := sha256.Sum256([]byte(script.Content))
	if got != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("the VM runs an older %s than this code-vm ships; restart it with `code-vm stop && code-vm start` so the boot scripts update, then retry", script.Path)
	}
	return nil
}

// configSwapLock serialises the swap-and-lock step between concurrent
// applies and the boot sequence, which takes the same lock around its own
// lock-settings.sh run. /run/lock is tmpfs, so a stale lock cannot survive
// a reboot.
const configSwapLock = "/run/lock/sandbox-config.lock"

// bootCompleteMarker is sandbox-boot.sh's final act and what the Lima
// readiness probe waits for. Lima reports Running as soon as ssh answers,
// well before that; an apply must not run against a guest whose boot is
// still provisioning the very tree it would replace.
const bootCompleteMarker = "/run/sandbox-boot-complete"

// configSwapScript exchanges the staged tree with the live one and runs the
// lock step, as one guest command under an exclusive lock. $1 is the live
// tree, $2 the staged tree, $3 the lock script. `mv --exchange` is a single
// renameat2(RENAME_EXCHANGE): the canonical path is never absent, not even
// between two renames, so an interruption at any point leaves either the
// old tree or the new one — never neither. The live tree is created first
// so a guest whose tree somehow went missing still has something to
// exchange; the exchange then hands the previous tree to the staging name,
// which is removed.
const configSwapScript = `set -e
install -d -m 0755 "$1"
mv -T --exchange "$2" "$1"
rm -rf "$2"
"$3"`

// stagingTreeName returns an unpredictable per-invocation sibling of the
// live tree, so two applies never build into the same directory.
func stagingTreeName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate staging name: %w", err)
	}
	return guest.ConfigRoot + ".new-" + hex.EncodeToString(b[:]), nil
}

// cleanupStagingTree removes an abandoned staging tree, best effort and on
// an independent context, the same way cleanupStaged treats a staged file.
func cleanupStagingTree(d Deps, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), stagedCleanupTimeout)
	defer cancel()
	_ = d.Client.Admin(ctx, []string{"rm", "-rf", dir})
}

// PushConfig replaces the canonical config tree in a running VM with files
// and re-runs lock-settings.sh, so the agent's locked settings match what
// this binary ships — the live counterpart of what Lima's data provisioning
// plus the boot sequence do on every start. lockScript is this binary's
// lock-settings.sh; the guest's copy must match it before anything is
// touched, since that script, not the files, is what runs against the live
// agent.
//
// Only clean paths under guest.ConfigRoot are accepted: the tree is replaced
// wholesale (a file dropped from the binary must not survive and get
// re-locked into the home), and that is only safe for a directory this
// function owns outright. The replacement is built in a per-invocation
// staging tree beside the live one and exchanged with it atomically once
// every file has landed, so a failed or interrupted push leaves the last
// good tree in place rather than an empty one that the lock step would then
// faithfully apply. Content travels through the admin staging path so the
// agent never gets a window to tamper with what root is about to lock.
func PushConfig(ctx context.Context, d Deps, files []guest.DataFile, lockScript guest.DataFile) error {
	for _, f := range files {
		if path.Clean(f.Path) != f.Path || !strings.HasPrefix(f.Path, guest.ConfigRoot+"/") {
			return fmt.Errorf("refusing to push %s: not a clean path inside the config tree %s", f.Path, guest.ConfigRoot)
		}
	}
	if err := verifyGuestScript(ctx, d, lockScript); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"test", "-f", bootCompleteMarker}); err != nil {
		return fmt.Errorf("the VM has not finished booting (no %s yet); wait for `code-vm start` to return, then retry", bootCompleteMarker)
	}
	stage, err := stagingTreeName()
	if err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-d", "-m", "0755", stage}); err != nil {
		return err
	}
	for _, f := range files {
		dst := stage + strings.TrimPrefix(f.Path, guest.ConfigRoot)
		if err := installContent(ctx, d, []byte(f.Content), dst, f.Permissions, "root", "root"); err != nil {
			cleanupStagingTree(d, stage)
			return err
		}
	}
	if err := d.Client.Admin(ctx, []string{
		"flock", configSwapLock, "sh", "-c", configSwapScript, "swap", guest.ConfigRoot, stage, lockScript.Path,
	}); err != nil {
		cleanupStagingTree(d, stage)
		return err
	}
	return nil
}
