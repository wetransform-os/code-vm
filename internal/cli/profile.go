package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/profile"
	"github.com/wetransform/code-vm/internal/session"
)

// profilesDir resolves the bundle directory next to the active config file.
func profilesDir() (string, error) {
	path := configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	return config.ProfilesDirFor(path), nil
}

// runGit runs git with the given arguments, streaming output to the command's
// writers so clone/pull progress stays visible.
func runGit(ctx context.Context, cmd *cobra.Command, dir string, args ...string) error {
	g := exec.CommandContext(ctx, "git", args...)
	g.Dir = dir
	g.Stdout = cmd.OutOrStdout()
	g.Stderr = cmd.ErrOrStderr()
	if err := g.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

const trustWarning = `A profile is host-trusted input, like config.yaml itself. Installing one
means trusting its author with: the agent's home directory contents, apt
package selection, additions to the egress allowlist, and code execution as
the agent user (hooks run as the agent, never as root).`

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage VM customization profiles",
		Long: "Profiles are named bundles under the profiles directory next to the\n" +
			"config file. A profile ships files into the agent's home, installs apt\n" +
			"packages, sets the agent's login shell, adds egress domains, and may run\n" +
			"a hook script as the agent user. Activate profiles by listing their\n" +
			"names under `profiles:` in the config; they apply at boot and via\n" +
			"`code-vm profile apply`.",
	}
	cmd.AddCommand(newProfileAddCmd(), newProfileUpdateCmd(), newProfileListCmd(),
		newProfileRemoveCmd(), newProfileApplyCmd())
	return cmd
}

func newProfileAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <git-url> [name]",
		Short: "Clone a profile bundle into the profiles directory",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			name := strings.TrimSuffix(filepath.Base(url), ".git")
			if len(args) == 2 {
				name = args[1]
			}
			// Validated before any filepath.Join: name reaches a clone
			// destination below, and an unchecked "../../x" would clone
			// outside the profiles directory.
			if err := profile.ValidateName(name); err != nil {
				return err
			}
			dir, err := profilesDir()
			if err != nil {
				return err
			}
			dst := filepath.Join(dir, name)
			if _, err := os.Stat(dst); err == nil {
				return fmt.Errorf("profile %s already exists; refresh it with `code-vm profile update %s`", name, name)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := runGit(cmd.Context(), cmd, "", "clone", url, dst); err != nil {
				return err
			}
			if _, loadErr := profile.Load(dir, name); loadErr != nil {
				// A broken bundle must not linger: it would fail every
				// loadConfigWithProfiles the moment someone activates it.
				if rmErr := os.RemoveAll(dst); rmErr != nil {
					return fmt.Errorf("cloned profile is invalid (%w) and cleanup also failed: %v", loadErr, rmErr)
				}
				return fmt.Errorf("cloned profile is invalid and was removed again: %w", loadErr)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Installed profile %s.\n\n%s\n\n", name, trustWarning)
			fmt.Fprintf(out, "Activate it by adding to your config:\n\nprofiles:\n  - %s\n", name)
			return nil
		},
	}
}

func newProfileUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name]",
		Short: "Pull the latest version of one or all git-managed profiles",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := profilesDir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var names []string
			if len(args) > 0 {
				// Validated up front and strictly: an explicit "../../repo"
				// argument would otherwise git-pull an arbitrary directory,
				// so any non-conforming name aborts the whole invocation
				// rather than being silently skipped.
				for _, name := range args {
					if err := profile.ValidateName(name); err != nil {
						return err
					}
				}
				names = args
			} else {
				// A directory listing is not user input in the same sense:
				// one stray non-conforming directory (hand-created, or left
				// over from something else) must not abort updating every
				// other, otherwise-valid profile.
				entries, err := os.ReadDir(dir)
				if err != nil {
					return fmt.Errorf("no profiles directory at %s", dir)
				}
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					if err := profile.ValidateName(e.Name()); err != nil {
						fmt.Fprintf(out, "%s: not a valid profile name, skipped\n", e.Name())
						continue
					}
					names = append(names, e.Name())
				}
			}
			for _, name := range names {
				p := filepath.Join(dir, name)
				if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
					// Hand-authored local profiles are first-class; only git
					// clones have somewhere to pull from.
					fmt.Fprintf(out, "%s: not a git clone, skipped\n", name)
					continue
				}
				if err := runGit(cmd.Context(), cmd, p, "pull", "--ff-only"); err != nil {
					return err
				}
				if _, err := profile.Load(dir, name); err != nil {
					return fmt.Errorf("profile %s is invalid after update: %w", name, err)
				}
				fmt.Fprintf(out, "%s: updated\n", name)
			}
			return nil
		},
	}
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Raw loadConfig: list must keep working when a listed profile is
			// broken — that is exactly when the user reaches for it.
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			dir, err := profilesDir()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(dir)
			if os.IsNotExist(err) {
				fmt.Fprintln(cmd.OutOrStdout(), "No profiles installed. Add one with: code-vm profile add <git-url>")
				return nil
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				state := "inactive"
				if slices.Contains(c.Profiles, name) {
					state = "active"
				}
				desc := ""
				if p, err := profile.Load(dir, name); err != nil {
					state = "invalid"
					desc = err.Error()
				} else {
					desc = p.Manifest.Description
				}
				fmt.Fprintf(out, "%-24s %-8s %s\n", name, state, desc)
			}
			return nil
		},
	}
}

func newProfileRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete an installed profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Validated before any filepath.Join: name reaches os.RemoveAll
			// below, and an unchecked "../../some-folder" would delete an
			// arbitrary existing path on the host.
			if err := profile.ValidateName(name); err != nil {
				return err
			}
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			if slices.Contains(c.Profiles, name) {
				return fmt.Errorf("profile %s is active; remove it from `profiles:` in the config first", name)
			}
			dir, err := profilesDir()
			if err != nil {
				return err
			}
			target := filepath.Join(dir, name)
			if _, err := os.Stat(target); err != nil {
				return fmt.Errorf("no profile named %s at %s", name, target)
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed profile %s.\n", name)
			return nil
		},
	}
}

func newProfileApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Push the active profiles into the running VM and apply them",
		Long: "Push the profiles listed in the config into the running VM and apply\n" +
			"them: install files into the agent's home, install packages, set the\n" +
			"shell and run hooks. The same application also happens automatically on\n" +
			"every boot; this command exists so profile changes do not need a restart.",
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
				return fmt.Errorf("the VM is not running; profiles apply automatically at boot — start it with `code-vm start`")
			}
			d := agentDeps(cl, c, profiles)
			if err := session.PushProfiles(ctx, d, profile.GuestFiles(profiles)); err != nil {
				return fmt.Errorf("push profiles: %w", err)
			}
			// Allowlist before hooks run: a profile's own domains must be
			// live before its hook needs them.
			if err := session.ApplyAllowlist(ctx, d); err != nil {
				return fmt.Errorf("apply allowlist: %w", err)
			}
			if err := session.ApplyProfiles(ctx, d); err != nil {
				return fmt.Errorf("apply profiles: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Profiles applied.")
			return nil
		},
	}
}
