package session

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// stagedCleanupTimeout bounds the best-effort removal of a staged file so it
// cannot hang the process: it runs even when the caller's own ctx is
// cancelled or already past its deadline (see cleanupStaged).
const stagedCleanupTimeout = 10 * time.Second

// relCharsetRe mirrors the charset profile.relPathRe enforces on the host
// side: conservative on purpose, since rel is written into a root-run
// install command on the other end of the relay. Defense in depth — every
// current caller already passes a host-validated rel — for future ones.
var relCharsetRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// PushUserFile delivers content into the agent's home at rel with the given
// mode, without ever writing there as root. The staged copy is relayed by
// install-user-file.sh: root moves it to an agent-group-readable tmpfs drop,
// and an agent-identity install places it — a symlink the agent plants can
// only redirect a write the agent could already make (the same posture as
// profile file installs). Used for rendered templates (0600) and the git
// identity (0644); rel comes from host-validated input only.
func PushUserFile(ctx context.Context, d Deps, content []byte, rel, mode string) error {
	// Defense in depth: every current caller passes a host-validated rel, but
	// this guards future ones from ever reaching the guest with a path that
	// escapes the agent home or carries a shell metacharacter into the
	// relay's positional args.
	if rel == "" || path.IsAbs(rel) || hasDotDotSegment(rel) || !relCharsetRe.MatchString(rel) {
		return fmt.Errorf("user file path %q: must be a clean relative path inside the agent home", rel)
	}
	staged, err := stageFile(ctx, d, content)
	if err != nil {
		return err
	}
	// Best-effort cleanup on every return path, not just failure: the relay
	// script itself removes the staged copy on success (see
	// install-user-file.sh), so this is a harmless double-delete then. It is
	// load-bearing on failure — notably the supported old-VM/missing-script
	// case below — where the relay never touches the staged copy at all,
	// which would otherwise leave a rendered credential sitting in the
	// admin-only staging dir indefinitely.
	defer cleanupStaged(d, staged)
	if err := d.Client.Admin(ctx, []string{
		"/usr/local/lib/sandbox/install-user-file.sh", staged, rel, mode,
	}); err != nil {
		// A VM booted from a pre-this-feature code-vm binary lacks
		// install-user-file.sh entirely, so this failure is otherwise an
		// opaque exec error with no clue that the fix is a restart.
		return fmt.Errorf("install %s: %w (the relay script may be missing because the VM predates this "+
			"code-vm version; restart it with `code-vm stop && code-vm start`)", rel, err)
	}
	return nil
}

// cleanupStaged best-effort removes a staged file via the admin channel. It
// uses an independent, bounded context rather than the caller's ctx so it
// still runs when that ctx is cancelled or already past its deadline —
// exactly the case a failed Admin call above may have left it in. The error
// is intentionally discarded: this is cleanup of a temporary drop, not a
// step whose failure should mask (or be conflated with) the actual result of
// PushUserFile.
func cleanupStaged(d Deps, staged string) {
	ctx, cancel := context.WithTimeout(context.Background(), stagedCleanupTimeout)
	defer cancel()
	_ = d.Client.Admin(ctx, []string{"rm", "-f", staged})
}

// hasDotDotSegment reports whether rel contains a literal ".." path segment.
func hasDotDotSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
