package session

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ExecHost runs a command on the host and returns its stdout.
func ExecHost(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// GitConfigContent renders a minimal gitconfig. Empty fields are omitted so
// git falls back to its own resolution rather than seeing a blank identity.
func GitConfigContent(name, email string) string {
	var b strings.Builder
	b.WriteString("# Written by code-vm from the host's git config.\n[user]\n")
	if name != "" {
		fmt.Fprintf(&b, "\tname = %s\n", name)
	}
	if email != "" {
		fmt.Fprintf(&b, "\temail = %s\n", email)
	}
	return b.String()
}

// ApplyGitIdentity copies the host's git identity into the guest so commits
// made by the agent are attributed correctly. A host with no identity
// configured is not an error.
func ApplyGitIdentity(ctx context.Context, d Deps) error {
	host := d.Host
	if host == nil {
		host = ExecHost
	}
	get := func(key string) string {
		out, err := host(ctx, "git", "config", "--get", key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	name, email := get("user.name"), get("user.email")
	if name == "" && email == "" {
		return nil
	}

	// Numeric ids, not names: the guest group carrying AgentGID may be a stock
	// group with a different name (see Deps).
	dst := "/home/" + d.AgentUser + "/.gitconfig"
	return installContent(ctx, d, []byte(GitConfigContent(name, email)), dst, "0644",
		strconv.Itoa(d.AgentUID), strconv.Itoa(d.AgentGID))
}
