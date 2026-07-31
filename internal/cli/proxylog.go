package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// proxyLogArgs validates the mode and builds the guest command.
func proxyLogArgs(mode string) ([]string, error) {
	switch mode {
	case "all", "denied", "allowed", "follow":
		return []string{"/usr/local/lib/sandbox/proxy-log.sh", mode}, nil
	default:
		return nil, fmt.Errorf("unknown mode %q; want all, denied, allowed or follow", mode)
	}
}

func newProxyLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "proxy-log [all|denied|allowed|follow]",
		Short:     "Read the Squid access log from the sandbox VM",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"all", "denied", "allowed", "follow"},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := "all"
			if len(args) == 1 {
				mode = args[0]
			}
			guestCmd, err := proxyLogArgs(mode)
			if err != nil {
				return err
			}
			return newClient().Admin(cmd.Context(), guestCmd)
		},
	}
}
