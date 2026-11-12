# Profile Sources — Design

**Date:** 2026-11-12
**Status:** Approved 2026-11-12, implemented on this branch

## Context

Profiles (2026-08-19 design) ship a `files/` tree that mirrors the agent's
home. That works for content written for code-vm, but not for repositories
that exist on their own terms. The motivating example is `wetf/jira-tickets`:
a repository that *is* a Claude Code skill — `SKILL.md`, `README.md` and
numbered step directories at the repo root — and whose own README says to
symlink the whole checkout to `~/.claude/skills/jira-ticket`. It has no
`profile.yaml` and no `files/` tree, and adding either would mean forking it
or bending a shared repository around one consumer.

This design extends profiles with **sources**: plain git repositories a
profile pulls in and places, as a whole, at a directory under the agent's
home. The source repository stays untouched; the referencing profile decides
where its content lands.

Two entry points, one mechanism:

- **Team composition.** The shared wetf profile lists the jira-tickets repo as
  a source. Everyone who activates that profile gets the skill on their next
  `profile update` and restart, with no per-user steps.
- **One-off on a single machine.** `code-vm profile add <url> --into <path>`
  pulls any repository in without editing a manifest by hand.

## Goals

- A profile can declare git repositories to place at chosen directories
  under the agent's home, without the source repository knowing about
  code-vm.
- Each source can be pinned to a commit or tag, or track a branch. Pinning
  is the recommended default for team profiles.
- Nothing moves silently: only `profile add` and `profile update` touch the
  network. `code-vm start` and `profile apply` deliver what is already on
  disk.
- Both delivery paths (template render at boot, staged push on
  `profile apply`) deliver sources identically, so the guest converges on
  the same state either way.
- An unreachable repository never blocks the VM: a source that cannot be
  fetched or no longer resolves is skipped with a warning, and everything
  else in the profile still applies.
- Every existing security invariant holds: sources are host-trusted input
  exactly like profile files; they reach the agent's home and nothing else.

## Non-goals (v1)

- A subdirectory of a source repository as the thing to place. The whole
  repository lands, nothing less. Trivially addable later as a `path:` key.
- Sources contributing packages, domains, hooks, shell or templates. A
  source is inert content; those capabilities stay with the profile.
- Transitive sources (a source repository that is itself a profile).
- Non-git sources (tarballs, local directories).
- A `profile sources` command group. `add`, `update`, `list` and `remove`
  learn about sources; no new verbs.

## Decisions

| Area | Decision |
|---|---|
| Where declared | `sources:` list in `profile.yaml`; each entry has `repo`, `into`, optional `ref` |
| Ref semantics | Branch or tag tracks on `profile update`; a commit pins until the manifest changes; no `ref` means the remote's default branch |
| Destination ownership | A source owns its destination directory outright: wiped and refilled on every boot and every apply |
| Local checkout location | `~/.config/code-vm/sources/<profile>/<source>/`, a bare clone managed by code-vm, never mounted |
| Content extraction | `git archive` of the resolved commit, so only tracked content ships: no `.git`, no ignored files, symlinks archived as links and never followed on the host |
| Guest delivery | One base64-encoded gzip tarball per source, delivered through the existing `mode: data` / staged-push channels |
| Guest application | A new "sources" step in `apply-profiles.sh`, before files, running with the agent's own privileges |
| Collisions | Rejected at load: no file, template or other source of any active profile may land inside a source's destination |
| One-off entry point | `profile add <url> --into <path> [--ref <ref>] [name]` generates a local wrapper profile with one source |

## Manifest

```yaml
description: wetf team defaults
sources:
  - repo: https://github.com/wetransform/jira-tickets
    into: .claude/skills/jira-ticket
    ref: 3f2a9c1e7d0b4a5f8c6e2d1b9a7f3e5c4d2b1a0f   # commit: pinned until edited
  - repo: git@github.com:wetransform/some-skill.git
    into: .claude/skills/some-skill
    ref: main                                        # branch: tracks on update
```

Fields:

- `repo` (required): a git URL, passed verbatim to `git clone`. Any transport
  git supports; private repositories use the user's own git credentials,
  exactly as `profile add` already does.
- `into` (required): directory relative to the agent's home where the
  repository content lands.
- `ref` (optional): branch name, tag name or full commit hash. Resolution
  order on update: `origin/<ref>` (branch), then `<ref>` as a tag or commit.
  Omitted means the remote's default branch (`origin/HEAD`).

Each source gets a **name** derived from the last path segment of `into`
(`jira-ticket` above): every character outside `[a-zA-Z0-9-]` becomes `-`,
runs of `-` collapse to one, leading and trailing `-` are trimmed, and the
result is cut to 63 characters, so `.oh-my-zsh` names `oh-my-zsh`, `.vim`
names `vim` and `jira_tickets` names `jira-tickets`. Names are used for the
checkout directory and in guest paths, so the result must pass
`ValidateName` (a segment such as `...` that leaves nothing is rejected).
Two sources in one profile with the same name (`tools/a_b` and
`tools/a-b`) are rejected, naming both `into` values; authors pick a
different directory name.

### Validation (host-side, at load)

- `repo`: non-empty, single-line printable (it is printed by `profile list`).
  Not otherwise parsed; git is the authority.
- `ref`: if present, matches `^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`, does not start
  with `-` (would be taken as a git option), contains no `..`. Printed and
  passed to git as a single argument.
- `into`: matches `relPathRe`, cleaned, no `..` segment, not `.` and not
  empty. Must have at least one segment. No forbidden file
  (`.claude/settings.json`, `.claude/settings.local.json`) may lie at or
  under `into`: a wipe of `.claude` would destroy the locked settings.
  Consequently `into: .claude` is rejected while `.claude/skills/x` is fine.
- The checkout at `sources/<profile>/<source>/` should exist and resolve
  `ref`. When it does not — never fetched, fetch failed, ref deleted
  upstream and not yet refetched — the source is **unavailable**. That is
  not a load error: the profile loads, the source is skipped, and a warning
  names the source, the reason and the fix (`run code-vm profile update
  <profile>`). See "Unavailable sources" below.
- The archive must be non-empty (an empty repository is an error), at
  most 8 MiB compressed and at most 64 MiB uncompressed. The compressed cap
  exists because the archive is embedded in the Lima template on every
  start and in the cidata image; the uncompressed cap bounds what the guest
  expands onto its disk after wiping the destination, since gzip can shrink
  a repetitive blob a thousandfold. Both caps are my call, and each error
  message states its cap so a builder can tune it.
- Archive entries are checked against the forbidden files after prefixing
  `into/`; a source may not smuggle the locked settings in under a
  permitted destination either.

A profile that declares only sources is a valid profile (the "declares
nothing" check gains `sources` as one more thing a profile can declare).

### Cross-profile collisions

`LoadAll` already tracks every `Rel` shipped by files and templates. It
gains the set of source destinations and rejects, with a message naming both
sides:

- a file or template of any active profile whose `Rel` lies at or under a
  source destination;
- a source destination at, under or above another source destination.

Rationale: the sources step wipes its destination, so an overlapping file
would be re-created or destroyed depending on step order, and the two
delivery paths cannot be guaranteed to agree. Rejecting outright matches
how files/templates collisions are treated today.

## Host side

### Checkout management

`~/.config/code-vm/sources/<profile>/<source>/` holds a bare clone
(`git clone --bare`). Derived from the config path like `ProfilesDirFor`,
so `--config` test setups stay isolated. `loadConfig` guards it with
`MountsExcludeTree`, exactly as it already guards the profiles and SSH
directories: the agent can never write what the host treats as trusted
input.

A bare clone, not a worktree, because nothing on the host ever needs the
files checked out: `git archive` reads objects directly. That also removes
the whole class of host-side symlink and ignored-file concerns the
`files/` loader has to defend against.

**Sync** (shared by `add` and `update`), per source:

1. Clone bare if missing. If the checkout exists but its origin differs
   from the manifest's `repo`, clone the new repository beside it and swap
   it in only once the clone succeeded: re-pointing the old checkout would
   keep its objects, and a commit pinned from the previous repository would
   go on resolving as if the new one contained it.
2. `git fetch origin` with `+refs/heads/*:refs/heads/*` and tags, pruning.
   For a `ref` that looks like a full commit hash and is not yet present,
   additionally `git fetch origin <sha>`.
3. Resolve `ref` to a commit (resolution order above). Store nothing
   extra: the bare clone plus the manifest's `ref` fully determine the
   resolved commit, and `Load` re-resolves on every run.
4. Re-run `profile.Load` for the profile and fail loudly if it no longer
   validates.

A fetch that fails (network down, missing credentials, repository moved or
deleted) **never discards an existing checkout**: the previous objects stay,
the failure is reported with the repository URL and git's own error, and the
sync continues with the next source. State is never worse after a failed
sync than before it.

A pinned commit therefore never moves on `update`, and a branch moves only
when `update` runs. `code-vm start` only resolves locally.

### Unavailable sources

An unreachable repository must never block the VM. A source is
*unavailable* when its checkout is missing (`never fetched`), incomplete (a
directory without `HEAD`, left by an interrupted clone: `checkout
incomplete`; the next sync reclones it), or its `ref` does not resolve in
the checkout. Everywhere, an unavailable source is **skipped with a
warning**, never an error:

- `Load` returns the profile with that source marked unavailable, carrying
  the reason. It is not rendered: no `sources.list` line, no archive. The
  guest step therefore does not touch its destination directory, so a
  previously applied tree stays as it was and a fresh VM simply lacks it.
- Every command that loads profiles for the VM (`start`, `shell`,
  `profile apply`, …) prints the warning to stderr once per unavailable
  source, in the form `profile <name>: source <source> unavailable
  (<reason>); run code-vm profile update <name>`. `profile list` shows the
  same state inline on the source's own line instead of warning, since the
  listing is where the user is already looking.
- The remaining parts of the profile — files, templates, packages, shell,
  hook, other sources — apply as normal.

A source that was fetched once keeps working offline for as long as its ref
resolves locally, which for a pinned commit is forever. Only moving to a new
ref needs the network.

Genuine manifest errors (bad `into`, bad `ref` syntax, collisions, an
oversize or empty archive, a forbidden file inside the archive) stay hard
load errors: those mean a broken profile, not a flaky network.

### Loading: two phases

The full `Load` requires every source checkout to exist, but the commands
that *create* those checkouts (`add`, `update`) and the one that must keep
working while they are missing (`list`) need to read the manifest first.
So the `profile` package gets a manifest-only phase:

- `LoadManifest(profilesDir, name)` — reads and validates `profile.yaml`
  (including every source field rule above) and the `files/`, `templates/`
  and hook content, but does **not** touch the checkouts. Returns the
  `Profile` with `Sources` carrying spec fields only, no commit or archive.
- `Load(profilesDir, name)` — `LoadManifest` plus, per source, resolve the
  commit against the bare clone and build the archive. This is what config
  validation, `start`, `shell` and `profile apply` call. A source whose
  checkout is missing or whose ref does not resolve is marked unavailable
  rather than failing the load (see "Unavailable sources").

Command flows (my call, settled so the motivating case works end to end):

- `profile add <url> [name]`: clone, `LoadManifest`, **sync every declared
  source**, then full `Load`. A manifest or validation failure removes the
  clone and the `sources/<name>/` directory again, mirroring today's
  invalid-clone cleanup. A source that cannot be fetched does **not** fail
  the add: the profile is installed, the failure is reported with the
  repository URL and git's error, and the source stays unavailable until a
  later `profile update` succeeds. Adding the team profile on a working
  network therefore leaves a machine fully ready; "network-minimal add" is
  not a goal.
- `profile add --into`: write the wrapper manifest, then the same
  sync-and-`Load` path; cleanup on a validation failure removes the wrapper
  too. A failed fetch keeps the wrapper, as above.
- `profile update [name]`: pull if a git clone, `LoadManifest`, sync every
  source, full `Load`. A fetch failure keeps the existing checkout and is
  reported; the update continues with the remaining sources and profiles
  and exits non-zero at the end with a one-line summary of what failed.
- `profile list`: `LoadManifest` for the description and source lines, then
  full `Load` to decide `active`/`inactive` vs `invalid`. An unavailable
  source does not make the profile `invalid`; its line shows
  `unavailable: <reason>` in place of a commit.

`profile.Load` gains `Sources []Source` on `Profile`:

```go
type SourceSpec struct { Repo, Into, Ref string }      // manifest
type Source struct {
    Name, Into, Repo, Ref string
    Commit      string  // resolved; empty when unavailable
    Archive     []byte  // gzip tarball from git archive; nil when unavailable
    Unavailable string  // reason, e.g. "never fetched" or "ref main not found"; empty when fine
}
```

Loading a source runs `git -C <bare> archive --format=tar <commit>` and
gzips the result in Go. `git archive` honours `export-ignore` attributes,
which is the right behaviour: a repository that marks files as not for
export does not want them shipped.

### Rendering

`GuestFiles` emits, per profile with sources:

```
/usr/local/share/sandbox-profiles/<profile>/
  sources.list                    # one line per source: "<name> <into>"
  sources/<name>.tar.gz.b64       # the archive, base64, 76-column lines
```

`manifest.env` gains `PROFILE_SOURCES="<profiles that ship sources>"`,
following the `PROFILE_FILES` pattern: the always-overwritten manifest gates
which `sources.list` files the applier reads, so a stale list or archive a
later profile version dropped stays inert.

Both delivery paths carry these files unchanged: `mode: data` entries at
boot (root-owned 0444) and the staged push on `profile apply`, which already
clears the profile root first.

`profile apply` also brings `apply-profiles.sh` itself up to the running
build before invoking it: after the boot-complete marker is present it
compares the guest copy's hash with this binary's, and on a mismatch stages
the new script root-owned beside the live path and renames it over, so the
canonical path is never absent for a concurrent exec. The applier is
otherwise delivered only by the Lima template at boot, so a VM booted from
an older code-vm would keep running an applier without the sources step
while the pushed tree already carried `sources.list`: the inputs would be
silently ignored until the next restart. This departs from `config apply`,
which verifies `lock-settings.sh` and refuses on a mismatch; it is sound
only while the applier stays self-contained against the pushed tree,
`provision.env` and the base image, and must revert to verify-and-refuse
if it ever depends on a newer boot script.

### CLI changes

- `profile add <git-url> [name]` unchanged for real profiles. With
  `--into <path>` it instead **generates a local wrapper profile**:
  `profiles/<name>/profile.yaml` with `description: <url> → ~/<path>` and one
  source (`repo`, `into`, and `ref` if `--ref` was given), then syncs the
  source. Name defaults to the repository basename. The trust warning is
  printed as today; the activation hint is the same. A wrapper is an
  ordinary hand-authored profile afterwards: not a git clone, editable,
  listed as `local`.
- `profile update [name]` now does two things per profile: pull if the
  profile directory is a git clone (as today), then sync every declared
  source. Non-git profiles are no longer skipped outright; they just skip
  the pull. A fetch failure for one source is reported and the update
  continues; the existing checkout is kept; the command exits non-zero at
  the end with a summary naming every source that failed. Without a name,
  a profile whose pull fails is reported the same way and skipped (its
  sources are not synced, since its manifest may be stale); named
  explicitly, a failed pull aborts.
- `profile list` appends one indented line per source under its profile:
  `name`, `into`, `ref` (or `default`) and the resolved short commit. An
  unavailable source shows `unavailable: <reason>` instead of a commit; the
  profile itself stays `active` or `inactive`.
- `profile remove <name>` also deletes `sources/<name>/`.
- `profile apply` needs no change beyond what `GuestFiles` already renders.

### Trust statement

`profile add` prints the existing trust warning. One sentence is added:

> A profile's sources are pulled from the repositories its author names and
> placed in the agent's home as-is; trusting the profile means trusting
> those repositories at the refs it pins.

## Guest application

`apply-profiles.sh` gains a **sources** step, placed after packages and
before files:

```
packages → sources → files → shell → hooks
```

Sources go before files so a hook (which runs last) sees the complete tree,
and before files so the step order is irrelevant in practice: overlaps are
rejected at load.

For each profile in `PROFILE_SOURCES`, for each `<name> <into>` line in its
`sources.list`:

1. `src=$PROFILE_ROOT/<profile>/sources/<name>.tar.gz.b64`,
   `dst=$AGENT_HOME/<into>`.
2. As the agent (`run_as_agent_sh`, positional arguments, same hardening as
   the files step): `rm -rf "$dst" && mkdir -p "$dst" && base64 -d "$src" |
   tar -xz -C "$dst"`.
3. Log `  <profile>: ~/<into> (source <name>)`.

Why this is safe with the agent's own privileges, as the files step already
argues: the source archive is root-owned and world-readable, and the
install touches only the agent's home with the agent's own access. A symlink
the agent plants at `dst` or a parent segment only redirects a write the
agent could already make. Tar runs as the agent, so extracted entries are
agent-owned; tar's default for a non-root user never chowns. Symlinks
inside the archive are created as symlinks and point wherever the repository
author pointed them, which is content the agent could have created itself.

Wiping the destination is what makes removals upstream show up in the guest
and makes the destination canonical, matching the "profile-shipped files are
canonical" rule. Deactivating the profile stops applying the source but
leaves the directory in place, exactly as with files today; `code-vm
recreate` is the clean-slate path.

Failures in this step abort with an error, like files: they are local and
deterministic. An unavailable source never reaches this step at all: the
host leaves it out of `sources.list`, so its destination is neither wiped
nor filled.

## Security

New surfaces, each validated host-side before anything is rendered:

- `repo` and `ref` reach `git` as single argv entries, never a shell. `ref`
  may not start with `-`.
- A local `repo` must be an absolute path or a `file://` URL (checked
  after percent-decoding, as git decodes it) with no `..` segment, must
  resolve, and every prefix of it must resolve outside every shared
  directory. The check judges the URL as git will open it, after the user's
  `url.<base>.insteadOf` rewrites (`git ls-remote --get-url`, which expands
  without contacting anything), so a rewrite to a local path cannot slip a
  remote-looking manifest value past it. `profile add` and `profile update`
  refuse anything else before any git command runs, because fetching runs
  `git upload-pack` in that
  repository under its own config. Checkouts clone with `--no-local` so
  its `objects/info/alternates` is never copied, and every network git
  command runs in `sources/<profile>/` rather than the user's working
  directory, where a planted `git@host:org/repo.git` directory would win
  over the scp-like URL. The profile bundle's own URL is held to the same
  rule: `profile add` refuses it, `profile update` refuses to pull from
  such an origin, and the bundle clone runs in the profiles directory.
- Source names are sanitised from the last segment of `into` to
  `[a-zA-Z0-9-]` and go through `ValidateName` before they appear in paths
  or `sources.list`.
- `into` goes through the same confinement as `files/` paths, plus the
  "may not contain a forbidden file" rule because the step wipes a
  directory rather than installing a file.
- Archive contents are generated by `git archive` from a bare clone the
  agent cannot reach, so there is no host-side symlink to follow, nothing
  to read outside the repository, and no untracked host state leaking in.
- The guest step runs with agent privileges only; no new root write into
  the agent home is introduced.
- The sources directory gets its own `MountsExcludeTree` guard in
  `loadConfig`, alongside the profiles and SSH directories.
- `profile update` deletes checkouts for sources a profile no longer
  declares, so a dropped source cannot linger as stale trusted input.

## Documentation

README "Profiles" section gains a short **Sources** subsection: the manifest
snippet, the `--into` one-liner, the ref semantics with a recommendation to
pin, and the "the destination is wiped on every apply" warning in the
notes list. The profile-manifest section of `profile.yaml` docs lists
`sources` alongside the other keys.

## Testing

Follows the repo's existing style.

- **Manifest/validation** — table tests: good sources; `into` of `.`,
  `..`, `.claude`, absolute; `ref` starting with `-`; duplicate source
  names; oversize archive; forbidden file smuggled inside the archive.
- **Unavailable sources** — a missing checkout and an unresolvable ref each
  load successfully with the source marked unavailable and the reason set;
  rendering omits the source from `sources.list` and ships no archive;
  the other sources and files of the same profile still render.
- **LoadAll collisions** — a file under a source destination; nested source
  destinations across profiles.
- **Archive loading** — against a local bare repository fixture built in a
  temp dir with `git`: a tracked file, an executable, a symlink, an
  `export-ignore`d file (absent from the archive), a pinned commit vs a
  moved branch.
- **Rendering** — golden-template fixture profile with a source: the
  `sources.list` entry, the base64 archive entry, and `PROFILE_SOURCES` in
  `manifest.env`.
- **CLI** — `add --into` generates the expected wrapper manifest and
  checkout; `add` with an unreachable repository URL still installs the
  profile and reports the source; `update` syncs a pinned commit without
  moving and a branch with moving; `update` against an unreachable remote
  keeps the old checkout, continues with the next source and exits non-zero
  with a summary; `remove` deletes the sources directory; `list` shows the
  commit, or `unavailable` with the reason.
- **Integration** — extend `test-vm-sandbox.sh`: a fixture profile with a
  source pointing at a local bare repo; assert the tree landed at `into`
  with the executable bit intact; remove a file upstream, update, apply,
  assert it is gone; plant a stray file in the destination, apply, assert
  it is gone.

## Left to the builder

- Exact git invocations for the sync step, as long as the ref semantics
  above hold and nothing moves on `start`.
- Whether `Source.Archive` is built on every `Load` or cached; correctness
  first, since `Load` runs on every command that reads the config.
- Output formatting of `profile list` source lines.
- The 8 MiB cap value, if the Lima template or cidata image turns out to
  tolerate less or comfortably more.
