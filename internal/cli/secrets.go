package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/profile"
)

// newSecretsCmd reports the union of secrets and vars declared by the active
// profiles, and which of them are mapped. It is a report, not a gate: unlike
// loadConfigWithProfiles's other callers, an unmapped secret must not fail
// the command, only be called out — the reader needs to see the whole
// picture (mapped and unmapped alike) in one place. Names and status only:
// a secret's resolved value is never read here, let alone printed.
func newSecretsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "secrets",
		Short: "List secrets and vars declared by the active profiles",
		Long: "Lists every secret and var the active profiles declare, whether it is\n" +
			"mapped in secrets.yaml (or config.yaml's vars), which profiles declare\n" +
			"it, and its description. Never prints a secret's value. For each\n" +
			"unmapped secret with a suggested command, also prints a ready-to-paste\n" +
			"secrets.yaml snippet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, profiles, path, err := loadConfigWithProfiles()
			if err != nil {
				return err
			}
			sources, warnings, err := config.LoadSecrets(config.SecretsPathFor(path))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, w := range warnings {
				fmt.Fprintln(out, w)
			}

			declaredSecrets := profile.DeclaredSecrets(profiles)
			declaredVars := profile.DeclaredVars(profiles)
			fmt.Fprintf(out, "%-24s %-9s %-20s %s\n", "NAME", "STATUS", "PROFILES", "DESCRIPTION")
			for _, d := range declaredSecrets {
				status := "UNMAPPED"
				if _, ok := sources[d.Name]; ok {
					status = "mapped"
				}
				fmt.Fprintf(out, "%-24s %-9s %-20s %s\n", d.Name, status, strings.Join(d.Profiles, ", "), d.Description)
			}
			for _, d := range declaredVars {
				status := "UNMAPPED"
				if _, ok := c.Vars[d.Name]; ok {
					status = "mapped"
				}
				fmt.Fprintf(out, "%-24s %-9s %-20s %s\n", d.Name, status, strings.Join(d.Profiles, ", "), d.Description)
			}

			secretsPath := config.SecretsPathFor(path)
			for _, d := range declaredSecrets {
				if _, ok := sources[d.Name]; ok {
					continue
				}
				fmt.Fprintf(out, "\n# add to %s:\n%s", secretsPath, profile.MissingSecretSnippet(d))
			}
			return nil
		},
	}
}
