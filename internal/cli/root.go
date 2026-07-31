// Package cli implements the code-vm command line.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
)

var configPath string

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "code-vm [-- command [args...]]",
		Short: "Run coding agents in a hardened VM with real Docker",
		Long: "code-vm runs Claude Code inside a hardened Lima VM with rootless Docker,\n" +
			"an egress allowlist and a non-root agent user. Run it from a project\n" +
			"directory: that directory becomes the working directory in the guest.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDefault(cmd.Context(), args)
		},
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "path to config.yaml (default ~/.config/code-vm/config.yaml)")
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newStartCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newMountCmd())
	root.AddCommand(newRecreateCmd())
	root.AddCommand(newProxyLogCmd())
	root.AddCommand(newFirewallCmd())
	return root
}

// Execute runs the CLI.
func Execute() error {
	return NewRootCmd().Execute()
}

// loadConfig resolves the config path and loads it.
func loadConfig() (config.Config, string, error) {
	path := configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return config.Config{}, "", err
		}
		path = p
	}
	c, err := config.Load(path)
	if err != nil {
		return config.Config{}, path, fmt.Errorf("load config: %w", err)
	}
	return c, path, nil
}
