package cli

import "github.com/spf13/cobra"

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the sandbox VM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return newClient().Stop(cmd.Context())
		},
	}
}
