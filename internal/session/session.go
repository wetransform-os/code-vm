package session

import "context"

// Setup performs every privileged per-invocation step.
//
// Nothing here reads anything out of the workspace. That is deliberate: the
// workspace is mounted writable and is exactly what the agent edits, so a file
// there is agent-authored input. Both mechanisms that used to read one — the
// per-project allowlist and credential injection — were removed for that
// reason.
func Setup(ctx context.Context, d Deps) error {
	if err := ApplyAllowlist(ctx, d); err != nil {
		return err
	}
	return ApplyGitIdentity(ctx, d)
}
