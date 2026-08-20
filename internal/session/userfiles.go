package session

import (
	"context"
	"fmt"
)

// PushUserFile delivers content into the agent's home at rel with the given
// mode, without ever writing there as root. The staged copy is relayed by
// install-user-file.sh: root moves it to an agent-group-readable tmpfs drop,
// and an agent-identity install places it — a symlink the agent plants can
// only redirect a write the agent could already make (the same posture as
// profile file installs). Used for rendered templates (0600) and the git
// identity (0644); rel comes from host-validated input only.
func PushUserFile(ctx context.Context, d Deps, content []byte, rel, mode string) error {
	staged, err := stageFile(ctx, d, content)
	if err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{
		"/usr/local/lib/sandbox/install-user-file.sh", staged, rel, mode,
	}); err != nil {
		return fmt.Errorf("install %s: %w", rel, err)
	}
	return nil
}
