package session

import (
	"context"
	"fmt"

	"github.com/wetransform/code-vm/internal/agentssh"
)

// HostKeys returns the guest sshd's public host keys, read as root from
// /etc/ssh. They are root-owned and written by the guest's own ssh-keygen, so
// unlike authorized_keys they are not agent-controlled; the admin channel
// itself is Lima's, which does not verify the guest's host key either, so
// this is as trustworthy as every other code-vm command against the guest.
// The lines are still validated before anything pins them (agentssh.ParseHostKeys).
func HostKeys(ctx context.Context, d Deps) ([]string, error) {
	out, err := d.Client.AdminOutput(ctx, []string{"sh", "-c", "cat /etc/ssh/ssh_host_*_key.pub"})
	if err != nil {
		return nil, fmt.Errorf("read the guest's SSH host keys: %w", err)
	}
	return agentssh.ParseHostKeys(string(out))
}
