# Profile Sources Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A profile can declare plain git repositories (`sources:`) that land, as a whole, at a chosen directory under the agent's home; `profile add --into` wraps a single repository in a local profile; an unreachable repository warns and is skipped, never blocking the VM.

**Architecture:** `internal/profile` gains a `sources` manifest section, a bare-mirror checkout manager (`source.go`: sync, ref resolution, `git archive` → gzip), and a two-phase load (`LoadManifest` without checkouts, `Load` with them). Rendering ships one base64 gzip tarball per available source plus a `sources.list`, gated by `PROFILE_SOURCES` in `manifest.env`. `apply-profiles.sh` gains a sources step (wipe + extract as the agent) before files. The CLI syncs on `add`/`update`, prints unavailable-source warnings wherever profiles load, and guards the sources directory from mounts.

**Tech Stack:** Go 1.26.5, Cobra, `gopkg.in/yaml.v3`, `os/exec` git, `archive/tar` + `compress/gzip` (stdlib), bash guest script (shellcheck/shfmt-clean), table-driven tests with real git fixtures, `test-vm-sandbox.sh`.

**Spec:** `docs/superpowers/specs/2026-11-12-profile-sources-design.md`

## Global Constraints

- A source is inert content: it contributes no packages, domains, shell, hook or templates. Only its tree reaches the agent home.
- `into` is confined like `files/` paths (`relPathRe`, cleaned, no `..`, not `.`/empty) and additionally may not have a forbidden file (`.claude/settings.json`, `.claude/settings.local.json`) at or under it. Archive entries prefixed with `into/` are checked against the same forbidden set.
- `ref` matches `^[a-zA-Z0-9][a-zA-Z0-9._/-]*$` and contains no `..`. `repo` and `ref` never start with `-` and reach git only as single argv entries, never a shell.
- Source name = last path segment of `into`, validated with `ValidateName`; unique within a profile.
- Archive = `git archive --format=tar <commit>` from the bare mirror, gzipped in Go with a zero `gzip.Header` (deterministic). Non-empty; at most **8 MiB compressed** (error text states the cap).
- Checkouts live at `<config dir>/sources/<profile>/<source>/` as `git clone --mirror` clones. Only `profile add` and `profile update` touch the network. A failed fetch never discards an existing checkout.
- Unavailable source (checkout missing or ref unresolvable) = profile loads, source carries `Unavailable` reason, nothing rendered for it, one stderr warning `profile <p>: source <s> unavailable (<reason>); run code-vm profile update <p>`. Manifest errors stay hard errors.
- Guest step order: packages → sources → files → shell → hooks. Sources step runs with agent privileges via `run_as_agent_sh`, positional args: `rm -rf "$dst" && mkdir -p "$dst" && base64 -d "$src" | tar -xz -C "$dst"`.
- Cross-profile collisions (file/template at or under a source `into`; source `into`s nested either way) are rejected in `LoadAll`; same-profile ones in `Load`.
- Verification before every commit: `mise run test:unit && mise run lint && mise run fmt-check` (plus `mise run build` before the last commit of a task touching `internal/cli`).
- Commit style: Conventional Commits (`feat:`, `test:`, `docs:`), no trailers.
- Golden file `internal/lima/testdata/golden-template.yaml` changes only in Task 5 (fixture gains a source) and nowhere else.

## Review Focus

Inputs the spec implies but no task's tests would otherwise exercise; each has its pinning test added to the owning task:

1. `repo: --upload-pack=…` or `ref: -x`: a leading `-` must be rejected at manifest load, not reach git as an option. (Task 1)
2. `into: ./.claude/skills/x/` with `./` prefix or trailing slash: must clean to `.claude/skills/x` and derive name `x`, not fail or produce `x/`. (Task 1)
3. A tracked **empty** file in a source repository must ship: the `files/` loader's blank-content rule does not apply to archives. (Task 2)
4. A source dropped from the manifest must have its checkout pruned on `profile update`, so stale trusted input does not linger. (Task 8)
5. The agent plants a symlink at a source destination before apply: `rm -rf` as the agent removes the link, not what it points at, and the tree is installed in its place. (Task 9, integration)

## File Structure

| File | Responsibility |
|---|---|
| `internal/profile/profile.go` | `SourceSpec` in `Manifest`; `Source` on `Profile`; `LoadManifest` (no checkouts) and `Load` (resolve + archive); same-profile source validation; `LoadAll` cross-profile source collisions |
| `internal/profile/source.go` (new) | `SourcesDir`, checkout paths, `SyncSources`, ref resolution, `git archive` + gzip, `Unavailable` marking, `UnavailableWarnings` |
| `internal/profile/profile_test.go`, `source_test.go` (new) | Tests; `makeSourceRepo` fixture helper |
| `internal/profile/render.go` | `sources.list`, `sources/<name>.tar.gz.b64`, `PROFILE_SOURCES` |
| `internal/lima/template_test.go`, `testdata/golden-template.yaml` | Golden fixture gains a source |
| `internal/guest/files/scripts/apply-profiles.sh` | Sources step |
| `internal/cli/root.go` | `loadConfigWithProfiles(warn io.Writer)`, sources-dir mount guard |
| `internal/cli/profile.go` | `add` (sync; `--into`/`--ref` wrapper), `update` (pull + sync + prune), `list` (source lines), `remove` (sources dir) |
| `internal/cli/profile_test.go` | CLI tests |
| `README.md`, `test-vm-sandbox.sh` | Docs, integration |

---

### Task 1: Manifest `sources` and the two-phase load

**Files:**
- Modify: `internal/profile/profile.go`
- Test: `internal/profile/profile_test.go`

**Interfaces:**
- Consumes: existing `Manifest`, `Profile`, `Load`, `ValidateName`, `relPathRe`, `forbiddenFiles`, `isSingleLinePrintable`.
- Produces:
  - `type SourceSpec struct { Repo string \`yaml:"repo"\`; Into string \`yaml:"into"\`; Ref string \`yaml:"ref"\` }`
  - `Manifest.Sources []SourceSpec \`yaml:"sources"\``
  - `type Source struct { Name, Into, Repo, Ref string; Commit string; Archive []byte; Unavailable string }`
  - `Profile.Sources []Source` (manifest order; `Into` cleaned; `Name` derived)
  - `func LoadManifest(profilesDir, name string) (Profile, error)` — today's `Load` body, plus source validation; never touches checkouts.
  - `func Load(profilesDir, name string) (Profile, error)` — in this task: `LoadManifest` only. Task 2 adds resolution.
  - `var refRe` matching `^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`
  - `func validateSources(m Manifest, files, templates []File) ([]Source, error)` (unexported)

- [ ] **Step 1: Write the failing tests**

Append to `internal/profile/profile_test.go`:

```go
func TestLoadManifestParsesSources(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "team", `
description: team
sources:
  - repo: https://example.invalid/jira-tickets
    into: ./.claude/skills/jira-ticket/
    ref: main
  - repo: git@example.invalid:x/y.git
    into: tools/y
`, nil)
	p, err := LoadManifest(dir, "team")
	if err != nil { t.Fatalf("LoadManifest: %v", err) }
	if len(p.Sources) != 2 { t.Fatalf("Sources = %d, want 2", len(p.Sources)) }
	s := p.Sources[0]
	if s.Name != "jira-ticket" || s.Into != ".claude/skills/jira-ticket" || s.Ref != "main" || s.Repo != "https://example.invalid/jira-tickets" {
		t.Errorf("source[0] = %+v", s)
	}
	if p.Sources[1].Ref != "" || p.Sources[1].Name != "y" { t.Errorf("source[1] = %+v", p.Sources[1]) }
	if s.Commit != "" || s.Archive != nil || s.Unavailable != "" {
		t.Error("LoadManifest must not resolve or archive")
	}
}

func TestLoadManifestSourcesOnlyProfileIsNotEmpty(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "s", "sources:\n  - repo: https://example.invalid/r\n    into: x\n", nil)
	if _, err := LoadManifest(dir, "s"); err != nil { t.Fatalf("sources-only profile must load: %v", err) }
}

func TestLoadManifestRejectsBadSources(t *testing.T) {
	cases := map[string]struct{ manifest string; files map[string]string; want string }{
		"into dot":          {"sources:\n  - {repo: u, into: .}\n", nil, "into"},
		"into empty":        {"sources:\n  - {repo: u, into: \"\"}\n", nil, "into"},
		"into dotdot":       {"sources:\n  - {repo: u, into: ../x}\n", nil, "agent home"},
		"into absolute":     {"sources:\n  - {repo: u, into: /x}\n", nil, "into"},
		"into claude dir":   {"sources:\n  - {repo: u, into: .claude}\n", nil, "locked Claude settings"},
		"into settings":     {"sources:\n  - {repo: u, into: .claude/settings.json}\n", nil, "locked Claude settings"},
		"repo empty":        {"sources:\n  - {repo: \"\", into: x}\n", nil, "repo"},
		"repo dash":         {"sources:\n  - {repo: --upload-pack=x, into: x}\n", nil, "repo"},
		"ref dash":          {"sources:\n  - {repo: u, into: x, ref: -x}\n", nil, "ref"},
		"ref dotdot":        {"sources:\n  - {repo: u, into: x, ref: a..b}\n", nil, "ref"},
		"ref space":         {"sources:\n  - {repo: u, into: x, ref: \"a b\"}\n", nil, "ref"},
		"duplicate name":    {"sources:\n  - {repo: u, into: a/x}\n  - {repo: v, into: b/x}\n", nil, "same name"},
		"nested own source": {"sources:\n  - {repo: u, into: a}\n  - {repo: v, into: a/b}\n", nil, "inside"},
		"own file under":    {"sources:\n  - {repo: u, into: a}\n", map[string]string{"files/a/f": "x\n"}, "inside"},
		"own template under": {"sources:\n  - {repo: u, into: a}\n", map[string]string{"templates/a/f": "x\n"}, "inside"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeProfile(t, dir, "p", tc.manifest, tc.files)
			_, err := LoadManifest(dir, "p")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/profile/ -run 'TestLoadManifest' -v`
Expected: FAIL, `undefined: LoadManifest`.

- [ ] **Step 3: Implement**

In `profile.go`: add `SourceSpec`, `Manifest.Sources`, `Source`, `Profile.Sources`, `refRe`. Rename the existing `Load` body to `LoadManifest`; after `loadTemplates`, call `validateSources(m, p.Files, p.Templates)` and assign `p.Sources`. Extend the "declares nothing" check with `len(m.Sources) == 0`. Add `Load` as a thin wrapper returning `LoadManifest` (Task 2 extends it).

`validateSources`: for each spec, index `i` in error messages (`sources[%d]: …`): `repo` non-empty, single-line printable, no leading `-`; `ref` empty or `refRe` match, no leading `-`, no `..`; `into` = `path.Clean(strings.TrimSuffix(into, "/"))`, reject `""`/`.`, reject leading `/`, `relPathRe`, no `..` segment; reject if any `forbiddenFiles` key equals `into` or has prefix `into+"/"`; name = `path.Base(into)` through `ValidateName`; duplicate names → "same name"; nested `into`s and files/templates under an `into` → "inside". The "inside" check lives in one helper reused by Task 4: `func under(rel, into string) bool { return rel == into || strings.HasPrefix(rel, into+"/") }`.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/profile/ -v`
Expected: PASS, including every pre-existing test (they call `Load`, which still works).

- [ ] **Step 5: Commit**

```bash
git add internal/profile/profile.go internal/profile/profile_test.go
git commit -m "feat(profile): parse and validate sources in the manifest"
```

---

### Task 2: Checkout resolution and archive

**Files:**
- Create: `internal/profile/source.go`
- Modify: `internal/profile/profile.go` (`Load`)
- Test: `internal/profile/source_test.go`

**Interfaces:**
- Consumes: `Source`, `Profile`, `LoadManifest`, `forbiddenFiles`.
- Produces:
  - `const MaxArchiveBytes = 8 << 20`
  - `func SourcesDir(profilesDir string) string` → `filepath.Join(filepath.Dir(profilesDir), "sources")`
  - `func checkoutDir(profilesDir, profile, source string) string` → `SourcesDir/profile/source`
  - `func resolveCommit(dir, ref string) (string, error)` — empty ref → `HEAD`; else first of `refs/heads/<ref>`, `refs/tags/<ref>`, `<ref>^{commit}` that `git rev-parse --verify --quiet` accepts; error names the ref.
  - `func archive(dir, commit string) ([]byte, []string, error)` — `git -C dir archive --format=tar commit`, gzip with zero header; returns the gzip bytes and the entry names; errors: empty tar (no entries), over `MaxArchiveBytes` (`"source archive is %d bytes compressed, over the %d byte cap"`).
  - `func forbiddenEntry(into string, names []string) string` — the first `into/<name>` that is in `forbiddenFiles`, or `""`. `loadSources` turns a non-empty result into a hard error.
  - `func loadSources(profilesDir string, p *Profile) error` — per source: missing checkout dir → `Unavailable = "never fetched"`; unresolvable → `Unavailable = "ref <ref> not found in checkout"`; else `Commit` + `Archive`. Only archive/forbidden/size failures return errors.
  - `Load` = `LoadManifest` + `loadSources`.
  - `func UnavailableWarnings(profiles []Profile) []string` — one line per unavailable source, exact text from Global Constraints.
  - Test helper `makeSourceRepo(t *testing.T) (dir string)` in `source_test.go`: a non-bare repo with `SKILL.md`, executable `bin/run`, empty file `empty`, symlink `link -> SKILL.md`, `secret.txt` marked `export-ignore` in `.gitattributes`, one commit on branch `main`, tag `v1`. Returns the path (usable as a clone URL). Exported-for-test via a `//go:build` free helper so `internal/cli` tests can copy the pattern (they already have their own git helpers).
  - Test helper `mirror(t, src, dst)` running `git clone --mirror src dst` (Task 3 replaces with `SyncSources`).

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadResolvesAndArchivesSource(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s, ref: main}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	p, err := Load(dir, "p")
	if err != nil { t.Fatal(err) }
	s := p.Sources[0]
	if s.Unavailable != "" || len(s.Commit) != 40 || len(s.Archive) == 0 { t.Fatalf("source = %+v", s) }
	entries := untar(t, s.Archive) // map[name]tar.Header, helper in this file
	if _, ok := entries["SKILL.md"]; !ok { t.Error("SKILL.md missing") }
	if entries["bin/run"].Mode&0o111 == 0 { t.Error("executable bit lost") }
	if h, ok := entries["empty"]; !ok || h.Size != 0 { t.Error("empty tracked file must ship") }
	if entries["link"].Typeflag != tar.TypeSymlink { t.Error("symlink must be archived as a link") }
	if _, ok := entries["secret.txt"]; ok { t.Error("export-ignore'd file must not ship") }
	if _, ok := entries[".git/HEAD"]; ok { t.Error(".git must not ship") }
}

func TestLoadMarksMissingCheckoutUnavailable(t *testing.T) {
	// no mirror call
	// expect: err == nil, Sources[0].Unavailable == "never fetched", Archive nil
	// expect: UnavailableWarnings(...) == []string{"profile p: source s unavailable (never fetched); run code-vm profile update p"}
}

func TestLoadMarksUnresolvableRefUnavailable(t *testing.T) {
	// mirror, then manifest ref: nope → Unavailable contains "nope", err == nil
}

func TestLoadResolvesTagAndCommitAndDefault(t *testing.T) {
	// three profiles over one mirror: ref v1, ref <full sha of HEAD>, no ref → all resolve to the same Commit
}

func TestForbiddenEntry(t *testing.T) {
	// The into rule already rejects ".claude" itself, so the archive check is
	// defense in depth; pin it directly.
	if got := forbiddenEntry(".claude", []string{"skills/x", "settings.json"}); got != ".claude/settings.json" {
		t.Errorf("forbiddenEntry = %q", got)
	}
	if got := forbiddenEntry(".claude/skills/s", []string{"settings.json"}); got != "" {
		t.Errorf("a settings.json inside a skill dir is fine, got %q", got)
	}
}

func TestLoadShipsSettingsJsonInsideSkillDir(t *testing.T) {
	// repo tracking "settings.json", into: .claude/skills/s → Load succeeds, entry present
}

func TestArchiveRejectsOversize(t *testing.T) {
	// repo with a 9 MiB file of crypto/rand bytes (incompressible) → Load error containing "over the"
}
```

Write the elided bodies in full; the comments state the fixture and the assertions.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/profile/ -run 'TestLoad(Resolves|Marks|Rejects)|TestArchive' -v`
Expected: FAIL, undefined helpers.

- [ ] **Step 3: Implement `source.go` and extend `Load`**

Use `exec.Command("git", "-C", dir, …)` with `Output()`; map a non-zero `rev-parse` to the unavailable reason, any `archive` failure to an error. Gzip via `gzip.NewWriter` with default `Header` (zero `ModTime`). Count tar entries by reading the gzipped output back with `archive/tar` in the same function to detect emptiness and forbidden names (`forbiddenEntry`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/profile/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profile/source.go internal/profile/source_test.go internal/profile/profile.go
git commit -m "feat(profile): resolve source checkouts and build archives on load"
```

---

### Task 3: Syncing checkouts

**Files:**
- Modify: `internal/profile/source.go`
- Test: `internal/profile/source_test.go`

**Interfaces:**
- Consumes: `checkoutDir`, `resolveCommit`, `Profile.Sources`.
- Produces:
  - `type SyncFailure struct { Profile, Source, Repo string; Err error }` with `Error()` = `"profile <p>: source <s> (<repo>): <err>"`.
  - `func SyncSources(ctx context.Context, profilesDir string, p Profile, out io.Writer) []SyncFailure` — per source, in order: if checkout missing `git clone --mirror --quiet <repo> <dir>`; else `git remote set-url origin <repo>` then `git fetch --prune --quiet origin`; if `Ref` is 40 hex and does not resolve afterwards, `git fetch --quiet origin <ref>` (best effort, its failure folds into the resolve error below); then `resolveCommit`; failure → append `SyncFailure`, continue. A clone that failed leaves no directory behind (`os.RemoveAll` on error). Progress lines `"<profile>: source <source> → <short commit>"` go to `out`.
  - `func PruneSources(profilesDir string, p Profile) ([]string, error)` — removes `SourcesDir/<profile>/<dir>` for every dir not named by `p.Sources`; returns removed names. Also removes the whole `SourcesDir/<profile>` when `p.Sources` is empty.

- [ ] **Step 1: Write the failing tests**

```go
func TestSyncSourcesClonesThenTracksBranch(t *testing.T) {
	// manifest ref main; SyncSources → no failures, checkout exists; Load → Commit == c1
	// commit c2 in src on main; SyncSources; Load → Commit == c2
}
func TestSyncSourcesKeepsPinnedCommit(t *testing.T) {
	// manifest ref <c1 sha>; commit c2; SyncSources; Load → Commit == c1
}
func TestSyncSourcesUnreachableKeepsCheckoutAndContinues(t *testing.T) {
	// two sources: first repo is a non-existent path, second valid.
	// SyncSources → exactly one failure, for source 1, Err non-nil; second checkout exists.
	// Then: make first valid by syncing once, then point manifest repo at a non-existent path (set-url),
	// SyncSources → failure, but Load still resolves source 1 (old objects kept).
}
func TestPruneSourcesRemovesUndeclared(t *testing.T) {
	// create SourcesDir/p/{a,b}; profile declares only a → Prune returns ["b"], a remains
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/profile/ -run 'TestSync|TestPrune' -v`
Expected: FAIL, undefined `SyncSources`.

- [ ] **Step 3: Implement**

Git runs via `exec.CommandContext`, stderr captured into the `SyncFailure.Err` text (`fmt.Errorf("git fetch: %w: %s", err, stderr)`), so the user sees git's own reason (auth, DNS, not found).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/profile/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profile/source.go internal/profile/source_test.go
git commit -m "feat(profile): sync and prune source checkouts"
```

---

### Task 4: Cross-profile source collisions

**Files:**
- Modify: `internal/profile/profile.go` (`LoadAll`)
- Test: `internal/profile/profile_test.go`

**Interfaces:**
- Consumes: `under(rel, into)` from Task 1, `shipped` map in `LoadAll`.
- Produces: `LoadAll` rejects (a) any file/template `Rel` with `under(rel, into)` for a source of any profile, in either load order, and (b) two source `into`s where either is `under` the other. Messages: `"<rel>: profile <a> ships files/<rel> inside source <into> of profile <b>; sources own their directory outright, so this cannot be resolved by order"` and `"source <into1> of profile <a> and source <into2> of profile <b> overlap"`.

- [ ] **Step 1: Write failing tests**

```go
func TestLoadAllRejectsFileUnderOtherProfilesSource(t *testing.T)   // p1 files/a/x, p2 source into a; both orders
func TestLoadAllRejectsOverlappingSources(t *testing.T)             // p1 into a, p2 into a/b; both orders
func TestLoadAllAllowsDisjointSources(t *testing.T)                 // p1 into a, p2 into ab → ok (prefix without separator)
```

Use manifests without checkouts: `LoadAll` must still succeed structurally (sources come back unavailable), so these tests need no git.

- [ ] **Step 2: Run to verify fail** — `go test ./internal/profile/ -run TestLoadAll -v` → FAIL (no error returned).

- [ ] **Step 3: Implement** — track `sourceOwners []struct{profile, into string}` across the loop; check each new profile's files, templates and sources against it, and each new source against previously shipped `Rel`s.

- [ ] **Step 4: Run** — `go test ./internal/profile/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/profile/profile.go internal/profile/profile_test.go
git commit -m "feat(profile): reject cross-profile collisions with source directories"
```

---

### Task 5: Rendering sources for the guest

**Files:**
- Modify: `internal/profile/render.go`
- Modify: `internal/lima/template_test.go` (`testProfiles`), `internal/lima/testdata/golden-template.yaml`
- Test: `internal/profile/render_test.go`

**Interfaces:**
- Consumes: `Profile.Sources`, `GuestRoot`, `manifestEnv`.
- Produces, per profile with ≥1 **available** source:
  - `GuestRoot/<profile>/sources.list` (0444): one line per available source, `"<name> <into>\n"`, manifest order.
  - `GuestRoot/<profile>/sources/<name>.tar.gz.b64` (0444): `base64.StdEncoding` of `Archive`, wrapped at 76 columns, trailing newline.
  - `manifest.env` line `PROFILE_SOURCES="<profiles with ≥1 available source>"`.
  - Unavailable sources render nothing. A profile whose sources are all unavailable is absent from `PROFILE_SOURCES` and gets no `sources.list`.

- [ ] **Step 1: Write failing tests** in `render_test.go`: extend `fixtureProfiles()` so `wetf-claude` has `Sources: []Source{{Name: "jira-ticket", Into: ".claude/skills/jira-ticket", Archive: []byte("fake-archive-bytes")}, {Name: "gone", Into: "x/gone", Unavailable: "never fetched"}}`. Assert in `TestGuestFilesLayout`: `PROFILE_SOURCES="wetf-claude"`, `sources.list == "jira-ticket .claude/skills/jira-ticket\n"`, the `.b64` path exists with perm 0444 and content equal to the wrapped base64 of the bytes, no entry for `gone`. New `TestGuestFilesOmitsSourcesWhenAllUnavailable`.

- [ ] **Step 2: Run** — `go test ./internal/profile/ -run TestGuestFiles -v` → FAIL.

- [ ] **Step 3: Implement** in `render.go`; base64 wrapping with a small `wrap76` helper.

- [ ] **Step 4: Golden** — add one available source (`Archive: []byte("golden-archive")`) to `testProfiles()` in `template_test.go`; run `UPDATE_GOLDEN=1 go test ./internal/lima/`, review the diff (expect exactly: `PROFILE_SOURCES` line, one `sources.list` entry, one `.b64` entry), then `go test ./internal/lima/` → PASS.

- [ ] **Step 5: Run all** — `mise run test:unit` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/profile/render.go internal/profile/render_test.go internal/lima/template_test.go internal/lima/testdata/golden-template.yaml
git commit -m "feat(profile): render source archives and sources.list for the guest"
```

---

### Task 6: Guest sources step

**Files:**
- Modify: `internal/guest/files/scripts/apply-profiles.sh`

**Interfaces:**
- Consumes: `PROFILE_SOURCES`, `<profile>/sources.list`, `<profile>/sources/<name>.tar.gz.b64`, `run_as_agent_sh`.
- Produces: a `# ── Sources ──` block between Packages and Files. Defense in depth on the guest side: skip with an error (exit 1) any `into` that is empty, `.`, starts with `/`, or contains a `..` segment (same `case` pattern as `install-user-file.sh`).

- [ ] **Step 1: Implement** the loop:

```bash
for name in ${PROFILE_SOURCES:-}; do
    list="$PROFILE_ROOT/$name/sources.list"
    [ -f "$list" ] || continue
    while read -r sname into; do
        [ -n "$sname" ] || continue
        case "$into" in
            "" | . | /* | */../* | */.. | ../* | ..) echo "[profiles] ERROR: unsafe source destination: $into" >&2; exit 1 ;;
        esac
        src="$PROFILE_ROOT/$name/sources/$sname.tar.gz.b64"
        dst="$AGENT_HOME/$into"
        # shellcheck disable=SC2016
        run_as_agent_sh 'rm -rf "$1" && mkdir -p "$1" && base64 -d "$2" | tar -xz -C "$1"' _ "$dst" "$src"
        log "  $name: ~/$into (source $sname)"
    done < "$list"
done
```

Header comment: why wipe (canonical directory; upstream removals propagate), why agent privileges (same argument as the files step), why unavailable sources never reach here.

- [ ] **Step 2: Verify** — `mise run lint && mise run fmt-check` → clean. `bash -n internal/guest/files/scripts/apply-profiles.sh` → ok.

- [ ] **Step 3: Commit**

```bash
git add internal/guest/files/scripts/apply-profiles.sh
git commit -m "feat(guest): install profile sources into the agent home"
```

---

### Task 7: Warnings and the sources-directory mount guard

**Files:**
- Modify: `internal/cli/root.go`, and every caller of `loadConfigWithProfiles` (`allow.go`, `mount.go`, `profile.go`, `secrets.go`, `recreate.go`, `shell.go`, `start.go`)
- Test: `internal/cli/root_test.go`

**Interfaces:**
- Consumes: `profile.UnavailableWarnings`, `profile.SourcesDir`, `config.MountsExcludeTree`.
- Produces:
  - `func loadConfigWithProfiles(warn io.Writer) (config.Config, []profile.Profile, string, error)` — after `LoadAll`, writes each `UnavailableWarnings` line to `warn`. Callers pass `cmd.ErrOrStderr()`.
  - `loadConfig` adds `c.MountsExcludeTree(profile.SourcesDir(config.ProfilesDirFor(path)), "the code-vm profile sources")` after the profiles guard.

- [ ] **Step 1: Write failing tests** in `root_test.go`:

```go
func TestLoadConfigRefusesMountOverSourcesDir(t *testing.T)   // extra mount == <cfgdir>/sources → error contains "profile sources"
func TestLoadConfigWithProfilesWarnsUnavailableSource(t *testing.T) // active profile with a never-fetched source → warn buffer contains "unavailable (never fetched); run code-vm profile update"
```

Follow the existing scratch-config pattern in `root_test.go` / `withScratchConfig`.

- [ ] **Step 2: Run** — `go test ./internal/cli/ -run 'TestLoadConfig' -v` → FAIL.

- [ ] **Step 3: Implement**; update all seven callers.

- [ ] **Step 4: Run** — `mise run test:unit && mise run build` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "feat(cli): warn on unavailable profile sources and guard the sources directory"
```

---

### Task 8: `profile add`, `update`, `list`, `remove`

**Files:**
- Modify: `internal/cli/profile.go`
- Test: `internal/cli/profile_test.go`

**Interfaces:**
- Consumes: `profile.LoadManifest`, `profile.Load`, `profile.SyncSources`, `profile.PruneSources`, `profile.SourcesDir`, `profile.UnavailableWarnings`.
- Produces:
  - `add <git-url> [name]` flags `--into <path>` (string) and `--ref <ref>` (string; only valid with `--into`). Without `--into`: clone → `LoadManifest` → `SyncSources` → `Load`. Manifest/`Load` error → remove clone and `SourcesDir/<name>` → error as today. Sync failures → printed to stderr as `"<failure>; the source stays unavailable until profile update succeeds"`, add still succeeds. With `--into`: write `profiles/<name>/profile.yaml` = `description: <url> → ~/<into>\nsources:\n  - repo: <url>\n    into: <into>\n[    ref: <ref>\n]` (YAML-quote `repo` with `%q`-style double quotes; `into`/`ref` are already charset-restricted), then the same sync-and-`Load` path; validation failure removes the wrapper directory. Trust warning gains the sources sentence from the spec, printed whenever the manifest declares sources.
  - `update [name]`: per profile: pull if `.git` exists (as today, message `"<name>: pulled"`), else `"<name>: not a git clone, sources only"`; `LoadManifest`; `PruneSources`; `SyncSources`; `Load` (hard error if invalid). Collect all `SyncFailure`s; at the end print `"<n> source(s) could not be updated:"` plus one line each, and return a non-nil error so the exit code is 1. Profiles without sources behave exactly as before.
  - `list`: after each profile row, one indented line per source: `"    source %-24s ~/%s  %s  %s"` with name, into, `ref` or `default`, and `Commit[:12]` or `unavailable: <reason>`. Uses `LoadManifest` for rows (so an unavailable source does not make the profile `invalid`) and `Load` for commits.
  - `remove <name>`: also `os.RemoveAll(SourcesDir/<name>)`.

- [ ] **Step 1: Write failing tests** (copy `makeSourceRepo` from `internal/profile/source_test.go` into `profile_test.go` as `makeSourceRepoCLI`, since test helpers do not cross packages):

```go
func TestProfileAddIntoGeneratesWrapperAndSyncs(t *testing.T)
// add <src> --into .claude/skills/s --ref main name=wrap → profiles/wrap/profile.yaml contains "into: .claude/skills/s" and "ref: main";
// <cfgdir>/sources/wrap/s/HEAD exists; output contains the trust warning and "sources are pulled".
func TestProfileAddRefRequiresInto(t *testing.T)           // --ref without --into → error
func TestProfileAddSyncsSourcesOfRealProfile(t *testing.T) // git profile whose manifest declares a source → checkout exists after add
func TestProfileAddUnreachableSourceStillInstalls(t *testing.T)
// manifest repo: /nonexistent/path → add exits 0, profile dir exists, stderr contains "stays unavailable"
func TestProfileUpdateSyncsPrunesAndReportsFailures(t *testing.T)
// wrapper over src; commit c2 upstream; update → Load commit == c2.
// edit wrapper manifest: into: other/t (name changes) → update prunes sources/wrap/s, creates sources/wrap/t.
// edit wrapper repo to /nonexistent → update returns error, output contains "could not be updated", sources/wrap/t still exists.
func TestProfileListShowsSourceLines(t *testing.T)
// synced wrapper → line containing "source" and "~/.claude/skills/s" and 12-hex commit;
// never-synced wrapper → line containing "unavailable: never fetched" and profile state "inactive" (not "invalid").
func TestProfileRemoveDeletesSources(t *testing.T)        // sources/wrap gone after remove
```

- [ ] **Step 2: Run** — `go test ./internal/cli/ -run 'TestProfile(Add|Update|List|Remove)' -v` → FAIL on the new tests; existing ones must still pass or be updated for the new `update` wording (`TestProfileUpdatePullsAndSkipsNonGit` expects "skipped": change it to expect "sources only").

- [ ] **Step 3: Implement** in `profile.go`.

- [ ] **Step 4: Run** — `mise run test:unit && mise run lint && mise run fmt-check && mise run build` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/profile.go internal/cli/profile_test.go
git commit -m "feat(cli): manage profile sources in add, update, list and remove"
```

---

### Task 9: README and integration test

**Files:**
- Modify: `README.md` (Profiles section), `test-vm-sandbox.sh` (Profiles block)

- [ ] **Step 1: README** — add `### Sources` after the bundle format notes and before `### Credentials`: the manifest snippet from the spec (jira-tickets example), the one-liner `code-vm profile add https://github.com/wetransform/jira-tickets --into .claude/skills/jira-ticket --ref <commit>`, ref semantics with "pin a commit for team profiles", the note that the destination directory is wiped on every boot and apply, and that an unreachable repository warns and is skipped. Add `sources:` to the `profile.yaml` key listing. Add `sources/` to the directory layout description of `~/.config/code-vm/`.

- [ ] **Step 2: Integration** — in the Profiles block of `test-vm-sandbox.sh`, after the fixture profile is written and before the first `profile apply`:
  - create a local git repo `$TEST_CONFIG_DIR/fixture-src` with `SKILL.md`, executable `bin/run`, commit;
  - append `sources:\n  - repo: $TEST_CONFIG_DIR/fixture-src\n    into: .claude/skills/fixture\n` to the fixture manifest (`yq -i` since the manifest is edited elsewhere with yq);
  - run `"${CODE_VM_ARGS[@]}" profile update test-profile` and assert exit 0;
  - plant a stray symlink as the agent: `agent ln -sfn /etc "$HOME/.claude/skills/fixture"` (resolve `$HOME` guest-side);
  - after `profile apply`: `assert_ok "source tree lands at its destination" adm test -f /home/$AGENT_USER/.claude/skills/fixture/SKILL.md`; `assert_ok "source executable bit survives" adm test -x /home/$AGENT_USER/.claude/skills/fixture/bin/run`; assert `/etc/passwd` still exists (the symlink was replaced, not followed); owner is the agent uid;
  - remove `bin/run` upstream, commit, `profile update`, `profile apply`, `assert_fails "upstream removal propagates" adm test -e .../bin/run`;
  - `agent touch .../stray`, `profile apply`, `assert_fails "stray files in a source directory are wiped" adm test -e .../stray`;
  - point the manifest `repo` at `$TEST_CONFIG_DIR/does-not-exist`, run `profile update test-profile` and assert it **fails** (exit non-zero) but `profile apply` still **succeeds** and `SKILL.md` is still present (old checkout kept, VM not blocked).

- [ ] **Step 3: Verify** — `mise run lint && mise run fmt-check` → clean; `mise run test:vm` on a machine with KVM → the new assertions pass. If the VM suite cannot run in this session, say so explicitly in the handoff rather than claiming it passed.

- [ ] **Step 4: Commit**

```bash
git add README.md test-vm-sandbox.sh
git commit -m "docs: document profile sources and cover them in the VM suite"
```

---

## Self-review notes

- Spec coverage: manifest/validation (T1), checkout + archive + unavailable (T2), sync semantics and prune (T3), cross-profile collisions (T4), rendering + golden (T5), guest step (T6), warnings + mount guard (T7), CLI flows + trust sentence (T8), docs + integration (T9). Spec's "Left to the builder" items are left open here too.
- Names used consistently: `LoadManifest`, `Load`, `SyncSources`, `PruneSources`, `SourcesDir`, `UnavailableWarnings`, `SyncFailure`, `Source{Name, Into, Repo, Ref, Commit, Archive, Unavailable}`, `PROFILE_SOURCES`, `sources.list`, `sources/<name>.tar.gz.b64`.
- Review Focus items 1–5 each have a named test in T1, T1, T2, T8 and T9.
