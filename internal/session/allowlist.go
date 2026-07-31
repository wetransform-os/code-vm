// Package session performs the privileged per-invocation setup that must
// happen before the agent runs: allowlist fragments, git identity and
// credential injection.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

// fragmentDir mirrors init-firewall.sh. It is tmpfs-backed, so fragments do
// not survive a VM restart and cannot widen the allowlist indefinitely.
const fragmentDir = "/run/sandbox/squid-allow.d"

// HostRunner executes a command on the host. Injectable for tests.
type HostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Deps carries everything session setup needs.
type Deps struct {
	Client    lima.Client
	Config    config.Config
	Workspace string
	AgentUser string
	Host      HostRunner
}

// ReadDomains parses the workspace's .sandbox-domains file. Comments and blank
// lines are dropped, entries trimmed, duplicates removed in first-seen order.
// A missing file yields no domains and no error.
func ReadDomains(workspace string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(workspace, ".sandbox-domains"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .sandbox-domains: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		d := strings.TrimSpace(line)
		if d == "" || strings.HasPrefix(d, "#") || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// FragmentName returns the per-workspace Squid fragment filename. The 10-
// prefix orders it after the base fragment written at boot.
func FragmentName(workspace string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(workspace)))
	return "10-" + hex.EncodeToString(sum[:])[:12] + ".conf"
}

// FragmentContent renders the Squid ACL lines for a workspace.
func FragmentContent(workspace string, domains []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# code-vm allowlist fragment for %s\n", filepath.Clean(workspace))
	for _, d := range domains {
		fmt.Fprintf(&b, "acl allowed_domains dstdomain %s\n", d)
	}
	return b.String()
}

// ApplyAllowlist installs the workspace's fragment and reloads Squid when the
// content changed. Reloading unconditionally would drop in-flight connections
// on every invocation.
func ApplyAllowlist(ctx context.Context, d Deps) error {
	domains, err := ReadDomains(d.Workspace)
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		return nil
	}
	name := FragmentName(d.Workspace)
	dst := fragmentDir + "/" + name
	want := FragmentContent(d.Workspace, domains)

	// A read failure means the fragment is absent, which is a change.
	current, _ := d.Client.AdminOutput(ctx, []string{"cat", dst})
	if string(current) == want {
		return nil
	}

	tmp, err := os.CreateTemp("", "code-vm-allow-*.conf")
	if err != nil {
		return fmt.Errorf("create temp fragment: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(want); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp fragment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp fragment: %w", err)
	}

	staged := "/tmp/" + name
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-m", "0444", "-o", "root", "-g", "root", staged, dst}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"rm", "-f", staged}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"squid", "-k", "reconfigure"})
}
