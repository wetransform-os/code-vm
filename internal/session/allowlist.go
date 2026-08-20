// Package session performs the privileged per-invocation setup that must
// happen before the agent runs: the egress allowlist, git identity and
// credential injection.
package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

// fragmentDir mirrors init-firewall.sh. It is tmpfs-backed, so the guest's
// allowlist cannot drift from the host config across a restart.
const fragmentDir = "/run/sandbox/squid-allow.d"

// HostFragmentName is the Squid fragment rendered from the host config's
// extraDomains. init-firewall.sh writes this same filename at boot, and
// ApplyAllowlist rewrites it afterwards; the two must stay in sync.
const HostFragmentName = "10-host-config.conf"

// HostRunner executes a command on the host. Injectable for tests.
type HostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Deps carries everything session setup needs. There is deliberately no
// workspace field: nothing in session setup may read agent-authored files.
//
// AgentGID is used instead of the account name when setting group ownership in
// the guest. The guest's group for that GID is not necessarily called
// AgentUser: stock groups occupy low GIDs, so a host user with GID 100 lands in
// the pre-existing "users" group and `chown root:devuser` would fail outright.
type Deps struct {
	Client    lima.Client
	Config    config.Config
	AgentUser string
	AgentUID  int
	AgentGID  int
	Host      HostRunner

	// AllowDomains is the full egress allowlist for the host-config fragment:
	// config extraDomains merged with active profile domains. The cli layer
	// computes it from trusted inputs only; session setup never derives it.
	AllowDomains []string
}

// FragmentContent renders the Squid ACL lines for the host config's domains.
// Entries are validated by config.Validate before they get here, so no
// escaping is required — an entry that could break out of the ACL line would
// have been rejected at load time.
func FragmentContent(domains []string) string {
	var b strings.Builder
	b.WriteString("# code-vm allowlist fragment rendered from the host config\n")
	for _, d := range domains {
		fmt.Fprintf(&b, "acl allowed_domains dstdomain %s\n", d)
	}
	return b.String()
}

// ApplyAllowlist brings the guest's allowlist fragment in line with the host
// config and reloads Squid if it changed. Reloading unconditionally would drop
// in-flight connections on every invocation.
//
// The host config is the only trusted source for this: it lives outside every
// mount, so the agent cannot widen its own egress by editing a file in the
// workspace. Active profile manifests are part of that trusted host input
// too — the cli layer merges their domains into d.AllowDomains before this
// runs, so this function itself never needs to know profiles exist.
//
// Once this has written the fragment at all, it stays the authority — even
// with an empty allowlist, a comment-only fragment is installed rather than
// removed. init-firewall.sh seeds this same file from the host config at
// boot, but only when it is absent, so that a firewall mode switch (which
// reruns init-firewall.sh) does not resurrect domains this function already
// revoked: absent means "no invocation has spoken since boot", present means
// "this is the current truth", and only a fresh boot may go back to absent.
func ApplyAllowlist(ctx context.Context, d Deps) error {
	dst := fragmentDir + "/" + HostFragmentName
	// Absent counts as a change. ReadFile keeps cat's "No such file" off the
	// caller's terminal: this runs on every invocation, and the fragment is
	// legitimately missing whenever no invocation has run since boot and no
	// extra domains are configured.
	current := d.Client.ReadFile(ctx, dst)

	want := FragmentContent(d.AllowDomains)
	if len(d.AllowDomains) == 0 && len(current) == 0 {
		// Nothing configured and nothing installed yet: installing a
		// comment-only fragment here would be harmless but would force a
		// pointless Squid reload on every invocation of a domain-less setup.
		return nil
	}
	if string(current) == want {
		return nil
	}
	if err := installContent(ctx, d, []byte(want), dst, "0444", "root", "root"); err != nil {
		return err
	}
	return reloadSquid(ctx, d)
}

// reloadSquid applies a changed configuration without dropping connections.
//
// Squid narrates every reconfigure — which files it parsed, which ACL entries it
// considers redundant — and that was landing in the caller's output, so a
// `code-vm -- cmd` invocation that happened to refresh the allowlist prefixed
// the command's own output with proxy chatter. Merging stderr into stdout in the
// guest lets AdminOutput capture all of it: discarded when the reload works,
// reported when it does not.
func reloadSquid(ctx context.Context, d Deps) error {
	out, err := d.Client.AdminOutput(ctx, []string{"bash", "-c", "squid -k reconfigure 2>&1"})
	if err != nil {
		return fmt.Errorf("reload squid: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
