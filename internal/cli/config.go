package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/session"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage the guest config this binary ships",
	}
	cmd.AddCommand(newConfigApplyCmd())
	return cmd
}

func newConfigApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Push this binary's Claude settings into the running VM and lock them",
		Long: "Push the canonical config tree embedded in this binary (the agent's\n" +
			"locked .claude/settings.json) into the running VM and re-run the lock\n" +
			"step, so a rebuilt code-vm takes effect without a restart. The same\n" +
			"delivery happens automatically on every boot. Boot scripts and units\n" +
			"are not touched; those still need a restart.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, profiles, _, err := loadConfigWithProfiles()
			if err != nil {
				return err
			}
			cl := clientFor(c)
			status, err := cl.Status(ctx)
			if err != nil {
				return err
			}
			if status != "Running" {
				return fmt.Errorf("the VM is not running; the config applies automatically at boot — start it with `code-vm start`")
			}
			files, err := guest.ConfigFiles()
			if err != nil {
				return err
			}
			if err := session.PushConfig(ctx, agentDeps(cl, c, profiles), files); err != nil {
				return fmt.Errorf("push config: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Config applied and locked. Running Claude sessions may need a restart to pick it up.")
			return nil
		},
	}
}
