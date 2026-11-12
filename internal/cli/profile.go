package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/profile"
	"github.com/wetransform/code-vm/internal/session"
)

// profilesDir resolves the bundle directory next to the active config file,
// as an absolute path: git commands run with it (or a directory below it)
// as their working directory, and must not depend on the process's.
func profilesDir() (string, error) {
	path := configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	return filepath.Abs(config.ProfilesDirFor(path))
}

// gitOrigin reports a profile's git remote origin, trimmed, or "local" when
// dir is not a git checkout (or has no origin remote) — the design spec's
// list command shows "git origin if any".
func gitOrigin(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "local"
	}
	origin := strings.TrimSpace(string(out))
	if origin == "" {
		return "local"
	}
	return origin
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

const sourcesTrustWarning = `A profile's sources are pulled from the repositories its author names and
placed in the agent's home as-is; trusting the profile means trusting those
repositories at the refs it pins.`

// wrapperManifest is the profile.yaml of a profile that only wraps one
// source. Every value is double-quoted (strconv.Quote yields valid YAML
// escapes), so none can break the YAML and none is read as anything but a
// string: a bare `ref: null` would decode as no ref at all. LoadManifest
// then applies the manifest rules to the values themselves.
func wrapperManifest(url, into, ref string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "description: %s\nsources:\n  - repo: %s\n    into: %s\n",
		strconv.Quote(url+" → ~/"+into), strconv.Quote(url), strconv.Quote(into))
	if ref != "" {
		fmt.Fprintf(&b, "    ref: %s\n", strconv.Quote(ref))
	}
	return b.String()
}

// reportSyncFailures prints each failed source. A failed source never fails
// add or stops update: the rest of the profile is still useful, and Load
// marks the source unavailable until a later update succeeds. A refused
// repository will not succeed on a retry, so it says what to change instead.
func reportSyncFailures(w io.Writer, fails []profile.SyncFailure) {
	for _, f := range fails {
		hint := "the source stays unavailable until profile update succeeds"
		var r *repoRefusal
		if errors.As(f.Err, &r) {
			hint = "the source stays unavailable; " + r.hint
		}
		fmt.Fprintf(w, "%v; %s\n", f, hint)
	}
}

// repoRefusal is a repository checkRepo refuses to let git touch, the only
// kind of error checkRepo returns; hint says what to change.
type repoRefusal struct{ msg, hint string }

func (r *repoRefusal) Error() string { return r.msg }

// checkRepo refuses a repository (a profile bundle's URL or a source's repo,
// what names which in the message) that is a local path the agent may be
// able to write. Fetching from a local repository runs git upload-pack in
// it, under that repository's own config (core.alternateRefsCommand and the
// like), so a repository the agent can write would run its commands on the
// host on the next update. Remote URLs pass. A local repository must be an
// absolute path or a file:// URL (whose percent-encoding git decodes, so the
// check sees the decoded path) without any ".." segment, must resolve, and
// its fully resolved location must lie outside every shared directory. The
// raw path is resolved, never cleaned lexically first: through a symlink,
// "link/../x" is not "x".
func checkRepo(c config.Config, repo, what string) error {
	fix := "use an absolute path or a file:// URL outside the shared directories, or a remote URL"
	local, ok, err := profile.LocalRepoPath(repo)
	if err != nil {
		return &repoRefusal{msg: err.Error(), hint: fix}
	}
	if !ok {
		return nil
	}
	if !filepath.IsAbs(local) {
		return &repoRefusal{msg: fmt.Sprintf("repository path %s is relative; a local repository must be an absolute path or a file:// URL", local), hint: fix}
	}
	if slices.Contains(strings.Split(filepath.ToSlash(local), "/"), "..") {
		return &repoRefusal{msg: fmt.Sprintf("repository path %s contains a \"..\" segment, which is not allowed", local), hint: fix}
	}
	// A path that does not resolve is refused too: inside a shared
	// directory, the agent could create the repository between this check
	// and git's run.
	real, err := filepath.EvalSymlinks(local)
	if err != nil {
		return &repoRefusal{msg: fmt.Sprintf("repository path %s does not resolve: %v", local, err),
			hint: "point it at an existing repository outside the shared directories, or a remote URL"}
	}
	var mounts []string
	for _, m := range c.Mounts() {
		mounts = append(mounts, config.CanonicalizeExisting(m))
	}
	// Not only where the path ends up: every prefix's resolution must stay
	// outside the shared directories too. A symlink inside one that points
	// outside resolves to a safe place now, but the agent can retarget it
	// before git follows it.
	shared, m := false, ""
	prefix := string(filepath.Separator)
	for _, seg := range strings.Split(filepath.Clean(local), string(filepath.Separator)) {
		if seg == "" {
			continue
		}
		prefix = filepath.Join(prefix, seg)
		r, err := filepath.EvalSymlinks(prefix)
		if err != nil {
			r = prefix // unreachable: the whole path resolved above
		}
		if m, shared = config.CoveringMount(mounts, r); shared {
			break
		}
	}
	if shared {
		return &repoRefusal{
			msg: fmt.Sprintf("repository path %s lies inside the shared directory %s, which the agent can write; "+
				"%s must come from a path or URL the agent cannot modify", real, m, what),
			hint: "move the repository outside the shared directories or use a remote URL",
		}
	}
	return nil
}

// pullProfile fast-forwards the profile clone at dir, unless checkRepo
// refuses its origin: pulling runs git upload-pack in the origin repository,
// so a clone of a repository inside a shared directory would hand the agent
// the host on every update. git runs in the clone itself, inside the
// profiles directory, which the agent cannot write.
func pullProfile(cmd *cobra.Command, c config.Config, dir string) error {
	// A clone without an origin has nothing checkRepo could judge; the
	// pull then fails on its own.
	if out, err := exec.CommandContext(cmd.Context(), "git", "-C", dir, "remote", "get-url", "origin").Output(); err == nil {
		var r *repoRefusal
		if err := checkRepo(c, strings.TrimSpace(string(out)), "profiles"); errors.As(err, &r) {
			return fmt.Errorf("profile clone origin: %w; %s", err, r.hint)
		}
	}
	return runGit(cmd.Context(), cmd, dir, "pull", "--ff-only")
}

// syncSources is profile.SyncSources minus the sources checkRepo refuses:
// each is reported as a failure instead and left untouched (no checkout
// created or fetched).
func syncSources(ctx context.Context, c config.Config, dir string, p profile.Profile, out io.Writer) []profile.SyncFailure {
	var refused []profile.SyncFailure
	allowed := p
	allowed.Sources = nil
	for _, s := range p.Sources {
		if err := checkRepo(c, s.Repo, "sources"); err != nil {
			refused = append(refused, profile.SyncFailure{Profile: p.Name, Source: s.Name, Repo: s.Repo, Err: err})
			continue
		}
		allowed.Sources = append(allowed.Sources, s)
	}
	return append(refused, profile.SyncSources(ctx, dir, allowed, out)...)
}

func newProfileAddCmd() *cobra.Command {
	var into, ref string
	cmd := &cobra.Command{
		Use:   "add <git-url> [name]",
		Short: "Clone a profile bundle, or wrap a plain repo as one with --into",
		Long: "Clone a profile bundle into the profiles directory and fetch the sources\n" +
			"it declares. With --into, the repository is not a profile itself:\n" +
			"a profile is generated that places it at the given path under the agent's\n" +
			"home, optionally pinned to --ref.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Usage checks come before anything is cloned or written.
			if ref != "" && into == "" {
				return fmt.Errorf("--ref is only valid together with --into")
			}
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
			// The mounts decide which local repositories are refused.
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			// The URL is the user's own argument, so a refusal is a hard
			// error before anything is written: for a bundle, git would
			// otherwise run in an agent-writable repository now and on
			// every update; for a wrapper, its only source could never sync.
			whatRepo := "profiles"
			if into != "" {
				whatRepo = "sources"
			}
			var refusal *repoRefusal
			if err := checkRepo(c, url, whatRepo); errors.As(err, &refusal) {
				return fmt.Errorf("%w; %s", err, refusal.hint)
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
			what := "cloned profile"
			if into != "" {
				what = "generated profile"
				if err := os.MkdirAll(dst, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dst, "profile.yaml"), []byte(wrapperManifest(url, into, ref)), 0o644); err != nil {
					_ = os.RemoveAll(dst)
					return err
				}
			} else if err := runGit(cmd.Context(), cmd, dir, "clone", "--", url, dst); err != nil {
				// Run in the profiles directory, not the process's working
				// directory: git prefers an existing directory relative to
				// it over an scp-like URL, and one planted in the project
				// code-vm was started from would be cloned instead.
				return err
			}
			// A broken bundle must not linger: it would fail every
			// loadConfigWithProfiles the moment someone activates it. Its
			// checkouts go with it, so a later add of the same name starts
			// clean.
			discard := func(loadErr error) error {
				rmErr := os.RemoveAll(dst)
				if err := os.RemoveAll(filepath.Join(profile.SourcesDir(dir), name)); rmErr == nil {
					rmErr = err
				}
				if rmErr != nil {
					return fmt.Errorf("%s is invalid (%w) and cleanup also failed: %v", what, loadErr, rmErr)
				}
				return fmt.Errorf("%s is invalid and was removed again: %w", what, loadErr)
			}
			manifest, err := profile.LoadManifest(dir, name)
			if err != nil {
				return discard(err)
			}
			out := cmd.OutOrStdout()
			reportSyncFailures(cmd.ErrOrStderr(), syncSources(cmd.Context(), c, dir, manifest, out))
			loaded, err := profile.Load(dir, name)
			if err != nil {
				return discard(err)
			}
			fmt.Fprintf(out, "Installed profile %s.\n\n%s\n\n", name, trustWarning)
			if len(loaded.Manifest.Sources) > 0 {
				fmt.Fprintf(out, "%s\n\n", sourcesTrustWarning)
			}
			fmt.Fprintf(out, "Activate it by adding to your config:\n\nprofiles:\n  - %s\n", name)
			// Flagged separately from the generic trust warning above: a
			// mapped secret's value becomes readable by the agent and by
			// every other active profile's hook, not just this one, which
			// is a sharper claim than "this profile is host-trusted".
			if declared := profile.DeclaredSecrets([]profile.Profile{loaded}); len(declared) > 0 {
				names := make([]string, len(declared))
				for i, d := range declared {
					names[i] = d.Name
				}
				fmt.Fprintf(out, "\nThis profile declares secrets: %s. Mapping them in secrets.yaml makes "+
					"their values readable by the agent (and every active profile's hook).\n",
					strings.Join(names, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&into, "into", "", "wrap the repository as a source placed at this path under the agent's home")
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit to pin the wrapped source to (requires --into)")
	return cmd
}

func newProfileUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name]",
		Short: "Update one or all profiles: pull git clones and fetch sources",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The mounts decide which local source paths are refused.
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
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
			// Failed sources are collected across all profiles rather than
			// aborting: one unreachable repo must not hold back the others,
			// and a failed fetch keeps the old checkout usable anyway. Without
			// a name, an invalid profile or a failed pull is collected the
			// same way: one broken profile must not stop the others from
			// updating.
			explicit := len(args) > 0
			errOut := cmd.ErrOrStderr()
			var fails []profile.SyncFailure
			var invalid, unpulled []string
			// finish prints what was collected and returns err, or an error
			// summarising the collection when err is nil, so every exit path
			// reports the failures gathered so far.
			finish := func(err error) error {
				if len(unpulled) > 0 {
					fmt.Fprintf(errOut, "%d profile(s) could not be pulled: %s\n", len(unpulled), strings.Join(unpulled, ", "))
				}
				if len(invalid) > 0 {
					fmt.Fprintf(errOut, "%d profile(s) invalid: %s\n", len(invalid), strings.Join(invalid, ", "))
				}
				if len(fails) > 0 {
					fmt.Fprintf(errOut, "%d source(s) could not be updated:\n", len(fails))
					for _, f := range fails {
						fmt.Fprintf(errOut, "  %v\n", f)
					}
				}
				if err != nil {
					return err
				}
				var parts []string
				if len(unpulled) > 0 {
					parts = append(parts, fmt.Sprintf("%d profile(s) could not be pulled: %s", len(unpulled), strings.Join(unpulled, ", ")))
				}
				if len(invalid) > 0 {
					parts = append(parts, fmt.Sprintf("%d profile(s) invalid", len(invalid)))
				}
				if len(fails) > 0 {
					parts = append(parts, fmt.Sprintf("%d source(s) could not be updated", len(fails)))
				}
				if len(parts) > 0 {
					return errors.New(strings.Join(parts, "; "))
				}
				return nil
			}
			for _, name := range names {
				p := filepath.Join(dir, name)
				state := "invalid"
				if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
					if err := pullProfile(cmd, c, p); err != nil {
						if explicit {
							return finish(err)
						}
						// Like an invalid profile, one unreachable or
						// diverged profile remote must not stop the
						// others. Its sources are left alone: the
						// manifest that names them may be stale.
						fmt.Fprintf(errOut, "%s: pull failed: %v\n", name, err)
						unpulled = append(unpulled, name)
						continue
					}
					fmt.Fprintf(out, "%s: pulled\n", name)
					state = "invalid after update"
				} else {
					// Hand-authored local profiles (and --into wrappers) are
					// first-class; only git clones have somewhere to pull
					// from, but their sources still sync.
					fmt.Fprintf(out, "%s: not a git clone, sources only\n", name)
				}
				// skip reports an invalid profile: fatal when it was named,
				// collected otherwise.
				skip := func(err error) error {
					if explicit {
						return finish(fmt.Errorf("profile %s is %s: %w", name, state, err))
					}
					fmt.Fprintf(errOut, "%s: %s: %v\n", name, state, err)
					invalid = append(invalid, name)
					return nil
				}
				// The manifest just pulled decides which checkouts exist:
				// prune what it no longer declares before syncing the rest.
				manifest, err := profile.LoadManifest(dir, name)
				if err != nil {
					if err := skip(err); err != nil {
						return err
					}
					continue
				}
				pruned, err := profile.PruneSources(dir, manifest)
				for _, s := range pruned {
					fmt.Fprintf(out, "%s: pruned source %s\n", name, s)
				}
				if err != nil {
					return finish(fmt.Errorf("profile %s: prune sources: %w", name, err))
				}
				fails = append(fails, syncSources(cmd.Context(), c, dir, manifest, out)...)
				if _, err := profile.Load(dir, name); err != nil {
					if err := skip(err); err != nil {
						return err
					}
					continue
				}
				fmt.Fprintf(out, "%s: updated\n", name)
			}
			return finish(nil)
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
				// Load, not just LoadManifest: it also resolves each source's
				// commit for the lines below. It never touches the network,
				// and an unfetched or unresolvable source is reported on its
				// own line rather than making the whole profile invalid.
				p, err := profile.Load(dir, name)
				if err != nil {
					state = "invalid"
					desc = err.Error()
				} else {
					desc = p.Manifest.Description
					// Surfaced right in the list a user already reaches for,
					// so a profile's secret needs are visible before they
					// even get as far as `code-vm secrets`.
					if declared := profile.DeclaredSecrets([]profile.Profile{p}); len(declared) > 0 {
						names := make([]string, len(declared))
						for i, d := range declared {
							names[i] = d.Name
						}
						desc += fmt.Sprintf("  [secrets: %s]", strings.Join(names, ", "))
					}
				}
				origin := gitOrigin(filepath.Join(dir, name))
				fmt.Fprintf(out, "%-24s %-8s %-40s %s\n", name, state, origin, desc)
				for _, s := range p.Sources {
					ref, commit := s.Ref, s.Unavailable
					if ref == "" {
						ref = "default"
					}
					if commit == "" {
						commit = s.Commit[:12]
					} else {
						commit = "unavailable: " + commit
					}
					fmt.Fprintf(out, "    source %-24s ~/%s  %s  %s\n", s.Name, s.Into, ref, commit)
				}
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
			// The checkouts belong to the profile; left behind they would be
			// picked up by a later, unrelated profile of the same name.
			if err := os.RemoveAll(filepath.Join(profile.SourcesDir(dir), name)); err != nil {
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
			"them: install sources and files into the agent's home, install packages,\n" +
			"set the shell and run hooks. Sources install from the checkouts that\n" +
			"`profile add` and `profile update` fetched; nothing is fetched here. The\n" +
			"same application also happens automatically on every boot; this command\n" +
			"exists so profile changes do not need a restart.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, profiles, cfgPath, err := loadConfigWithProfiles(cmd.ErrOrStderr())
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
			// Resolve (and render) before touching the guest at all:
			// resolution needs nothing from the guest, and PushProfiles/
			// ApplyAllowlist below stage a new profile tree and make its
			// domains live. An unmapped-secret failure must abort before
			// any of that happens, not after.
			rendered, err := resolveRendered(ctx, c, profiles, cfgPath, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			// This binary's applier, fetched up front for the same reason:
			// ApplyProfiles reinstalls it in the guest when it is older.
			applier, err := embeddedFile(session.ApplierScript)
			if err != nil {
				return err
			}
			d := agentDeps(cl, c, profiles)
			// First guest interaction: the boot sequence provisions the very
			// tree PushProfiles wipes and execs the applier ApplyProfiles
			// may replace, so nothing below may interleave with it.
			if err := session.RequireBootComplete(ctx, d); err != nil {
				return err
			}
			if err := session.PushProfiles(ctx, d, profile.GuestFiles(profiles)); err != nil {
				return fmt.Errorf("push profiles: %w", err)
			}
			// Allowlist before hooks run: a profile's own domains must be
			// live before its hook needs them.
			if err := session.ApplyAllowlist(ctx, d); err != nil {
				return fmt.Errorf("apply allowlist: %w", err)
			}
			// Rendered templates land before hooks run: a hook may consume
			// the configs a profile's own templates just produced. Already
			// resolved above, so this only pushes — no second resolution.
			if err := pushRendered(ctx, cl, c, profiles, rendered, cmd.OutOrStdout()); err != nil {
				return err
			}
			if err := session.ApplyProfiles(ctx, d, applier); err != nil {
				return fmt.Errorf("apply profiles: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Profiles applied.")
			return nil
		},
	}
}
