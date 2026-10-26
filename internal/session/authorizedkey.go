package session

import (
	"context"
	"strings"
)

// AuthorizedKeysRel is the agent user's authorized_keys, relative to its home.
const AuthorizedKeysRel = ".ssh/authorized_keys"

// PushAuthorizedKey installs line as the agent user's only authorized key. It
// goes through the identity relay as file content, never via a shell string.
func PushAuthorizedKey(ctx context.Context, d Deps, line string) error {
	return PushUserFile(ctx, d, []byte(line+"\n"), AuthorizedKeysRel, "0600")
}

// AuthorizedKeyInstalled reports whether the guest's authorized_keys holds
// exactly line and nothing else.
func AuthorizedKeyInstalled(ctx context.Context, d Deps, line string) bool {
	path := "/home/" + d.AgentUser + "/" + AuthorizedKeysRel
	return strings.TrimSpace(string(d.Client.ReadFile(ctx, path))) == line
}
