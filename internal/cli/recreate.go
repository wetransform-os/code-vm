package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newRecreateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "recreate",
		Short: "Delete and rebuild the sandbox VM from scratch",
		Long: "Delete and rebuild the sandbox VM.\n\n" +
			"This destroys the guest disk, which holds Claude authentication,\n" +
			"installed plugins and the Docker image cache. Workspace files live\n" +
			"on the host and are unaffected.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, profiles, _, err := loadConfigWithProfiles()
			if err != nil {
				return err
			}
			if !yes {
				fmt.Fprint(cmd.OutOrStdout(),
					"This deletes the guest disk, including Claude authentication and the\n"+
						"Docker image cache. Workspace files are not affected. Continue? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return fmt.Errorf("aborted")
				}
			}
			cl := clientFor(c)
			if err := cl.Delete(cmd.Context()); err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), cl, c, profiles)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
