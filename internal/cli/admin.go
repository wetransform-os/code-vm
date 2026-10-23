package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "admin [-- command [args...]]",
		Short: "Enter the guest as limaadmin (sudo available)",
		Long: "Open a shell in the running VM as limaadmin, the privileged setup account,\n" +
			"or run one command as it. Nothing goes through the agent user or its\n" +
			"workspace; sudo is available inside. Put `--` before a command that\n" +
			"takes flags so they reach the guest instead of code-vm.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			cl := clientFor(c)
			status, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			if status != "Running" {
				return fmt.Errorf("the VM is not running; start it with `code-vm start`")
			}
			return cl.Shell(ctx, args)
		},
	}
}
