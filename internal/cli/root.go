// Package cli implements the code-vm command line.
package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/profile"
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
	root.AddCommand(newAllowCmd())
	root.AddCommand(newProfileCmd())
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
	// Canonicalize before Load: every downstream consumer of path (Save,
	// ProfilesDirFor, and both mount guards below) compares it against
	// absolute mounts. A relative --config would silently defeat both guards
	// — they'd compare "./config.yaml" against an absolute mount and never
	// match — rather than refuse a config that exposes itself to the agent.
	path, err := filepath.Abs(path)
	if err != nil {
		return config.Config{}, path, fmt.Errorf("resolve config path: %w", err)
	}
	c, err := config.Load(path)
	if err != nil {
		return config.Config{}, path, fmt.Errorf("load config: %w", err)
	}
	// Checked on every invocation, not just at Validate time: the config is the
	// only trusted source for the egress allowlist, so a mount that exposes it
	// to the agent has to be refused before the VM is touched.
	if err := c.MountsExclude(path); err != nil {
		return config.Config{}, path, err
	}
	// Profiles feed the egress allowlist too, so the same rule applies to
	// their directory — including mounts *inside* it, which the config-file
	// check cannot see.
	if err := c.MountsExcludeTree(config.ProfilesDirFor(path)); err != nil {
		return config.Config{}, path, err
	}
	return c, path, nil
}

// loadConfigWithProfiles is loadConfig plus the active profile bundles.
// Commands that render the VM or apply session state use this, so a broken
// profile fails at invocation start rather than mid-boot. Management commands
// (stop, status, profile list/remove, ...) stay on loadConfig: they must keep
// working precisely when a listed profile is broken.
func loadConfigWithProfiles() (config.Config, []profile.Profile, string, error) {
	c, path, err := loadConfig()
	if err != nil {
		return config.Config{}, nil, "", err
	}
	profiles, err := profile.LoadAll(config.ProfilesDirFor(path), c.Profiles)
	if err != nil {
		return config.Config{}, nil, "", fmt.Errorf("load profiles: %w", err)
	}
	return c, profiles, path, nil
}
