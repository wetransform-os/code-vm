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
	"github.com/wetransform/code-vm/internal/session"
)

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
	return session.PushAuthorizedKey(ctx, agentDeps(cl, c, nil), key.AuthorizedLine)
}
