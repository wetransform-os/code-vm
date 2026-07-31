package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

	tmp, err := os.CreateTemp("", "code-vm-gitconfig-*")
	if err != nil {
		return fmt.Errorf("create temp gitconfig: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(GitConfigContent(name, email)); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp gitconfig: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp gitconfig: %w", err)
	}

	staged := "/tmp/code-vm-gitconfig"
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return err
	}
	dst := "/home/" + d.AgentUser + "/.gitconfig"
	if err := d.Client.Admin(ctx, []string{"install", "-m", "0644", "-o", d.AgentUser, "-g", d.AgentUser, staged, dst}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"rm", "-f", staged})
}
