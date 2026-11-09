package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/wetransform/code-vm/internal/agentssh"
	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/profile"
	"github.com/wetransform/code-vm/internal/session"
)

// prepareAgentAccess readies a freshly booted guest for sessions that start
// over SSH rather than through `code-vm`: the git identity, which the default
// command otherwise applies per invocation, the rendered profile templates,
// and the agent's SSH key. Without the identity, a VM booted with `code-vm
// start` and only ever used from Orca would make unattributed commits.
//
// Order matters and mirrors the default command (session.Setup, then
// pushRendered): the identity is a minimal [user] block, so a profile that
// ships its own .gitconfig template must land after it and win.
func prepareAgentAccess(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile, rendered []profile.Rendered, cfgPath string, out io.Writer) error {
	if err := session.ApplyGitIdentity(ctx, agentDeps(cl, c, profiles)); err != nil {
		return err
	}
	if err := pushRendered(ctx, cl, c, profiles, rendered, out); err != nil {
		return err
	}
	return ensureAgentKey(ctx, cl, c, cfgPath, out)
}

// agentKey returns the agent user's SSH identity for this config, generating it on first use.
func agentKey(ctx context.Context, c config.Config, cfgPath string) (agentssh.Key, error) {
	return agentssh.Ensure(ctx, agentssh.InstanceDir(config.SSHDirFor(cfgPath), c.Instance), c.Instance)
}

// ensureAgentKey makes sure the agent's keypair exists on the host and its
// public half is the guest's only authorized key. A host without ssh-keygen
// only gets a warning; every other failure aborts the boot.
func ensureAgentKey(ctx context.Context, cl lima.Client, c config.Config, cfgPath string, out io.Writer) error {
	key, err := agentKey(ctx, c, cfgPath)
	if errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintln(out, "warning: ssh-keygen not found on the host; skipping the agent SSH key (code-vm ssh-target needs it)")
		return nil
	}
	if err != nil {
		return err
	}
	if err := session.PushAuthorizedKey(ctx, agentDeps(cl, c, nil), key.AuthorizedLine); err != nil {
		return err
	}
	pinHostKey(ctx, cl, c, key, []int{c.SSHPort, liveSSHPort(ctx, cl)}, out)
	return nil
}

// pinHostKey rewrites the known_hosts file code-vm hands to Orca and the
// ssh_config snippet from the guest's current host keys, for 127.0.0.1 on
// each of ports (zero and duplicates are ignored).
//
// The guest's host key changes on recreate, and changed once on the first
// boot after the upgrade that started preserving it. A client that still
// pins the old key fails verification and disconnects before authenticating;
// under OpenSSH's per-source penalties enough of those from one address
// (every host-side client shares the Lima gateway's) refuse all of them with
// "Not allowed at this time". Keeping the pin current removes the trigger.
//
// The pin is a convenience for clients, not a control, so every failure here
// is a warning: a boot or an ssh-target must not fail because the guest could
// not answer a cat.
func pinHostKey(ctx context.Context, cl lima.Client, c config.Config, key agentssh.Key, ports []int, out io.Writer) {
	keys, err := session.HostKeys(ctx, agentDeps(cl, c, nil))
	if err != nil {
		fmt.Fprintf(out, "warning: could not pin the guest's SSH host key: %v\n", err)
		return
	}
	content := agentssh.KnownHostsContent(ports, keys)
	if content == "" {
		// No port known: nothing a client could match against.
		return
	}
	changed, err := agentssh.WriteKnownHosts(key.KnownHostsPath, content, keys)
	if err != nil {
		fmt.Fprintf(out, "warning: could not pin the guest's SSH host key: %v\n", err)
		return
	}
	if changed {
		fmt.Fprintf(out, "note: the guest's SSH host key changed; %s now trusts the new one\n", key.KnownHostsPath)
	}
}
