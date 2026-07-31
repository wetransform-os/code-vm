package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/session"
)

// squidACLPrefix is how allowlist entries appear in the generated squid.conf.
const squidACLPrefix = "acl allowed_domains dstdomain "

// normalizeDomain turns user or log input into an allowlist entry: a bare
// hostname prefixed with a dot, which Squid reads as the domain and all of its
// subdomains. Scheme, port and path are stripped. An input with no usable
// hostname returns "".
func normalizeDomain(raw string) string {
	d := strings.ToLower(strings.TrimSpace(raw))
	// Scheme first: "://" contains the colon the port strip looks for.
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	// Port. IPv6 literals are not valid allowlist entries anyway, and callers
	// drop them via isIPAddress before this matters.
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[:i]
	}
	d = strings.Trim(d, ".")
	if d == "" {
		return ""
	}
	// Anything that is not hostname-shaped is not a domain the caller meant.
	// Rejecting here rather than downstream keeps whitespace and quoting out of
	// the Squid ACL line entirely, and gives a clearer error than a validation
	// failure several steps later.
	for _, r := range d {
		hostnameChar := r == '.' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')
		if !hostnameChar {
			return ""
		}
	}
	return "." + d
}

// isIPAddress reports whether s denotes an IP rather than a domain. Allowing
// an IP through Squid's dstdomain ACL does not work, so such entries are
// dropped rather than added.
func isIPAddress(s string) bool {
	return net.ParseIP(strings.TrimPrefix(s, ".")) != nil
}

// parseDeniedDomains extracts the domains Squid refused from an access log,
// de-duplicated and sorted. Field 7 is the CONNECT target for HTTPS and the
// full URL for plain HTTP.
func parseDeniedDomains(log string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(log, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || !strings.Contains(fields[3], "DENIED") {
			continue
		}
		d := normalizeDomain(fields[6])
		if d == "" || isIPAddress(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// alreadyCovered reports whether domain is already permitted by one of the
// existing entries, either exactly or because an existing ".domain" entry is a
// parent of it.
func alreadyCovered(existing []string, domain string) bool {
	for _, e := range existing {
		if e == domain {
			return true
		}
		if strings.HasPrefix(e, ".") && strings.HasSuffix(domain, e) {
			return true
		}
	}
	return false
}

// mergeDomains returns the sorted union of a and b.
func mergeDomains(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range append(append([]string{}, a...), b...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// guestAllowedDomains reads the allowlist Squid is actually enforcing, so
// domains baked into the shipped default list are not offered again. In audit
// and open mode the generated config has no such ACL lines, and the result is
// empty — the only cost is that a redundant entry may be offered.
func guestAllowedDomains(ctx context.Context, cl lima.Client) []string {
	out, err := cl.AdminOutput(ctx, []string{"cat", "/etc/squid/squid.conf"})
	if err != nil {
		return nil
	}
	var domains []string
	for _, line := range splitLines(string(out)) {
		if d := strings.TrimPrefix(line, squidACLPrefix); d != line {
			domains = append(domains, strings.TrimSpace(d))
		}
	}
	return domains
}

// confirmDomains asks about each candidate. Lowercase answers apply to one
// domain, uppercase to all remaining ones.
func confirmDomains(in io.Reader, out io.Writer, candidates []string) ([]string, error) {
	reader := bufio.NewReader(in)
	var chosen []string
	batch := ""
	for _, d := range candidates {
		answer := batch
		if answer == "" {
			fmt.Fprintf(out, "  %s\n    [a] allow  [s] skip  (A/S = apply to all remaining): ", d)
			line, err := reader.ReadString('\n')
			if err != nil && strings.TrimSpace(line) == "" {
				return nil, fmt.Errorf("aborted")
			}
			answer = strings.TrimSpace(line)
		}
		switch answer {
		case "A":
			batch, answer = "a", "a"
		case "S":
			batch, answer = "s", "s"
		}
		if strings.ToLower(answer) == "a" {
			chosen = append(chosen, d)
		}
	}
	return chosen, nil
}

func newAllowCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "allow [domain ...]",
		Short: "Add domains to the egress allowlist and apply them immediately",
		Long: "Add domains to the egress allowlist.\n\n" +
			"With no arguments, the domains Squid recently denied are read from the\n" +
			"proxy log and offered one by one — the usual way to find what a build\n" +
			"actually needs.\n\n" +
			"Accepted domains are written to extraDomains in the host config, which\n" +
			"is the only trusted source for the allowlist: it lives outside every\n" +
			"mount, so the agent cannot widen its own egress. When the VM is running\n" +
			"the change is pushed to Squid straight away, without a restart.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			c, path, err := loadConfig()
			if err != nil {
				return err
			}
			cl := newClient()
			status, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			running := status == "Running"

			candidates, err := collectCandidates(ctx, cl, args, running, out)
			if err != nil || len(candidates) == 0 {
				return err
			}

			known := mergeDomains(c.ExtraDomains, nil)
			if running {
				known = mergeDomains(known, guestAllowedDomains(ctx, cl))
			}
			var pending []string
			for _, d := range candidates {
				if alreadyCovered(known, d) {
					fmt.Fprintf(out, "  already allowed: %s\n", d)
					continue
				}
				pending = append(pending, d)
			}
			if len(pending) == 0 {
				fmt.Fprintln(out, "Nothing to add.")
				return nil
			}
			for _, d := range pending {
				if err := config.ValidateDomain(d); err != nil {
					return err
				}
			}

			chosen := pending
			if !yes {
				if !isTerminal(os.Stdin) {
					return fmt.Errorf("confirmation needs a terminal; re-run with --yes to add: %s",
						strings.Join(pending, " "))
				}
				if chosen, err = confirmDomains(os.Stdin, out, pending); err != nil {
					return err
				}
			}
			if len(chosen) == 0 {
				fmt.Fprintln(out, "Nothing added.")
				return nil
			}

			c.ExtraDomains = mergeDomains(c.ExtraDomains, chosen)
			if err := c.Validate(); err != nil {
				return err
			}
			if err := c.Save(path); err != nil {
				return err
			}
			fmt.Fprintf(out, "Added %d domain(s) to %s.\n", len(chosen), path)

			if !running {
				fmt.Fprintln(out, "VM is not running; the new domains apply on next start.")
				return nil
			}
			if err := session.ApplyAllowlist(ctx, session.Deps{
				Client: cl, Config: c, AgentUser: agentUser,
			}); err != nil {
				return fmt.Errorf("apply allowlist: %w", err)
			}
			mode, err := currentFirewallMode(ctx, cl)
			if err != nil || mode == "allowlist" {
				fmt.Fprintln(out, "Squid reloaded — the new domains are active now.")
				return nil
			}
			fmt.Fprintf(out,
				"Stored, but not applied live: firewall mode is %q, which already allows every domain.\n"+
					"They take effect when you return to allowlist mode.\n", mode)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "add every domain without confirmation")
	return cmd
}

// collectCandidates gathers the domains to consider, from arguments or from
// the proxy log's denied entries.
func collectCandidates(ctx context.Context, cl lima.Client, args []string, running bool, out io.Writer) ([]string, error) {
	if len(args) > 0 {
		var candidates []string
		for _, a := range args {
			d := normalizeDomain(a)
			if d == "" {
				return nil, fmt.Errorf("cannot read a domain from %q", a)
			}
			if isIPAddress(d) {
				fmt.Fprintf(out, "  skipping IP address: %s\n", strings.TrimPrefix(d, "."))
				continue
			}
			candidates = append(candidates, d)
		}
		return mergeDomains(candidates, nil), nil
	}

	if !running {
		return nil, fmt.Errorf("reading denied domains needs a running VM; start it with `code-vm start`, " +
			"or pass domains explicitly: code-vm allow example.com")
	}
	log, err := cl.AdminOutput(ctx, []string{"cat", "/var/log/squid/access.log"})
	if err != nil {
		return nil, fmt.Errorf("read the proxy log: %w", err)
	}
	candidates := parseDeniedDomains(string(log))
	if len(candidates) == 0 {
		fmt.Fprintln(out, "No denied requests in the proxy log.")
	}
	return candidates, nil
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
