package session

import "context"

// Setup performs every privileged per-invocation step, in order. Credential
// rendering is added in Task 8 and must run after ApplyAllowlist, because
// lock-settings.sh consumes the credential deny rules.
func Setup(ctx context.Context, d Deps) error {
	if err := ApplyAllowlist(ctx, d); err != nil {
		return err
	}
	if err := ApplyGitIdentity(ctx, d); err != nil {
		return err
	}
	return ApplyCredentials(ctx, d)
}
