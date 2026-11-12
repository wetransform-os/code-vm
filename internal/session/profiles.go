package session

import (
	"context"
	"fmt"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

// PushProfiles replaces the guest's profile tree with the given rendering.
// The old tree is removed first so a deactivated profile's content actually
// disappears — mode:data delivery on the next boot can only overwrite, which
// is why the applier additionally keys off manifest.env and files.list.
// Content travels through the admin staging path: the agent can read the
// installed result (it is destined for its own home anyway) but never gets a
// window to tamper with what feeds a root-driven apply.
func PushProfiles(ctx context.Context, d Deps, files []guest.DataFile) error {
	if err := d.Client.Admin(ctx, []string{"rm", "-rf", profile.GuestRoot}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-d", "-m", "0755", profile.GuestRoot}); err != nil {
		return err
	}
	for _, f := range files {
		if err := installContent(ctx, d, []byte(f.Content), f.Path, f.Permissions, "root", "root"); err != nil {
			return err
		}
	}
	return nil
}

// ApplierScript is the guest path of the profile applier, the boot-time step
// that installs profile content into the agent's home. Re-running it is what
// makes a pushed profile tree take effect without a reboot.
const ApplierScript = "/usr/local/lib/sandbox/apply-profiles.sh"

// ApplyProfiles brings the guest's applier up to this build and runs it in
// strict mode: on the explicit `profile apply` path the user is watching, so
// a hook failure is an error — unlike at boot, where it only warns. applier
// is this binary's apply-profiles.sh.
//
// Unlike lock-settings.sh, which PushConfig merely verifies and refuses on a
// mismatch, the applier is reinstalled when it differs: it is the one script
// whose behaviour a code-vm upgrade changes on the apply path (a new step
// such as sources), and refusing would send the user to a restart for a
// change whose inputs the pushed tree already carries. That is only sound
// because the applier is self-contained against inputs the apply path
// controls or that never change for a booted VM: manifest.env and the
// profile tree (just pushed), provision.env (fixed at boot) and the base
// image's tools. An applier that comes to depend on a newer boot script
// must switch this back to verify-and-refuse.
//
// "Up to this build" is literal: the comparison is a hash, so a guest
// applier from a newer code-vm is replaced too. Nothing in the guest records
// which build delivered a script, and the binary running the apply is the
// one whose expectations the applier has to meet.
//
// The caller gates on RequireBootComplete before its first guest write;
// sandbox-boot.sh execs the applier itself, and Lima reports Running long
// before that. The replacement lands root-owned through the admin staging
// path so the agent never gets a window to substitute it, and goes live by
// a single rename so the canonical path is never absent or half-written.
func ApplyProfiles(ctx context.Context, d Deps, applier guest.DataFile) error {
	if applier.Path != ApplierScript {
		return fmt.Errorf("refusing to run %s as the profile applier; expected %s", applier.Path, ApplierScript)
	}
	same, err := guestScriptMatches(ctx, d, applier)
	if err != nil {
		return err
	}
	if !same {
		if err := replaceGuestScript(ctx, d, applier); err != nil {
			return fmt.Errorf("reinstall %s: %w", applier.Path, err)
		}
	}
	return d.Client.Admin(ctx, []string{
		"env", "SANDBOX_PROFILES_STRICT=1", ApplierScript,
	})
}

// scriptSwapScript installs a staged script beside its live path and renames
// it over, as one guest command. $1 is the live path, $2 the mode, $3 the
// staged copy, $4 the sibling. Siblings left by an earlier interrupted swap
// are swept first: /usr/local/lib persists across reboots, unlike the
// staging directory. The rename is a single atomic replace on the same
// filesystem, so a concurrent exec sees either the old script or the new.
const scriptSwapScript = `set -e
rm -f "$1".new-*
install -m "$2" -o root -g root "$3" "$4"
mv -T "$4" "$1"
rm -f "$3"`

// replaceGuestScript brings script's guest path to this binary's content
// via the staging path and scriptSwapScript.
func replaceGuestScript(ctx context.Context, d Deps, script guest.DataFile) error {
	staged, err := stageFile(ctx, d, []byte(script.Content))
	if err != nil {
		return err
	}
	suffix, err := randomSuffix(8)
	if err != nil {
		cleanupStaged(d, staged)
		return err
	}
	sibling := script.Path + ".new-" + suffix
	if err := d.Client.Admin(ctx, []string{
		"sh", "-c", scriptSwapScript, "swap", script.Path, script.Permissions, staged, sibling,
	}); err != nil {
		cleanupStaged(d, sibling)
		cleanupStaged(d, staged)
		return err
	}
	return nil
}
