# VM Customization Profiles — Design

**Date:** 2026-08-19
**Status:** Approved design, pending implementation plan

## Context

The VM's shape is fully determined by embedded assets plus a small host config
(`~/.config/code-vm/config.yaml`). Users have no supported way to customize the
guest environment: no pre-defined `CLAUDE.md` (the predecessor container
sandbox shipped one), no preferred shell, no extra tools. Hand-editing the
guest does not survive `code-vm recreate` and cannot be shared with teammates.

This design adds **profiles**: named, team-shareable customization bundles that
users activate in the host config. Motivating use cases:

- Ship a pre-defined `.claude/CLAUDE.md` into the agent's home.
- Set up the user's preferred environment: install fish, make it the agent's
  login shell, install fisher and fisher plugins.
- Install additional tools via apt.

## Goals

- Team-shareable bundles: distributed via git, activated by name in
  `config.yaml`.
- Declarative core interpreted by code-vm (files, apt packages, login shell,
  allowlist domains) plus an optional hook script for anything else.
- Hook scripts run only as the agent user — a profile author never gets guest
  root code execution.
- Changes apply on VM restart automatically, and on demand into a running VM
  via `code-vm profile apply`.
- Preserve every existing security invariant, in particular: the host config
  tree is the only trusted input to the egress allowlist, and it is never
  mounted into the guest.

## Non-goals (v1)

- Claude settings fragments (merging profile-provided rules or hooks into the
  locked `settings.json`). Deferred; profiles may not touch that file at all.
- Per-profile parameters/options in `config.yaml`. Bundles are fixed; variation
  means making the bundle itself flexible or forking it.
- Version pinning/locking of git-distributed profiles. `profile update` is
  explicit, so nothing moves silently.
- Undoing removals: deactivating a profile removes its canonical tree from the
  guest, but does not uninstall packages or revert the shell.
  `code-vm recreate` is the clean-slate path.

## Decisions

| Area | Decision |
|---|---|
| Audience | Team-shared bundles, git as the distribution channel |
| Power level | Declarative core + hook scripts; hooks run as the agent user only, never root |
| Bundle location | `~/.config/code-vm/profiles/<name>/`; code-vm only ever reads the local copy |
| Activation | `profiles: [name, ...]` list in `config.yaml` — the single source of truth |
| v1 declarative surface | File tree into the agent home, apt packages, login shell, extra allowlist domains |
| Apply timing | On every boot (inside the readiness gate) and on demand via `code-vm profile apply` |
| Delivery | Rendered into the Lima template as `mode: data` entries; `profile apply` pushes the same tree via the existing staging path |
| Guest application | One root script, `apply-profiles.sh`, shared by both callers |
| Collisions | List order in `config.yaml` wins: later profiles overwrite earlier files; last declared shell wins |

## Profile bundle format

A profile is a directory:

```
~/.config/code-vm/profiles/
  fish-shell/
    profile.yaml          # manifest (required)
    files/                # optional file tree, mirrors the agent's home
      .config/fish/config.fish
    hook.sh               # optional, referenced from the manifest
  wetf-claude/
    profile.yaml
    files/.claude/CLAUDE.md
```

`profile.yaml`:

```yaml
description: Fish as the agent's shell, with fisher and plugins
packages: [fish]              # apt, installed as root during provisioning
shell: /usr/bin/fish          # agent's login shell; must come from a listed
                              # package or already exist in the guest
domains:                      # merged into the Squid allowlist
  - raw.githubusercontent.com
hook: hook.sh                 # run as the agent user, after files and
                              # packages, through the proxy
```

Validation, enforced host-side before anything is rendered:

- Profile names match the instance-name pattern
  (`[a-zA-Z0-9][a-zA-Z0-9-]{0,62}`) — they appear in guest paths and env
  files.
- Every manifest key is optional, but a profile with an empty manifest and no
  `files/` tree is an error.
- `domains` go through the existing `ValidateDomain` — they are interpolated
  into Squid `acl` lines.
- `packages` match a conservative apt package name regex — they reach a root
  apt invocation via `manifest.env`.
- `shell` must be an absolute path.
- `files/` entries must resolve to cleaned relative paths inside the agent
  home: no `..`, no absolute paths, no symlinks escaping the tree. A profile
  can write anywhere under `/home/<agent>/` and nowhere else.
- A profile may not ship `.claude/settings.json` or
  `.claude/settings.local.json`; those collide with `lock-settings.sh` and are
  rejected at validation.

Profile-shipped files are installed **agent-owned and writable** (0644, or
0755 when the source is executable). They are convenience content, not
security config — unlike the root-locked `settings.json`.

## Host config and CLI

`config.yaml` gains one key:

```yaml
profiles:
  - fish-shell
  - wetf-claude
```

Order is preserved and meaningful. `Config.Validate` checks each name against
the pattern and that the profile directory exists with a parseable, valid
manifest — a broken or missing profile fails at config load, not mid-boot.

New `code-vm profile` command group:

- `profile add <git-url> [name]` — clones into
  `~/.config/code-vm/profiles/<name>/` (name defaults to the repo basename),
  validates the manifest, prints the trust warning (see Security) and a hint
  to add the name to `config.yaml`. Does not activate.
- `profile update [name]` — `git pull` in one or all profile directories that
  are git clones, re-validating after. Non-git directories are skipped with a
  note; hand-authored local profiles are first-class.
- `profile list` — name, description, active/inactive, git origin if any.
- `profile remove <name>` — deletes the directory; refuses while the name is
  still listed in `config.yaml`.
- `profile apply` — pushes all active profiles into the running VM and runs
  the guest applier. Errors when the VM is not running.

## Delivery and guest layout

Both delivery paths land content in one guest location, so the applier never
knows how it arrived:

```
/usr/local/share/sandbox-profiles/
  manifest.env                    # rendered: ordered profile list, package
                                  # union, shell, hook list — quoted values
  fish-shell/
    files/...
    files.list                    # the file paths this profile version ships
    hook                          # hook script, normalized name
  wetf-claude/
    files/.claude/CLAUDE.md
    files.list
```

The applier consumes only what `manifest.env` and each profile's `files.list`
name. This is what keeps stale guest state inert: `mode: data` delivery can
overwrite files but never delete them, so a file (or hook) removed from a
profile would otherwise linger on the persistent disk and keep being applied
on every boot. `manifest.env` is always delivered — even with zero active
profiles — so deactivating every profile also deactivates application.

**Path 1 — template render.** The rendered Lima config is already re-applied
on every start (`ensureRunning` replaces the stored `lima.yaml`), so profiles
ride the existing channel: `renderParams` loads active profiles and appends
their contents as `mode: data` entries (root-owned, 0444). Profile `domains`
are appended to `EXTRA_ALLOWED_DOMAINS` in `provision.env`, so the boot-time
firewall path is unchanged. Packages and shell go into `manifest.env`, keeping
profile inputs in one file.

**Path 2 — `profile apply`.** The host clears
`/usr/local/share/sandbox-profiles/` (so deactivated profiles disappear), then
pushes the same rendered tree through the existing `installContent` staging
path — random staging name, root install, no agent-tamperable window. Profile
domains are merged into `session.FragmentContent` alongside `extraDomains`, so
`ApplyAllowlist` works unchanged.

After `profile apply` the running VM matches the config; the next restart
re-renders from the same config. The two paths converge at every boundary —
no markers, no migration state.

## Guest application — `apply-profiles.sh`

One root-run script, `/usr/local/lib/sandbox/apply-profiles.sh`, owns all
profile semantics. Called identically by `sandbox-boot.sh` at boot and by
`code-vm profile apply` via the admin channel. Sources `provision.env` and
`manifest.env`.

**Packages** are installed in two places so both callers are covered.
`provision-system.sh`, which already owns apt, additionally sources
`manifest.env` and installs the union of profile packages at boot.
`apply-profiles.sh` repeats the same missing-only install as its first step,
which is a no-op at boot (provisioning just ran) and is what makes
`profile apply` work on a running VM. No allowlist changes are needed for
either path: apt runs as root, and root egress is direct by design (the
firewall's `--uid-owner 0 ACCEPT` rule exists exactly for provisioning-class
work).

`apply-profiles.sh` steps, in order (step 0 being the package install above):

1. **Files** — for each active profile in list order, install the files named
   in its `files.list` from `files/` into `/home/<agent>/`, chown to
   `AGENT_UID:AGENT_GID`, mode 0644/0755. Later profiles overwrite earlier
   ones. Parent directories are created agent-owned one segment at a time, and
   any symlinked segment (or destination) aborts the step: the installs run as
   root, so a symlink the agent planted in its home must not be able to
   redirect one outside it. Files are re-installed on every boot and every
   apply: profile-shipped files are canonical (same philosophy as
   `lock-settings.sh`), so local edits to them do not survive a restart.
2. **Shell** — `chsh -s <shell>` for the agent user; last profile declaring a
   shell wins. Before switching, verify the path exists and is registered in
   `/etc/shells` (adding it if the package installed it) — a bad shell would
   lock the agent out of every session.
3. **Hooks** — run each profile's hook in list order as the agent user via the
   `setpriv` pattern the boot script already uses: `HOME`, proxy env, sane
   `PATH`, cwd `/home/<agent>`. Hooks run after files and packages, behind the
   firewall, so network access works iff the profile declared its domains.
   Hooks must be idempotent — they run on every boot and every apply; this is
   the documented contract for profile authors.

**Failure semantics.** File and shell steps abort with an error — they are
local and deterministic, so failure means a broken profile. A hook failing at
boot logs a loud warning but does not trip the boot-failure marker: a flaky
download must not brick an otherwise safe VM. Via `profile apply`, any
failure including hooks surfaces as a non-zero exit.

**Boot ordering:** `lock-settings → init-firewall → connectivity check →
apply-profiles → update-agent-clis → done-marker`. Profiles sit inside the
readiness gate: `code-vm` returns only once the VM is fully shaped.

## Security

One new trust statement, stated to the user by `profile add`:

> A profile is host-trusted input, like `config.yaml` itself. Installing a
> profile means trusting its author with: the agent's home directory contents,
> apt package selection, additions to the egress allowlist, and arbitrary code
> execution as the agent user (hooks).

Bounds on that trust:

- Hooks never run as root; packages and shell are interpreted from validated
  declarative fields, not scripts.
- Profiles cannot touch the locked Claude settings (validation rejects those
  paths).
- Profiles live under `~/.config/code-vm/`, which `MountsExclude` already
  protects from being mounted — the agent cannot edit profile sources, so it
  cannot widen its own egress through them. The agent may *read* delivered
  profile content in the guest (it is destined for its own home anyway).
- All injection surfaces validate at load time, matching the existing pattern:
  domains via `ValidateDomain` before Squid, package names via strict regex
  before root apt, profile names via the name regex before paths and env
  files, file paths cleaned and confined to the agent home, `manifest.env`
  values shell-quoted.
- Hook stdout/stderr lands in the boot journal / apply output, and hook
  network traffic goes through the proxy log like all other traffic.

## Testing

Follows the repo's existing style: table-driven unit tests, golden files, fake
runner, plus the integration script.

- **Manifest loading/validation** — table tests for good and bad manifests:
  bad domains, `..` in file paths, `settings.json` smuggling, bad package
  names, empty profile, missing hook file.
- **Template rendering** — extend the golden-template test with a fixture
  profile; assert the `mode: data` entries, `manifest.env` content, and merged
  `EXTRA_ALLOWED_DOMAINS`.
- **Session/apply path** — fake-runner tests asserting the staged-install
  command sequences for `profile apply`, mirroring `allowlist_test.go`; assert
  profile domains are merged into the allowlist fragment content.
- **Config** — `profiles` list validation, including the missing/broken
  profile case; `MountsExclude` still covers the profiles directory.
- **CLI** — `profile add/list/remove/update` against temp directories with
  local git fixtures; `remove` refusing while active.
- **Integration** — extend `test-vm-sandbox.sh` with a fixture profile
  carrying a file, a package, a shell and a hook; assert all four landed, then
  deactivate, apply, and assert the canonical tree is gone from the guest.
