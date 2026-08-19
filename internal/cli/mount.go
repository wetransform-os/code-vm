package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
)

// addMount adds path to the shared directories unless an existing mount
// already covers it. Lima declares mounts in the instance config, so this
// requires a VM restart to take effect.
func addMount(c config.Config, path string) (config.Config, bool, error) {
	p, err := config.ExpandPath(path)
	if err != nil {
		return c, false, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return c, false, fmt.Errorf("cannot share %s: %w", p, err)
	}
	if !fi.IsDir() {
		return c, false, fmt.Errorf("cannot share %s: not a directory", p)
	}
	if _, ok := config.CoveringMount(c.Mounts(), p); ok {
		return c, false, nil
	}
	c.ExtraMounts = append(c.ExtraMounts, p)
	return c, true, nil
}

func newMountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount <directory>",
		Short: "Share an additional host directory with the sandbox VM",
		Long: "Share an additional host directory with the sandbox VM.\n\n" +
			"Lima declares mounts in the instance configuration, so the VM is\n" +
			"restarted to apply the change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, profiles, path, err := loadConfigWithProfiles()
			if err != nil {
				return err
			}
			updated, changed, err := addMount(c, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !changed {
				fmt.Fprintf(out, "%s is already shared; nothing to do.\n", args[0])
				return nil
			}
			if err := updated.Save(path); err != nil {
				return err
			}
			fmt.Fprintf(out, "Added %s to %s.\n", args[0], path)

			cl := clientFor(updated)
			status, err := cl.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status != "Running" {
				fmt.Fprintln(out, "VM is not running; the new mount applies on next start.")
				return nil
			}
			fmt.Fprintln(out, "Restarting the VM to apply the new mount...")
			if err := cl.Stop(cmd.Context()); err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), cl, updated, profiles)
		},
	}
}
