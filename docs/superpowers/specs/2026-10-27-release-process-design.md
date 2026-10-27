# Release process for code-vm

## Intent

Team members should be able to install `code-vm` on their own machine
without cloning the repository and building it with mise and Go. A release
is a tagged version with a GitHub release that carries ready-to-run binaries
for the platforms the project supports.

Installing should work by downloading an archive from the release page, and
should also be possible through mise (`mise use github:wetransform-os/code-vm`),
so release artifacts follow the naming that tools like mise discover
automatically. An AUR package is a possible later step and should not need
the release layout to change.

## Goals

- A manually triggered GitHub Actions workflow that cuts a release: works out
  the next version from conventional commits, updates `CHANGELOG.md`, makes a
  release commit, tags it `vX.Y.Z`, and creates a GitHub release with notes.
- The release carries one archive per target platform and a SHA-256
  checksum file.
- The binary knows its version: `code-vm --version` prints it.
- A dry-run mode that shows the next version and release notes without
  releasing anything.
- The README explains how to install from a release.

## Non-goals

- Automatic releases on every merge to `main`. A person decides when to
  release by running the workflow.
- AUR, Homebrew or other package-manager publishing (possible follow-up).
- macOS code signing and notarization. Binaries ship unsigned.
- Intel macOS, Linux arm64 and Windows binaries. The README does not list
  them as supported, or the team does not use them.

## Anti-goals

- A release mechanism that works differently from the team's other repos
  without good reason. The shared workflows in `wetransform/gha-workflows`
  are the default.
- Requiring the person cutting a release to pick a version number or edit
  files by hand.
- Release binaries that differ from what `mise run build` produces, other
  than the stamped version and the target platform.

## Decisions

### Mechanism: the shared `mise-release` workflow

Releases use the reusable workflow
`wetransform/gha-workflows/.github/workflows/mise-release.yml`, which wraps
semantic-release. It already handles the version calculation (conventional
commits), `CHANGELOG.md`, the `chore(release): x.y.z` commit, the tag, the
GitHub release with Jira-linked notes, and a `dry-run` input. Its conventions
for consumers:

- mise environment `ci` (`MISE_ENV=ci`).
- A mise task `ci:verify-release`, run before anything changes.
- A mise task `ci:set-version --version <version>`, run during the prepare
  step. Files to attach to the GitHub release must exist when it finishes.
- `.wetf-ci.yml` lists release assets under `release.assets` as
  `- path: <glob>`.
- Secrets `WE_RELEASE_GITHUB_APP_ID` and `WE_RELEASE_GITHUB_PRIVATE_KEY`
  (a GitHub App token is used to push the release commit and tag). Both are
  available to this repository, and the release app may push past branch
  protection.

The workflow file is not hand-written. Like the repo's other `tf-*.yml`
workflows, it is managed by `wetransform/github-bootstrap`, which generates
`.github/workflows/tf-release.yml` from `workflow-templates/release.mise.tfpl.yml`.
This repo opts in by adding `.wetf-repo.yml`:

```yaml
presets:
  - mise
  - workflows-release
```

- `mise` selects the mise profiles. `workflows-release` turns on only the
  release workflow. `workflows-default` is deliberately not used, because it
  would also generate check and publish workflows, and the existing `ci.yml`
  already covers checks.
- The template's defaults need no change: trigger is manual only (no
  `automatic-release` preset), the input is `dryRun`, there is a concurrency
  group and a fork guard, and the `WE_RELEASE_*` secrets are passed explicitly
  for `wetransform-os`. The mise release profile's default inputs are empty,
  which gives the non-npm variant.
- The workflow file shows up once github-bootstrap is applied after this
  change, which happens outside this repository. Until then, the mise tasks
  and `.wetf-ci.yml` can be checked locally.

GoReleaser was considered. It is not used now, because the shared workflow
already covers tagging and releasing and a plain cross-compile is enough for
two targets. If AUR or Homebrew packaging comes later, GoReleaser can take
over the build-and-package step inside `ci:set-version`, while the shared
workflow keeps doing the tag and release.

### First version

There are no tags yet. semantic-release's default applies: the first release
is `v1.0.0`, and its notes cover the whole history. No seed tag is pushed.

### Targets

| OS     | Arch  | Why                                   |
|--------|-------|---------------------------------------|
| linux  | amd64 | README requires x86_64 with KVM       |
| darwin | arm64 | Apple Silicon with HVF                |

Builds are cross-compiled from the Ubuntu runner with `CGO_ENABLED=0`.

### Artifacts

- Archive per target: `code-vm_<version>_<os>_<arch>.tar.gz` (version without
  the leading `v`). Each one holds the `code-vm` binary at the archive root,
  plus `LICENSE` and `README.md`.
- `checksums.txt`: SHA-256 for every archive, in `sha256sum` format.
- All of it goes in a release output directory under `dist/` (already
  git-ignored), so semantic-release's git plugin never commits it.

### Version stamping

- The version is injected at build time through `-ldflags -X` and shown
  through cobra's `Version` field on the root command, so `code-vm --version`
  works.
- Builds without a stamped version (`mise run build`, `go build`) report a
  non-release version such as `dev`. Whether to add VCS details from
  `runtime/debug.ReadBuildInfo` is the builder's call.

### mise tasks

- `ci:verify-release`: run the same checks CI runs (`fmt-check`, `lint`,
  `test:unit`), so a release cannot be cut from a broken tree.
- `ci:set-version --version <v>`: build all targets with the version stamped
  in, package the archives, and write `checksums.txt`. No source file holds
  the version, so this task changes nothing that gets committed.
- Developers should be able to run the packaging step locally, for example
  `mise run release:build --version 0.0.0-test`, to check the archives
  without running the workflow. `ci:set-version` can delegate to it.

### `.wetf-ci.yml`

New file at the repo root that lists the archive glob and `checksums.txt`
under `release.assets`.

### README

Add an "Install from a release" section ahead of the build-from-source steps:

- download, verify the checksum, extract, and install to `/usr/local/bin`
- the mise route (`mise use -g github:wetransform-os/code-vm`). The
  repository is public, so no token is needed.
- macOS: binaries are unsigned. A browser download is quarantined by
  Gatekeeper. Clear it with `xattr -d com.apple.quarantine code-vm` or allow
  it in System Settings. curl and mise downloads are not affected.
- how to check the installed version (`code-vm --version`)

Add a short note for maintainers on how to cut a release: run the "Release"
workflow, with dry-run first if you want a preview.

## Constraints

- Conventional commits are already enforced in CI. They drive the version
  and the release notes, and the commit types `feat`/`fix` (or a breaking
  change) decide whether there is a release at all.
- The release commit is pushed to `main` by the release GitHub App, so the
  app must be allowed to push past any branch protection, as in other repos.
- New tools and actions use their latest releases, managed with mise where
  they are command-line tools.

## Left to the builder

- Exact task names other than the two required `ci:*` tasks.
- Where the version variable lives (`main` or `internal/cli`).
- How the cross-compile and packaging loop is written (a mise file task or
  an inline script), as long as it passes shellcheck/shfmt if it is shell.
- Whether `mise.ci.toml` is needed. The shared workflow sets `MISE_ENV=ci`
  but does not require the file.
