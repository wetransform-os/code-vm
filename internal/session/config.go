package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/wetransform/code-vm/internal/guest"
)

// lockSettingsScript is the boot-time step that copies the canonical config
// into the agent's home and locks it. Re-running it is what makes a pushed
// config take effect without a reboot.
const lockSettingsScript = "/usr/local/lib/sandbox/lock-settings.sh"

// PushConfig replaces the canonical config tree in a running VM with files
// and re-runs lock-settings.sh, so the agent's locked settings match what
// this binary ships — the live counterpart of what Lima's data provisioning
// plus the boot sequence do on every start.
//
// Only paths under guest.ConfigRoot are accepted: the tree is removed and
// recreated wholesale (a file dropped from the binary must not survive and
// get re-locked into the home), and that is only safe for a directory this
// function owns outright. Content travels through the admin staging path so
// the agent never gets a window to tamper with what root is about to lock.
func PushConfig(ctx context.Context, d Deps, files []guest.DataFile) error {
	for _, f := range files {
		if !strings.HasPrefix(f.Path, guest.ConfigRoot+"/") {
			return fmt.Errorf("refusing to push %s: outside the config tree %s", f.Path, guest.ConfigRoot)
		}
	}
	if err := d.Client.Admin(ctx, []string{"rm", "-rf", guest.ConfigRoot}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-d", "-m", "0755", guest.ConfigRoot}); err != nil {
		return err
	}
	for _, f := range files {
		if err := installContent(ctx, d, []byte(f.Content), f.Path, f.Permissions, "root", "root"); err != nil {
			return err
		}
	}
	return d.Client.Admin(ctx, []string{lockSettingsScript})
}
