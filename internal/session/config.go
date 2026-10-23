package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// PushConfig replaces the canonical config tree in a running VM with files
// and re-runs lock-settings.sh, so the agent's locked settings match what
// this binary ships — the live counterpart of what Lima's data provisioning
// plus the boot sequence do on every start. lockScript is this binary's
// lock-settings.sh; the guest's copy must match it before anything is
// touched, since that script, not the files, is what runs against the live
// agent.
//
// Only paths under guest.ConfigRoot are accepted: the tree is replaced
// wholesale (a file dropped from the binary must not survive and get
// re-locked into the home), and that is only safe for a directory this
// function owns outright. The replacement is built beside the live tree and
// swapped in only once every file has landed, so a failed or interrupted
// push leaves the last good tree in place rather than an empty one that
// the lock step would then faithfully apply. Content travels through the
// admin staging path so the agent never gets a window to tamper with what
// root is about to lock.
func PushConfig(ctx context.Context, d Deps, files []guest.DataFile, lockScript guest.DataFile) error {
	for _, f := range files {
		if !strings.HasPrefix(f.Path, guest.ConfigRoot+"/") {
			return fmt.Errorf("refusing to push %s: outside the config tree %s", f.Path, guest.ConfigRoot)
		}
	}
	if err := verifyGuestScript(ctx, d, lockScript); err != nil {
		return err
	}
	next, prev := guest.ConfigRoot+".new", guest.ConfigRoot+".old"
	if err := d.Client.Admin(ctx, []string{"rm", "-rf", next}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-d", "-m", "0755", next}); err != nil {
		return err
	}
	for _, f := range files {
		dst := next + strings.TrimPrefix(f.Path, guest.ConfigRoot)
		if err := installContent(ctx, d, []byte(f.Content), dst, f.Permissions, "root", "root"); err != nil {
			return err
		}
	}
	// Swap. An absent live tree (a push interrupted right here once before)
	// is given an empty stand-in so the rename has something to move aside.
	// Between the two renames the canonical path is briefly missing; the
	// lock step refuses to run without it, and a boot re-provisions it.
	for _, cmd := range [][]string{
		{"rm", "-rf", prev},
		{"install", "-d", "-m", "0755", guest.ConfigRoot},
		{"mv", "-T", guest.ConfigRoot, prev},
		{"mv", "-T", next, guest.ConfigRoot},
		{"rm", "-rf", prev},
	} {
		if err := d.Client.Admin(ctx, cmd); err != nil {
			return err
		}
	}
	return d.Client.Admin(ctx, []string{LockSettingsScript})
}
