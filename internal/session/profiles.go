package session

import (
	"context"

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

// ApplyProfiles runs the guest applier in strict mode: on the explicit
// `profile apply` path the user is watching, so a hook failure is an error —
// unlike at boot, where it only warns.
func ApplyProfiles(ctx context.Context, d Deps) error {
	return d.Client.Admin(ctx, []string{
		"env", "SANDBOX_PROFILES_STRICT=1", "/usr/local/lib/sandbox/apply-profiles.sh",
	})
}
