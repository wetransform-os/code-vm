package cli

import "github.com/spf13/cobra"

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the sandbox VM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			return clientFor(c).Stop(cmd.Context())
		},
	}
}
