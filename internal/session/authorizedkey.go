package session

import "context"

// AuthorizedKeysRel is the agent user's authorized_keys, relative to its home.
const AuthorizedKeysRel = ".ssh/authorized_keys"

// PushAuthorizedKey installs line as the agent user's only authorized key. It
// goes through the identity relay as file content, never via a shell string.
func PushAuthorizedKey(ctx context.Context, d Deps, line string) error {
	return PushUserFile(ctx, d, []byte(line+"\n"), AuthorizedKeysRel, "0600")
}
