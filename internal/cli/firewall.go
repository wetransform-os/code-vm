package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
)

// firewallModes are the supported egress modes, loosest last.
var firewallModes = []string{"allowlist", "audit", "open"}

func validateFirewallMode(mode string) error {
	for _, m := range firewallModes {
		if mode == m {
			return nil
		}
	}
	return fmt.Errorf("unknown firewall mode %q; want one of: %s", mode, strings.Join(firewallModes, ", "))
}

func setFirewallModeArgs(mode string) ([]string, error) {
	if err := validateFirewallMode(mode); err != nil {
		return nil, err
	}
	return []string{"/usr/local/lib/sandbox/set-firewall-mode.sh", mode}, nil
}

// currentFirewallMode reads the mode init-firewall.sh recorded in its verify
// file. Shared with `code-vm allow`, which reports whether a new domain took
// effect live or only once allowlist mode returns.
func currentFirewallMode(ctx context.Context, cl lima.Client) (string, error) {
	verify, err := cl.AdminOutput(ctx, []string{"cat", "/run/firewall-verify"})
	if err != nil {
		return "", fmt.Errorf("read firewall state: %w", err)
	}
	for _, line := range splitLines(string(verify)) {
		if strings.HasPrefix(line, "FIREWALL_MODE=") {
			return strings.TrimPrefix(line, "FIREWALL_MODE="), nil
		}
	}
	return "", fmt.Errorf("firewall mode not reported; is the VM running?")
}

func newFirewallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "firewall [allowlist|audit|open]",
		Short: "Show or change the egress firewall mode",
		Long: "Show or change the egress firewall mode.\n\n" +
			"  allowlist  domain allowlist, proxy mandatory (default)\n" +
			"  audit      all domains allowed, proxy still mandatory, still logged\n" +
			"  open       agent egress unfiltered and unlogged\n\n" +
			"The mode is runtime-only and lives in tmpfs: restarting the VM always\n" +
			"reverts to allowlist. There is no config key for it on purpose.\n\n" +
			"Note that the VM is shared by every workspace you have mounted, so a\n" +
			"loosened firewall applies to all of them at once, including any\n" +
			"injected credentials.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: firewallModes,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl := newClient()
			out := cmd.OutOrStdout()

			if len(args) == 0 {
				mode, err := currentFirewallMode(cmd.Context(), cl)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, mode)
				return nil
			}

			mode := args[0]
			guestCmd, err := setFirewallModeArgs(mode)
			if err != nil {
				return err
			}
			if mode == "open" && !yes {
				fmt.Fprint(out,
					"mode=open removes egress filtering AND the audit log for every\n"+
						"workspace mounted in this VM, including injected credentials.\n"+
						"It reverts on the next VM restart. Continue? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return fmt.Errorf("aborted")
				}
			}
			if err := cl.Admin(cmd.Context(), guestCmd); err != nil {
				return err
			}
			fmt.Fprintf(out, "Firewall mode is now %s (reverts to allowlist on VM restart).\n", mode)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt for mode=open")
	return cmd
}
