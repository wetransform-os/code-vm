# VM Customization Profiles Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Named, team-shareable customization bundles ("profiles") that users activate in the host config: declarative files/packages/shell/domains plus agent-user hook scripts, applied at boot and on demand via `code-vm profile apply`.

**Architecture:** A new `internal/profile` package loads and validates bundles from `~/.config/code-vm/profiles/<name>/` and renders them into `guest.DataFile`s under `/usr/local/share/sandbox-profiles/`. Delivery rides the two existing channels — the Lima template's `mode: data` entries (re-applied on every start) and the `installContent` staging path (for a running VM). One guest script, `apply-profiles.sh`, applies everything idempotently from `manifest.env` + per-profile `files.list`, called by `sandbox-boot.sh` at boot and by `code-vm profile apply`.

**Tech Stack:** Go 1.26.5, Cobra, `gopkg.in/yaml.v3`, bash guest scripts (shellcheck/shfmt-clean), table-driven Go tests + golden file, `test-vm-sandbox.sh` integration suite.

**Spec:** `docs/superpowers/specs/2026-08-19-vm-profiles-design.md`

## Global Constraints

- Profiles are **host-trusted** input; everything reaching a privileged guest context (Squid ACL lines, root apt, env files, guest paths) is validated at load time. Hooks run **only as the agent user**, never root.
- Profiles may never ship `.claude/settings.json` or `.claude/settings.local.json`.
- `manifest.env` is **always** delivered, even with zero active profiles; the applier consumes only what `manifest.env` and `files.list` name (stale `mode: data` leftovers must stay inert).
- List order in `config.yaml` wins: later profiles overwrite earlier files; the last declared shell wins; hooks run in list order.
- No allowlist changes for apt: root egress is direct (firewall `--uid-owner 0 ACCEPT`).
- Verification commands: `mise run test:unit` (`go test ./...`), `mise run lint` (golangci-lint + shellcheck), `mise run fmt-check` (gofmt + shfmt), `mise run build`. All must pass before every commit.
- Commit style: conventional commits (`feat:`, `test:`, `docs:`), ending with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Comment style: comments state constraints and reasons, not narration of the next line. Match the density of neighboring code.
- Golden file regeneration: `UPDATE_GOLDEN=1 go test ./internal/lima/` — always review the diff before committing.

## File Structure

| File | Responsibility |
|---|---|
| `internal/profile/profile.go` (new) | Manifest schema, `Load`/`LoadAll`, all validation |
| `internal/profile/profile_test.go` (new) | Table tests for loading/validation |
| `internal/profile/render.go` (new) | `GuestFiles` (guest tree + `manifest.env` + `files.list`), `AllowDomains` |
| `internal/profile/render_test.go` (new) | Rendering tests |
| `internal/config/config.go` | `Profiles []string` field + name/dup validation |
| `internal/config/paths.go` | `ProfilesDirFor` |
| `internal/lima/template.go` | `RenderParams.AllowDomains` feeding `provision.env` |
| `internal/session/allowlist.go` | `Deps.AllowDomains` replaces `Config.ExtraDomains` use |
| `internal/session/profiles.go` (new) | `PushProfiles`, `ApplyProfiles` |
| `internal/session/stage.go` | `install -D` so nested guest paths work |
| `internal/cli/start.go` | `loadConfigWithProfiles`, thread profiles into render/deps |
| `internal/cli/profile.go` (new) | `profile add/update/list/remove/apply` |
| `internal/cli/root.go` | Register the `profile` command group |
| `internal/guest/files/scripts/apply-profiles.sh` (new) | Guest-side application |
| `internal/guest/files/scripts/provision-system.sh` | Boot-time profile package install |
| `internal/guest/files/scripts/sandbox-boot.sh` | Call the applier in the boot sequence |
| `test-vm-sandbox.sh` | Integration coverage |
| `README.md` | User documentation |

---

### Task 1: Profile manifest loading and validation

**Files:**
- Create: `internal/profile/profile.go`
- Test: `internal/profile/profile_test.go`

**Interfaces:**
- Consumes: `config.ValidateDomain(string) error` (exists).
- Produces:
  - `type Manifest struct { Description string; Packages []string; Shell string; Domains []string; Hook string }` (yaml tags: `description`, `packages`, `shell`, `domains`, `hook`)
  - `type File struct { Rel string; Content []byte; Executable bool }`
  - `type Profile struct { Name, Dir string; Manifest Manifest; Files []File; Hook []byte }`
  - `func Load(profilesDir, name string) (Profile, error)`
  - `func LoadAll(profilesDir string, names []string) ([]Profile, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/profile/profile_test.go`. Tests build fixture profiles in `t.TempDir()`:

```go
package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfile lays out a profile directory under dir. files maps a path
// relative to the profile dir (e.g. "files/.claude/CLAUDE.md") to content.
func writeProfile(t *testing.T, dir, name, manifest string, files map[string]string) {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(root, "profile.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadValidProfile(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "fish-shell", `
description: fish everywhere
packages: [fish]
shell: /usr/bin/fish
domains: [raw.githubusercontent.com]
hook: hook.sh
`, map[string]string{
		"files/.config/fish/config.fish": "set -g fish_greeting\n",
		"files/.claude/CLAUDE.md":        "# rules\n",
		"hook.sh":                        "#!/bin/bash\necho hi\n",
	})

	p, err := Load(dir, "fish-shell")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Manifest.Shell != "/usr/bin/fish" {
		t.Errorf("Shell = %q", p.Manifest.Shell)
	}
	if len(p.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(p.Files))
	}
	// Sorted by Rel, forward-slash relative paths.
	if p.Files[0].Rel != ".claude/CLAUDE.md" || p.Files[1].Rel != ".config/fish/config.fish" {
		t.Errorf("Files sorted wrong: %q, %q", p.Files[0].Rel, p.Files[1].Rel)
	}
	if p.Hook == nil || !strings.Contains(string(p.Hook), "echo hi") {
		t.Error("hook content not loaded")
	}
}

func TestLoadExecutableBit(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{"files/bin/tool": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(dir, "p", "files", "bin", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !p.Files[0].Executable {
		t.Error("executable bit not detected")
	}
}

func TestLoadRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		files    map[string]string
		wantErr  string
	}{
		{"bad name is rejected before disk IO", "", nil, "profile name"},
		{"missing manifest", "", map[string]string{"files/a": "x"}, "read manifest"},
		{"unknown manifest key", "descriptoin: typo\n", nil, "field descriptoin not found"},
		{"bad package name", "packages: ['fish; rm -rf /']\n", nil, "not a Debian package name"},
		{"relative shell", "shell: usr/bin/fish\n", nil, "shell must be an absolute path"},
		{"shell with spaces", "shell: '/usr/bin/fi sh'\n", nil, "shell must be an absolute path"},
		{"bad domain", "domains: ['evil.com whitespace']\n", nil, "not a valid domain"},
		{"hook with path separator", "hook: ../outside.sh\n", nil, "hook must be a plain file name"},
		{"missing hook file", "hook: hook.sh\n", nil, "read hook"},
		{"empty profile", "description: only words\n", nil, "declares nothing"},
		{"settings.json smuggling", "description: x\n",
			map[string]string{"files/.claude/settings.json": "{}"}, "locked Claude settings"},
		{"settings.local.json smuggling", "description: x\n",
			map[string]string{"files/.claude/settings.local.json": "{}"}, "locked Claude settings"},
		{"file path with spaces", "description: x\n",
			map[string]string{"files/has space": "x"}, "file path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			name := "p"
			if tt.name == "bad name is rejected before disk IO" {
				name = "no/slashes"
			} else {
				writeProfile(t, dir, name, tt.manifest, tt.files)
			}
			_, err := Load(dir, name)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsSymlinkInFiles(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{"files/real": "x"})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "p", "files", "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "regular files") {
		t.Errorf("Load error = %v, want symlink rejection", err)
	}
}

func TestLoadAllPreservesOrderAndRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "a", "description: a\npackages: [git]\n", nil)
	writeProfile(t, dir, "b", "description: b\npackages: [jq]\n", nil)

	ps, err := LoadAll(dir, []string{"b", "a"})
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(ps) != 2 || ps[0].Name != "b" || ps[1].Name != "a" {
		t.Errorf("order not preserved: %+v", ps)
	}

	if _, err := LoadAll(dir, []string{"a", "a"}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate names must be rejected, got %v", err)
	}
	if _, err := LoadAll(dir, []string{"missing"}); err == nil {
		t.Error("a missing profile must be an error")
	}
	if ps, err := LoadAll(dir, nil); err != nil || len(ps) != 0 {
		t.Errorf("no names must load cleanly to an empty slice, got %v, %v", ps, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/profile/`
Expected: FAIL — package does not exist / functions undefined.

- [ ] **Step 3: Implement `internal/profile/profile.go`**

```go
// Package profile loads and validates VM customization profiles: named,
// team-shareable bundles activated in the host config. A profile is
// host-trusted input, like config.yaml itself — everything that reaches a
// privileged guest context (Squid ACL lines, a root apt run, env files,
// guest paths) is validated here at load time, so nothing downstream needs
// escaping.
package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wetransform/code-vm/internal/config"
)

// nameRe matches profile names: the same shape as Lima instance names,
// because names appear in guest paths and in manifest.env.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// packageRe matches Debian package names (policy §5.6.1). Package names
// reach a root apt-get invocation via manifest.env.
var packageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

// shellRe matches an absolute path safe to embed in manifest.env and
// /etc/shells: no whitespace, no quotes, no shell metacharacters.
var shellRe = regexp.MustCompile(`^/[a-zA-Z0-9._/-]+$`)

// relPathRe matches the file paths a profile may ship. Deliberately
// conservative: paths are written line-by-line into files.list, which the
// guest applier reads back, so whitespace and metacharacters are rejected
// wholesale. ".." is excluded by the per-segment check in loadFiles.
var relPathRe = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// hookRe matches the manifest's hook entry: a plain file name inside the
// profile directory.
var hookRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// forbiddenFiles are agent-home paths a profile may never ship: the
// security-critical files lock-settings.sh owns and locks.
var forbiddenFiles = map[string]bool{
	".claude/settings.json":       true,
	".claude/settings.local.json": true,
}

// Manifest is the profile.yaml schema. Every key is optional, but a profile
// that declares nothing at all is rejected.
type Manifest struct {
	Description string   `yaml:"description"`
	Packages    []string `yaml:"packages"`
	Shell       string   `yaml:"shell"`
	Domains     []string `yaml:"domains"`
	Hook        string   `yaml:"hook"`
}

// File is one file a profile ships into the agent home.
type File struct {
	Rel        string // cleaned forward-slash path relative to the agent home
	Content    []byte
	Executable bool
}

// Profile is a loaded, validated bundle.
type Profile struct {
	Name     string
	Dir      string
	Manifest Manifest
	Files    []File // sorted by Rel
	Hook     []byte // nil when the manifest declares no hook
}

// Load reads and validates the profile at profilesDir/name.
func Load(profilesDir, name string) (Profile, error) {
	if !nameRe.MatchString(name) {
		return Profile{}, fmt.Errorf("profile name must look like %q, got %q", "fish-shell", name)
	}
	dir := filepath.Join(profilesDir, name)
	data, err := os.ReadFile(filepath.Join(dir, "profile.yaml"))
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: read manifest: %w", name, err)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys are almost always typos, and a typoed key silently doing
	// nothing is worse than an error.
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return Profile{}, fmt.Errorf("profile %s: parse profile.yaml: %w", name, err)
	}
	p := Profile{Name: name, Dir: dir, Manifest: m}
	if err := validateManifest(m); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", name, err)
	}
	if p.Files, err = loadFiles(dir); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", name, err)
	}
	if m.Hook != "" {
		b, err := os.ReadFile(filepath.Join(dir, m.Hook))
		if err != nil {
			return Profile{}, fmt.Errorf("profile %s: read hook: %w", name, err)
		}
		p.Hook = b
	}
	if len(p.Files) == 0 && len(m.Packages) == 0 && m.Shell == "" && len(m.Domains) == 0 && m.Hook == "" {
		return Profile{}, fmt.Errorf("profile %s: declares nothing: no files, packages, shell, domains or hook", name)
	}
	return p, nil
}

// LoadAll loads the named profiles in order. Order is meaningful and
// preserved: later profiles win file collisions, and hooks run in this order.
func LoadAll(profilesDir string, names []string) ([]Profile, error) {
	seen := map[string]bool{}
	out := make([]Profile, 0, len(names))
	for _, n := range names {
		if seen[n] {
			return nil, fmt.Errorf("profile %s: listed twice in the config", n)
		}
		seen[n] = true
		p, err := Load(profilesDir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func validateManifest(m Manifest) error {
	for i, pkg := range m.Packages {
		if !packageRe.MatchString(pkg) {
			return fmt.Errorf("packages[%d]: not a Debian package name: %q", i, pkg)
		}
	}
	if m.Shell != "" && !shellRe.MatchString(m.Shell) {
		return fmt.Errorf("shell must be an absolute path like %q, got %q", "/usr/bin/fish", m.Shell)
	}
	for i, d := range m.Domains {
		if err := config.ValidateDomain(d); err != nil {
			return fmt.Errorf("domains[%d]: %w", i, err)
		}
	}
	if m.Hook != "" && !hookRe.MatchString(m.Hook) {
		return fmt.Errorf("hook must be a plain file name inside the profile, got %q", m.Hook)
	}
	return nil
}

// loadFiles reads the files/ tree. Only regular files are accepted: a symlink
// could escape the tree on the host, or change content between validation and
// delivery.
func loadFiles(dir string) ([]File, error) {
	root := filepath.Join(dir, "files")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("files/%s: only regular files may be shipped (symlinks are rejected)", rel)
		}
		if !relPathRe.MatchString(rel) {
			return fmt.Errorf("file path %q: only [a-zA-Z0-9._/-] is allowed", rel)
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == ".." {
				return fmt.Errorf("file path %q: must stay inside the agent home", rel)
			}
		}
		if forbiddenFiles[rel] {
			return fmt.Errorf("files/%s: profiles may not ship the locked Claude settings", rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, File{Rel: rel, Content: b, Executable: info.Mode()&0o100 != 0})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profile/ -v`
Expected: PASS. If the yaml error message for unknown fields differs from `field descriptoin not found`, adjust the test's `wantErr` to the actual message (verify it is the unknown-field error, not something else).

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/profile/
git commit -m "feat: add profile bundle loading and validation"
```

---

### Task 2: Rendering profiles into guest content

**Files:**
- Create: `internal/profile/render.go`
- Test: `internal/profile/render_test.go`

**Interfaces:**
- Consumes: `Profile`, `Manifest`, `File` (Task 1); `guest.DataFile{Path, Permissions, Content string}` (exists).
- Produces:
  - `const GuestRoot = "/usr/local/share/sandbox-profiles"`
  - `const ManifestPath = GuestRoot + "/manifest.env"`
  - `func GuestFiles(profiles []Profile) []guest.DataFile`
  - `func AllowDomains(extra []string, profiles []Profile) []string`

- [ ] **Step 1: Write the failing tests**

Create `internal/profile/render_test.go`:

```go
package profile

import (
	"reflect"
	"strings"
	"testing"
)

func fixtureProfiles() []Profile {
	return []Profile{
		{
			Name: "fish-shell",
			Manifest: Manifest{
				Packages: []string{"fish", "git"},
				Shell:    "/usr/bin/fish",
				Domains:  []string{"raw.githubusercontent.com"},
				Hook:     "hook.sh",
			},
			Files: []File{
				{Rel: ".config/fish/config.fish", Content: []byte("set -g fish_greeting\n")},
				{Rel: "bin/tool", Content: []byte("#!/bin/sh\n"), Executable: true},
			},
			Hook: []byte("#!/bin/bash\nfisher install x\n"),
		},
		{
			Name:     "wetf-claude",
			Manifest: Manifest{Packages: []string{"git"}},
			Files:    []File{{Rel: ".claude/CLAUDE.md", Content: []byte("# rules\n")}},
		},
	}
}

func TestGuestFilesLayout(t *testing.T) {
	files := GuestFiles(fixtureProfiles())
	byPath := map[string]string{}
	perms := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Content
		perms[f.Path] = f.Permissions
	}

	manifest := byPath[ManifestPath]
	for _, want := range []string{
		`PROFILES="fish-shell wetf-claude"`,
		`PROFILE_PACKAGES="fish git"`, // union, order preserved, deduped
		`PROFILE_SHELL="/usr/bin/fish"`,
		`PROFILE_HOOKS="fish-shell"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest.env missing %q, got:\n%s", want, manifest)
		}
	}

	if byPath[GuestRoot+"/fish-shell/files/.config/fish/config.fish"] == "" {
		t.Error("profile file not rendered at its guest path")
	}
	if perms[GuestRoot+"/fish-shell/files/bin/tool"] != "0555" {
		t.Error("executable files must be delivered 0555")
	}
	if perms[GuestRoot+"/fish-shell/files/.config/fish/config.fish"] != "0444" {
		t.Error("regular files must be delivered 0444")
	}
	if byPath[GuestRoot+"/fish-shell/files.list"] != ".config/fish/config.fish\nbin/tool\n" {
		t.Errorf("files.list = %q", byPath[GuestRoot+"/fish-shell/files.list"])
	}
	if perms[GuestRoot+"/fish-shell/hook"] != "0555" {
		t.Error("hook must be delivered 0555 under the normalized name")
	}
	if _, ok := byPath[GuestRoot+"/wetf-claude/hook"]; ok {
		t.Error("a profile without a hook must not render one")
	}
}

func TestGuestFilesAlwaysIncludesManifest(t *testing.T) {
	files := GuestFiles(nil)
	if len(files) != 1 || files[0].Path != ManifestPath {
		t.Fatalf("zero profiles must still render exactly manifest.env, got %+v", files)
	}
	for _, want := range []string{`PROFILES=""`, `PROFILE_PACKAGES=""`, `PROFILE_SHELL=""`, `PROFILE_HOOKS=""`} {
		if !strings.Contains(files[0].Content, want) {
			t.Errorf("empty manifest.env missing %q", want)
		}
	}
}

func TestAllowDomains(t *testing.T) {
	got := AllowDomains(
		[]string{"registry.example.com", "raw.githubusercontent.com"},
		fixtureProfiles(),
	)
	// extra first, then profile domains, duplicates dropped.
	want := []string{"registry.example.com", "raw.githubusercontent.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllowDomains = %v, want %v", got, want)
	}
	if got := AllowDomains(nil, nil); len(got) != 0 {
		t.Errorf("no inputs must yield no domains, got %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/profile/`
Expected: FAIL — `GuestFiles`, `AllowDomains` undefined.

- [ ] **Step 3: Implement `internal/profile/render.go`**

```go
package profile

import (
	"fmt"
	"path"
	"strings"

	"github.com/wetransform/code-vm/internal/guest"
)

// GuestRoot is where profile content lands in the guest. Delivered root-owned
// outside every mount: the agent may read it, never write it.
const GuestRoot = "/usr/local/share/sandbox-profiles"

// ManifestPath is the env file provision-system.sh and apply-profiles.sh
// source. It is always delivered — even with zero active profiles — so a
// deactivated profile's stale tree on the guest disk is never applied.
const ManifestPath = GuestRoot + "/manifest.env"

// GuestFiles renders the active profiles into the guest files both delivery
// paths install: mode:data entries at start, staged pushes on `profile apply`.
func GuestFiles(profiles []Profile) []guest.DataFile {
	out := []guest.DataFile{manifestEnv(profiles)}
	for _, p := range profiles {
		var list strings.Builder
		for _, f := range p.Files {
			perm := "0444"
			if f.Executable {
				// The applier keys the installed mode off this bit.
				perm = "0555"
			}
			out = append(out, guest.DataFile{
				Path:        path.Join(GuestRoot, p.Name, "files", f.Rel),
				Permissions: perm,
				Content:     string(f.Content),
			})
			list.WriteString(f.Rel + "\n")
		}
		if len(p.Files) > 0 {
			// files.list names what THIS version ships; the applier installs
			// exactly these, so files dropped from the profile stop being
			// applied even though mode:data cannot delete their leftovers.
			out = append(out, guest.DataFile{
				Path:        path.Join(GuestRoot, p.Name, "files.list"),
				Permissions: "0444",
				Content:     list.String(),
			})
		}
		if p.Hook != nil {
			out = append(out, guest.DataFile{
				Path:        path.Join(GuestRoot, p.Name, "hook"),
				Permissions: "0555",
				Content:     string(p.Hook),
			})
		}
	}
	return out
}

// manifestEnv renders the applier's inputs. Every value was validated at load
// time against charsets free of whitespace and quotes, so %q quoting is safe
// for shell sourcing.
func manifestEnv(profiles []Profile) guest.DataFile {
	var names, packages, hooks []string
	seenPkg := map[string]bool{}
	shell := ""
	for _, p := range profiles {
		names = append(names, p.Name)
		for _, pkg := range p.Manifest.Packages {
			if !seenPkg[pkg] {
				seenPkg[pkg] = true
				packages = append(packages, pkg)
			}
		}
		if p.Manifest.Shell != "" {
			shell = p.Manifest.Shell // last profile wins, like file collisions
		}
		if p.Manifest.Hook != "" {
			hooks = append(hooks, p.Name)
		}
	}
	var b strings.Builder
	b.WriteString("# Written by code-vm. Sourced by provision-system.sh and apply-profiles.sh.\n")
	fmt.Fprintf(&b, "PROFILES=%q\n", strings.Join(names, " "))
	fmt.Fprintf(&b, "PROFILE_PACKAGES=%q\n", strings.Join(packages, " "))
	fmt.Fprintf(&b, "PROFILE_SHELL=%q\n", shell)
	fmt.Fprintf(&b, "PROFILE_HOOKS=%q\n", strings.Join(hooks, " "))
	return guest.DataFile{Path: ManifestPath, Permissions: "0444", Content: b.String()}
}

// AllowDomains merges the config's extraDomains with every active profile's
// domains, preserving order and dropping duplicates. This one list feeds both
// provision.env (boot) and the Squid fragment (running VM), so the two paths
// cannot drift.
func AllowDomains(extra []string, profiles []Profile) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ds []string) {
		for _, d := range ds {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	add(extra)
	for _, p := range profiles {
		add(p.Manifest.Domains)
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/profile/ -v`
Expected: PASS.

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/profile/
git commit -m "feat: render profiles into guest files and merged allowlist"
```

---

### Task 3: `profiles` key in the host config

**Files:**
- Modify: `internal/config/config.go` (Config struct, Validate)
- Modify: `internal/config/paths.go`
- Test: `internal/config/config_test.go`, `internal/config/paths_test.go`

**Interfaces:**
- Produces: `Config.Profiles []string` (yaml `profiles,omitempty`); `config.ProfilesDirFor(configPath string) string`; `Config.MountsExcludeTree(dir string) error`.
- Note: `Config.Validate` checks only the name pattern and duplicates. Existence and manifest validity are checked by `profile.LoadAll` at CLI config load (Task 5) — the config package cannot import profile (profile imports config).
- `MountsExcludeTree` exists because `MountsExclude` guards a single file path: a mount of the profiles directory itself — or of one profile inside it — would not cover `config.yaml` and so would slip past it, handing the agent write access to sources that feed the Squid allowlist. Overlap in either direction must be refused.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (follow the existing test style in that file):

```go
func TestValidateProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profiles []string
		wantErr  string
	}{
		{"valid names", []string{"fish-shell", "wetf-claude"}, ""},
		{"empty list", nil, ""},
		{"bad name", []string{"has/slash"}, "profiles[0]"},
		{"empty name", []string{""}, "profiles[0]"},
		{"duplicate", []string{"a", "a"}, "listed twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.Profiles = tt.profiles
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
```

Append to `internal/config/paths_test.go`:

```go
func TestProfilesDirFor(t *testing.T) {
	got := ProfilesDirFor("/home/st/.config/code-vm/config.yaml")
	if got != "/home/st/.config/code-vm/profiles" {
		t.Errorf("ProfilesDirFor = %q", got)
	}
}
```

Append to `internal/config/config_test.go`:

```go
func TestMountsExcludeTree(t *testing.T) {
	dir := "/home/st/.config/code-vm/profiles"
	tests := []struct {
		name    string
		mount   string
		wantErr bool
	}{
		{"unrelated mount", "/home/st/projects", false},
		{"sibling with shared prefix", "/home/st/.config/code-vm-other", false},
		{"mount above the tree", "/home/st/.config", true},
		{"the tree itself", dir, true},
		{"mount inside the tree", dir + "/one-profile", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			c.ProjectsRoot = tt.mount
			err := c.MountsExcludeTree(dir)
			if (err != nil) != tt.wantErr {
				t.Errorf("MountsExcludeTree(%q) with mount %q = %v, wantErr %v", dir, tt.mount, err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/`
Expected: FAIL — `Profiles` field and `ProfilesDirFor` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to the `Config` struct after `ExtraDomains`:

```go
	// Profiles names the customization bundles applied to the guest, in
	// order: later profiles win file collisions and the last declared shell
	// wins. Each name must exist under the profiles directory next to this
	// config file; that is checked when profiles are loaded, not here.
	Profiles []string `yaml:"profiles,omitempty"`
```

Add to `Validate`, after the `ExtraDomains` loop:

```go
	seenProfiles := map[string]bool{}
	for i, p := range c.Profiles {
		if !instanceRe.MatchString(p) {
			return fmt.Errorf("profiles[%d]: must be a name like %q, got %q", i, "fish-shell", p)
		}
		if seenProfiles[p] {
			return fmt.Errorf("profiles[%d]: %q is listed twice", i, p)
		}
		seenProfiles[p] = true
	}
```

In `internal/config/paths.go`:

```go
// ProfilesDirFor returns the profile bundle directory belonging to a config
// file: a "profiles" directory next to it. Deriving it from the config path —
// rather than a fixed location — keeps a --config test setup fully isolated.
func ProfilesDirFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "profiles")
}
```

In `internal/config/config.go`, after `MountsExclude`:

```go
// MountsExcludeTree reports an error if any shared directory overlaps dir in
// either direction: a mount above dir exposes it wholesale, and a mount at or
// below it exposes part of it. MountsExclude cannot cover the second case —
// it guards a single file path — and profiles feed the egress allowlist, so
// an agent-writable profile source would be an allowlist the agent controls.
func (c Config) MountsExcludeTree(dir string) error {
	d := filepath.Clean(dir)
	if m, ok := CoveringMount(c.Mounts(), d); ok {
		return fmt.Errorf(
			"shared directory %s would expose the code-vm profiles (%s) to the agent, "+
				"which could then widen its own egress allowlist; narrow projectsRoot or the extra mount",
			m, d)
	}
	for _, m := range c.Mounts() {
		m = filepath.Clean(m)
		if m == d || strings.HasPrefix(m, d+string(filepath.Separator)) {
			return fmt.Errorf(
				"shared directory %s lies inside the code-vm profiles directory (%s); "+
					"the agent must not be able to edit profile sources", m, d)
		}
	}
	return nil
}
```

(Add the `strings` import to config.go.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/config/
git commit -m "feat: add profiles key to the host config"
```

---

### Task 4: Merged allowlist in the Lima template

**Files:**
- Modify: `internal/lima/template.go` (RenderParams, provisionEnv)
- Test: `internal/lima/template_test.go`

**Interfaces:**
- Produces: `RenderParams.AllowDomains []string` — nil means "fall back to `c.ExtraDomains`" so existing callers/tests keep their behavior until Task 5 threads the real value.

- [ ] **Step 1: Write the failing test**

Append to `internal/lima/template_test.go`:

```go
// provision.env carries the merged allowlist (config extraDomains plus
// profile domains) when the caller resolves one; a nil slice preserves the
// pre-profile behavior of using the config's extraDomains directly.
func TestRenderAllowDomains(t *testing.T) {
	p := testParams(nil)
	p.AllowDomains = []string{"registry.example.com", "raw.githubusercontent.com"}
	out, err := Render(testConfig(), p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `EXTRA_ALLOWED_DOMAINS="registry.example.com raw.githubusercontent.com"`
	if !strings.Contains(out, want) {
		t.Errorf("rendered template missing %q", want)
	}

	out, err = Render(testConfig(), testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `EXTRA_ALLOWED_DOMAINS="registry.example.com"`) {
		t.Error("nil AllowDomains must fall back to the config's extraDomains")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/lima/`
Expected: FAIL — `AllowDomains` field undefined.

- [ ] **Step 3: Implement**

In `RenderParams`, after `VMType`:

```go
	// AllowDomains is the merged egress allowlist: config extraDomains plus
	// active profile domains. nil means just the config's extraDomains, so a
	// caller without profiles renders identically to before profiles existed.
	AllowDomains []string
```

In `provisionEnv`, replace the `EXTRA_ALLOWED_DOMAINS` line:

```go
	domains := p.AllowDomains
	if domains == nil {
		domains = c.ExtraDomains
	}
	fmt.Fprintf(&b, "EXTRA_ALLOWED_DOMAINS=%q\n", strings.Join(domains, " "))
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/lima/ -v`
Expected: PASS (golden test unchanged — the fallback preserves output).

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/lima/
git commit -m "feat: render a merged allowlist into provision.env"
```

---

### Task 5: Thread profiles from config load through render and session deps

**Files:**
- Modify: `internal/session/allowlist.go` (Deps, ApplyAllowlist)
- Modify: `internal/cli/root.go` (`loadConfigWithProfiles`)
- Modify: `internal/cli/start.go` (`agentDeps`, `renderParams`, `renderInstanceFile`, `ensureRunning`, `newStartCmd`)
- Modify: `internal/cli/shell.go:51,54`, `internal/cli/mount.go:74`, `internal/cli/recreate.go:39`, `internal/cli/allow.go:244` (call sites)
- Test: `internal/session/allowlist_test.go`, `internal/lima/template_test.go` (golden), affected `internal/cli/*_test.go`

**Interfaces:**
- Consumes: `profile.LoadAll`, `profile.AllowDomains`, `profile.GuestFiles`, `config.ProfilesDirFor` (Tasks 1–3).
- Produces:
  - `session.Deps.AllowDomains []string` — `ApplyAllowlist` now reads this instead of `d.Config.ExtraDomains`.
  - `func loadConfigWithProfiles() (config.Config, []profile.Profile, string, error)` in cli — used by every command that starts the VM or applies session state. Plain `loadConfig()` keeps its signature for commands that must work even when a listed profile is broken (`stop`, `status`, `profile remove`, ...).
  - `agentDeps(cl lima.Client, c config.Config, profiles []profile.Profile) session.Deps`
  - `renderParams(c config.Config, profiles []profile.Profile) (lima.RenderParams, error)` — appends `profile.GuestFiles(profiles)` to `DataFiles` and sets `AllowDomains`.
  - `ensureRunning(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile) error`

- [ ] **Step 1: Write the failing session test**

`internal/session/allowlist_test.go` already has a `fakeRunner` (records `calls [][]string`, serves `Output` from an `out` map keyed by substring, helper `ranAny(substr string) bool`) and `testDeps(t, r)`. Add:

```go
// AllowDomains, not Config.ExtraDomains, is the source of truth: the cli
// layer merges profile domains into it, and session must not re-derive.
func TestApplyAllowlistUsesAllowDomains(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.Config.ExtraDomains = nil // deliberately empty: only AllowDomains counts
	d.AllowDomains = []string{"registry.example.com", "raw.githubusercontent.com"}
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if !r.ranAny("install -m 0444 -o root -g root") {
		t.Errorf("fragment must be installed for AllowDomains, got %v", r.calls)
	}
	if !r.ranAny("squid -k reconfigure") {
		t.Error("Squid must be reloaded after the allowlist changes")
	}
}
```

Also update the existing tests in that file: everywhere a test sets `d.Config.ExtraDomains = ...` to drive `ApplyAllowlist` (`TestApplyAllowlistInstallsFragmentAndReloadsSquid`, `TestApplyAllowlistSkipsReloadWhenUnchanged`, `TestApplyAllowlistRemovesFragmentWhenDomainsCleared`), set `d.AllowDomains = ...` instead. `TestApplyAllowlistNoDomainsAndNoFragmentIsNoOp` and `TestApplyAllowlistIgnoresDomainFilesInTheWorkingDirectory` need no change (nil stays nil).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/session/`
Expected: FAIL — `AllowDomains` field undefined.

- [ ] **Step 3: Implement the session change**

In `internal/session/allowlist.go`:

Add to `Deps`:

```go
	// AllowDomains is the full egress allowlist for the host-config fragment:
	// config extraDomains merged with active profile domains. The cli layer
	// computes it from trusted inputs only; session setup never derives it.
	AllowDomains []string
```

In `ApplyAllowlist`, replace both uses of `d.Config.ExtraDomains` with `d.AllowDomains`:

```go
	if len(d.AllowDomains) == 0 { ... }
	want := FragmentContent(d.AllowDomains)
```

Update the doc comment on `ApplyAllowlist` ("The host config is the only trusted source..." still holds — extend it to say profile manifests are part of that trusted host input). Update every existing test in `allowlist_test.go` that set `Config.ExtraDomains` to set `AllowDomains` instead.

- [ ] **Step 4: Implement the cli threading**

In `internal/cli/root.go`, extend `loadConfig` — after the existing `MountsExclude(path)` check — so every command refuses a mount overlapping the profiles tree, for the same reason the config check exists:

```go
	// Profiles feed the egress allowlist too, so the same rule applies to
	// their directory — including mounts *inside* it, which the config-file
	// check cannot see.
	if err := c.MountsExcludeTree(config.ProfilesDirFor(path)); err != nil {
		return config.Config{}, path, err
	}
```

Then add, after `loadConfig`:

```go
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
```

In `internal/cli/start.go`:

```go
func agentDeps(cl lima.Client, c config.Config, profiles []profile.Profile) session.Deps {
	return session.Deps{
		Client:       cl,
		Config:       c,
		AgentUser:    agentUser,
		AgentUID:     os.Getuid(),
		AgentGID:     os.Getgid(),
		AllowDomains: profile.AllowDomains(c.ExtraDomains, profiles),
	}
}

func renderParams(c config.Config, profiles []profile.Profile) (lima.RenderParams, error) {
	files, err := guest.DataFiles()
	if err != nil {
		return lima.RenderParams{}, err
	}
	files = append(files, profile.GuestFiles(profiles)...)
	vmType, err := config.ResolveVMType(c.VMType, runtime.GOOS)
	if err != nil {
		return lima.RenderParams{}, err
	}
	return lima.RenderParams{
		AgentUser:    agentUser,
		AgentUID:     os.Getuid(),
		AgentGID:     os.Getgid(),
		VMType:       vmType,
		DataFiles:    files,
		AllowDomains: profile.AllowDomains(c.ExtraDomains, profiles),
	}, nil
}
```

Thread `profiles []profile.Profile` through `renderInstanceFile` and `ensureRunning` the same way. Update every call site (`grep -rn "ensureRunning\|agentDeps" internal/cli`):

- `start.go` `newStartCmd`: use `loadConfigWithProfiles()`, pass profiles.
- `shell.go:51,54`: `runDefault`/shell path — use `loadConfigWithProfiles()`, pass profiles to both `ensureRunning` and `agentDeps`.
- `mount.go:74`: use `loadConfigWithProfiles()`; `ensureRunning(cmd.Context(), cl, updated, profiles)` (mount edits mounts, not profiles — the loaded profiles stay valid).
- `recreate.go:39`: use `loadConfigWithProfiles()`, pass profiles.
- `allow.go:244`: use `loadConfigWithProfiles()` at the top of the command, `agentDeps(cl, c, profiles)`. Note `allow` mutates `c.ExtraDomains` before this call — `agentDeps` recomputes `AllowDomains` from the updated `c`, which is exactly right.

Update the existing cli tests mechanically: in `internal/cli/start_test.go`, every call to `ensureRunning(ctx, client, cfg)`, `renderParams(cfg)` and `renderInstanceFile(cfg)` gains a final `nil` profiles argument. Behavior must not change: with nil profiles, `profile.GuestFiles(nil)` still adds `manifest.env` to the data files — extend `TestRenderInstanceFileIsPrivateAndComplete` with:

```go
	if !strings.Contains(s, "/usr/local/share/sandbox-profiles/manifest.env") {
		t.Error("rendered instance must always deliver manifest.env, even with no profiles")
	}
```

- [ ] **Step 5: Extend the golden template test**

In `internal/lima/template_test.go` (imports `github.com/wetransform/code-vm/internal/profile` — no cycle: profile does not import lima):

```go
func testProfiles() []profile.Profile {
	return []profile.Profile{{
		Name: "fixture",
		Manifest: profile.Manifest{
			Description: "golden fixture",
			Packages:    []string{"fish"},
			Shell:       "/usr/bin/fish",
			Domains:     []string{"raw.githubusercontent.com"},
			Hook:        "hook.sh",
		},
		Files: []profile.File{{Rel: ".claude/CLAUDE.md", Content: []byte("# rules\n")}},
		Hook:  []byte("#!/bin/bash\necho hook\n"),
	}}
}
```

In `TestRenderMatchesGolden`, change the params to:

```go
	profs := testProfiles()
	files := append(profile.GuestFiles(profs), guest.DataFile{
		Path: "/usr/local/lib/sandbox/example.sh", Permissions: "0755", Content: "#!/bin/bash\necho hi\n",
	})
	p := testParams(files)
	p.AllowDomains = profile.AllowDomains(testConfig().ExtraDomains, profs)
	out, err := Render(testConfig(), p)
```

- [ ] **Step 6: Regenerate the golden file and review**

Run: `UPDATE_GOLDEN=1 go test ./internal/lima/ && git diff internal/lima/testdata/golden-template.yaml`
Expected: the diff adds `mode: data` entries for `manifest.env`, the fixture's file, `files.list` and `hook`, and `EXTRA_ALLOWED_DOMAINS` now reads `"registry.example.com raw.githubusercontent.com"`. Nothing else changes.

- [ ] **Step 7: Run all tests**

Run: `mise run test:unit && mise run lint && mise run fmt-check`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/session/ internal/cli/ internal/lima/
git commit -m "feat: thread profiles into template render and session deps"
```

---

### Task 6: Guest scripts — `apply-profiles.sh`, provisioning, boot sequence

**Files:**
- Create: `internal/guest/files/scripts/apply-profiles.sh`
- Modify: `internal/guest/files/scripts/provision-system.sh` (after the Packages section)
- Modify: `internal/guest/files/scripts/sandbox-boot.sh` (after the connectivity check)
- Test: `internal/guest/embed_test.go` (delivery mapping), shellcheck/shfmt

**Interfaces:**
- Consumes: `/etc/sandbox/provision.env` (`AGENT_USER/UID/GID`), `/usr/local/share/sandbox-profiles/manifest.env` (`PROFILES`, `PROFILE_PACKAGES`, `PROFILE_SHELL`, `PROFILE_HOOKS`), per-profile `files/`, `files.list`, `hook` (Task 2 layout).
- Produces: `/usr/local/lib/sandbox/apply-profiles.sh` (0755, auto-delivered by the existing `files/scripts/` mapping in `embed.go` — no embed.go change needed). Env contract: `SANDBOX_PROFILES_STRICT=1` makes hook failures fatal (the `profile apply` path).

- [ ] **Step 1: Write the failing embed test**

Append to `internal/guest/embed_test.go` (match the existing test style there):

```go
func TestApplyProfilesScriptIsDelivered(t *testing.T) {
	files, err := DataFiles()
	if err != nil {
		t.Fatalf("DataFiles: %v", err)
	}
	for _, f := range files {
		if f.Path == "/usr/local/lib/sandbox/apply-profiles.sh" {
			if f.Permissions != "0755" {
				t.Errorf("apply-profiles.sh permissions = %s, want 0755", f.Permissions)
			}
			return
		}
	}
	t.Error("apply-profiles.sh is not delivered to the guest")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/guest/`
Expected: FAIL — script not found.

- [ ] **Step 3: Write `internal/guest/files/scripts/apply-profiles.sh`**

```bash
#!/bin/bash
###############################################################################
# apply-profiles.sh — apply the active customization profiles
#
# Runs as root. Two callers, one implementation:
#   * sandbox-boot.sh at boot, after the firewall is up
#   * `code-vm profile apply` on a running VM (SANDBOX_PROFILES_STRICT=1)
#
# Inputs live under /usr/local/share/sandbox-profiles, delivered root-owned by
# code-vm (mode:data at boot, staged push on apply):
#   manifest.env       ordered profile list, package union, shell, hook list
#   <name>/files/...   file tree destined for the agent home
#   <name>/files.list  the paths this profile version ships — the applier
#                      installs exactly these, so stale tree content left by
#                      an earlier version is inert
#   <name>/hook        hook script, run as the agent user
#
# File and shell failures abort (set -e): they are local and deterministic, so
# failure means a broken profile — at boot the ERR trap in sandbox-boot.sh
# turns that into a failed boot. Hook failures only warn at boot (a flaky
# download must not brick an otherwise safe VM) and are fatal in strict mode.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

PROFILE_ROOT=/usr/local/share/sandbox-profiles
MANIFEST="$PROFILE_ROOT/manifest.env"
STRICT="${SANDBOX_PROFILES_STRICT:-0}"
AGENT_HOME="/home/${AGENT_USER}"

log() { echo "[profiles] $*"; }

if [ ! -f "$MANIFEST" ]; then
    log "No profile manifest; nothing to apply"
    exit 0
fi
# shellcheck source=/dev/null
. "$MANIFEST"

# ── Packages ─────────────────────────────────────────────────────────────────
# Missing-only. At boot provision-system.sh has already installed these; on
# `profile apply` this is what installs newly declared ones. apt runs as root,
# and root egress is direct (the firewall's uid-0 ACCEPT), so no proxy needed.
MISSING=()
for p in ${PROFILE_PACKAGES:-}; do
    dpkg -s "$p" > /dev/null 2>&1 || MISSING+=("$p")
done
if [ ${#MISSING[@]} -gt 0 ]; then
    log "Installing packages: ${MISSING[*]}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq "${MISSING[@]}"
fi

# ── Files ────────────────────────────────────────────────────────────────────
# ensure_parents creates the parent directories for a home-relative path, one
# segment at a time, agent-owned. A symlinked segment aborts: these installs
# run as root, so a symlink the agent planted (~/.config -> /etc) must not be
# able to redirect one outside the home.
ensure_parents() {
    local rel_dir cur="$AGENT_HOME" seg
    rel_dir=$(dirname "$1")
    [ "$rel_dir" = "." ] && return 0
    local IFS=/
    for seg in $rel_dir; do
        [ -n "$seg" ] || continue
        cur="$cur/$seg"
        if [ -L "$cur" ]; then
            echo "[profiles] ERROR: $cur is a symlink; refusing to install through it" >&2
            return 1
        fi
        if [ ! -d "$cur" ]; then
            mkdir -m 0755 "$cur"
            chown "$AGENT_UID:$AGENT_GID" "$cur"
        fi
    done
}

# Profile files are canonical: re-installed on every boot and every apply, so
# local edits to them do not survive (same philosophy as lock-settings.sh).
# Later profiles overwrite earlier ones — list order in the config wins.
for name in ${PROFILES:-}; do
    list="$PROFILE_ROOT/$name/files.list"
    [ -f "$list" ] || continue
    while IFS= read -r rel; do
        [ -n "$rel" ] || continue
        src="$PROFILE_ROOT/$name/files/$rel"
        dst="$AGENT_HOME/$rel"
        ensure_parents "$rel"
        mode=0644
        [ -x "$src" ] && mode=0755
        # Remove first: install would write through a symlink the agent left
        # at the destination. A directory at $dst fails rm -f and aborts loud.
        rm -f "$dst"
        install -m "$mode" -o "$AGENT_UID" -g "$AGENT_GID" "$src" "$dst"
        log "  $name: ~/$rel"
    done < "$list"
done

# ── Shell ────────────────────────────────────────────────────────────────────
# Last declared shell wins (rendered into PROFILE_SHELL by code-vm). Never
# reverted when a profile is deactivated — recreate is the clean-slate path.
if [ -n "${PROFILE_SHELL:-}" ]; then
    if [ ! -x "$PROFILE_SHELL" ]; then
        echo "[profiles] ERROR: shell $PROFILE_SHELL does not exist or is not executable" >&2
        exit 1
    fi
    grep -qxF "$PROFILE_SHELL" /etc/shells || echo "$PROFILE_SHELL" >> /etc/shells
    CURRENT_SHELL=$(getent passwd "$AGENT_USER" | cut -d: -f7)
    if [ "$CURRENT_SHELL" != "$PROFILE_SHELL" ]; then
        chsh -s "$PROFILE_SHELL" "$AGENT_USER"
        log "Login shell: $PROFILE_SHELL"
    fi
fi

# ── Hooks ────────────────────────────────────────────────────────────────────
# Same hardened pattern as update-agent-clis.sh: root-driven but running as
# the agent, so nothing the agent can write may influence it — no login shell,
# system PATH only, BASH_ENV/ENV cleared. Egress goes through the proxy, so a
# hook reaches exactly what its profile's domains (plus the base allowlist)
# permit, and everything it does lands in the proxy log.
run_as_agent() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env -u BASH_ENV -u ENV \
        HOME="$AGENT_HOME" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        http_proxy=http://localhost:3128 \
        https_proxy=http://localhost:3128 \
        no_proxy=localhost,127.0.0.1 \
        bash "$1"
}

HOOK_FAILED=0
for name in ${PROFILE_HOOKS:-}; do
    hook="$PROFILE_ROOT/$name/hook"
    if [ ! -f "$hook" ]; then
        echo "[profiles] WARNING: hook for profile '$name' is missing" >&2
        HOOK_FAILED=1
        continue
    fi
    log "Running hook: $name"
    if ! (cd "$AGENT_HOME" && run_as_agent "$hook"); then
        echo "[profiles] WARNING: hook for profile '$name' failed" >&2
        HOOK_FAILED=1
    fi
done
if [ "$STRICT" = 1 ] && [ "$HOOK_FAILED" = 1 ]; then
    echo "[profiles] ERROR: one or more hooks failed" >&2
    exit 1
fi

log "Profiles applied"
```

- [ ] **Step 4: Modify `provision-system.sh`**

Insert after the existing `# ── Packages ──` block (after the `apt-get install` of `NEEDED`/`MISSING`, before the modprobe lines):

```bash
# ── Profile packages ─────────────────────────────────────────────────────────
# Declared by active profiles; manifest.env is delivered as mode:data before
# provisioning runs. Installed here, pre-firewall, like the base packages.
# apply-profiles.sh repeats a missing-only install for the `profile apply`
# path on a running VM.
PROFILE_MANIFEST=/usr/local/share/sandbox-profiles/manifest.env
if [ -f "$PROFILE_MANIFEST" ]; then
    # shellcheck source=/dev/null
    . "$PROFILE_MANIFEST"
    PROFILE_MISSING=()
    for p in ${PROFILE_PACKAGES:-}; do
        dpkg -s "$p" > /dev/null 2>&1 || PROFILE_MISSING+=("$p")
    done
    if [ ${#PROFILE_MISSING[@]} -gt 0 ]; then
        log "Installing profile packages: ${PROFILE_MISSING[*]}"
        apt-get update -qq
        apt-get install -y -qq "${PROFILE_MISSING[@]}"
    fi
fi
```

- [ ] **Step 5: Modify `sandbox-boot.sh`**

Insert between the connectivity-check `if` block and the `update-agent-clis.sh` line:

```bash
# Profiles after the firewall (hooks egress through the proxy) and before the
# CLI update, so profile files and shell are in place the moment the readiness
# probe releases `code-vm start`. Hook failures warn inside the script; file
# and shell failures abort the boot via the ERR trap above.
/usr/local/lib/sandbox/apply-profiles.sh
```

- [ ] **Step 6: Verify**

Run: `go test ./internal/guest/ && bash -n internal/guest/files/scripts/apply-profiles.sh && mise run lint && mise run fmt-check`
Expected: embed test PASS, shellcheck clean, shfmt clean (run `shfmt -w internal/guest/files/scripts/apply-profiles.sh` if formatting differs — the repo uses 4-space indent per `.editorconfig`).

- [ ] **Step 7: Commit**

```bash
git add internal/guest/
git commit -m "feat: apply profiles in the guest at boot"
```

---

### Task 7: `session.PushProfiles` and `session.ApplyProfiles`

**Files:**
- Create: `internal/session/profiles.go`
- Modify: `internal/session/stage.go` (add `-D` to the final install)
- Test: `internal/session/profiles_test.go`

**Interfaces:**
- Consumes: `installContent` (Task: existing, modified here), `profile.GuestRoot`, `guest.DataFile`.
- Produces:
  - `func PushProfiles(ctx context.Context, d Deps, files []guest.DataFile) error`
  - `func ApplyProfiles(ctx context.Context, d Deps) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/session/profiles_test.go`, reusing `fakeRunner` and `testDeps` from `allowlist_test.go` (same package):

```go
package session

import (
	"context"
	"testing"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

func TestPushProfilesReplacesTheGuestTree(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	files := []guest.DataFile{
		{Path: profile.GuestRoot + "/manifest.env", Permissions: "0444", Content: "PROFILES=\"\"\n"},
		{Path: profile.GuestRoot + "/p/files/.claude/CLAUDE.md", Permissions: "0444", Content: "# rules\n"},
		{Path: profile.GuestRoot + "/p/hook", Permissions: "0555", Content: "#!/bin/bash\n"},
	}
	if err := PushProfiles(context.Background(), d, files); err != nil {
		t.Fatalf("PushProfiles: %v", err)
	}
	if !r.ranAny("rm -rf " + profile.GuestRoot) {
		t.Error("the old tree must be removed so deactivated profiles disappear")
	}
	if !r.ranAny("install -d -m 0755 " + profile.GuestRoot) {
		t.Error("the tree root must be recreated")
	}
	// installContent stages via `limactl copy` then root-installs; -D creates
	// the nested per-profile parents.
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != len(files) {
		t.Errorf("staged %d files, want %d", copies, len(files))
	}
	if !r.ranAny("install -D -m 0444 -o root -g root") {
		t.Errorf("files must be installed root-owned with their permissions, got %v", r.calls)
	}
	if !r.ranAny("install -D -m 0555 -o root -g root") {
		t.Error("the hook must be installed with the executable permission set")
	}
}

func TestApplyProfilesRunsStrict(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := ApplyProfiles(context.Background(), d); err != nil {
		t.Fatalf("ApplyProfiles: %v", err)
	}
	if !r.ranAny("env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh") {
		t.Errorf("the applier must run in strict mode on the apply path, got %v", r.calls)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/session/`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Implement**

In `internal/session/stage.go`, change the final install in `installContent`:

```go
	if err := d.Client.Admin(ctx, []string{
		"install", "-D", "-m", mode, "-o", owner, "-g", group, staged, dst,
	}); err != nil {
```

with a comment on the `-D`: profile files live in nested per-profile directories that do not exist on a fresh push; `-D` creates root-owned parents and is a no-op for the existing flat destinations (fragment dir, home).

Create `internal/session/profiles.go`:

```go
package session

import (
	"context"

	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

// PushProfiles replaces the guest's profile tree with the given rendering.
// The old tree is removed first so a deactivated profile's content actually
// disappears — mode:data delivery on the next boot can only overwrite, which
// is why the applier additionally keys off manifest.env and files.list.
// Content travels through the admin staging path: the agent can read the
// installed result (it is destined for its own home anyway) but never gets a
// window to tamper with what feeds a root-driven apply.
func PushProfiles(ctx context.Context, d Deps, files []guest.DataFile) error {
	if err := d.Client.Admin(ctx, []string{"rm", "-rf", profile.GuestRoot}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-d", "-m", "0755", profile.GuestRoot}); err != nil {
		return err
	}
	for _, f := range files {
		if err := installContent(ctx, d, []byte(f.Content), f.Path, f.Permissions, "root", "root"); err != nil {
			return err
		}
	}
	return nil
}

// ApplyProfiles runs the guest applier in strict mode: on the explicit
// `profile apply` path the user is watching, so a hook failure is an error —
// unlike at boot, where it only warns.
func ApplyProfiles(ctx context.Context, d Deps) error {
	return d.Client.Admin(ctx, []string{
		"env", "SANDBOX_PROFILES_STRICT=1", "/usr/local/lib/sandbox/apply-profiles.sh",
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/session/ -v`
Expected: PASS. The `-D` changes the install argv, so update every existing assertion on it: in `allowlist_test.go`, `ranAny("install -m 0444 -o root -g root")` (including the one added in Task 5) becomes `ranAny("install -D -m 0444 -o root -g root")`; make the same adjustment in `gitidentity_test.go` and `stage_test.go` wherever the install command line is asserted.

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/session/
git commit -m "feat: push and apply profiles into a running VM"
```

---

### Task 8: `code-vm profile add/update/list/remove`

**Files:**
- Create: `internal/cli/profile.go`
- Modify: `internal/cli/root.go` (register `newProfileCmd()`)
- Test: `internal/cli/profile_test.go`

**Interfaces:**
- Consumes: `profile.Load`, `config.ProfilesDirFor`, `loadConfig` (raw — management must work with broken profiles), `configPath` package var.
- Produces: `newProfileCmd() *cobra.Command` with subcommands `add`, `update`, `list`, `remove` (`apply` added in Task 9).

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/profile_test.go`. Use `t.TempDir()` for a scratch config dir; set the package `configPath` var (restore with `t.Cleanup`). For git fixtures, create a local repo:

```go
// makeGitProfile creates a local git repo laid out as a valid profile and
// returns its path, usable as a clone URL.
func makeGitProfile(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "team-fish")
	if err := os.MkdirAll(filepath.Join(src, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "profile.yaml"), []byte("description: fixture\npackages: [fish]\n"), 0o644)
	os.WriteFile(filepath.Join(src, "files", "marker"), []byte("v1\n"), 0o644)
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v1"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return src
}

func withScratchConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() // config dir; profiles live under dir/profiles
	// projectsRoot must be a separate tree: loadConfig refuses any mount
	// overlapping the profiles directory (MountsExcludeTree).
	projects := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("instance: code-sandbox-clitest\nprojectsRoot: "+projects+"\ncpus: 1\nmemory: 1GiB\ndisk: 10GiB\n"), 0o600)
	old := configPath
	configPath = cfg
	t.Cleanup(func() { configPath = old })
	return dir
}
```

Test cases (each running the command via `NewRootCmd()` with `SetArgs` and captured output):

```go
func TestProfileAddClonesAndValidates(t *testing.T)
// add <local-git-path> → profiles/team-fish exists, output contains the trust
// warning ("trusting its author") and the activation hint ("profiles:").

func TestProfileAddRejectsInvalidBundle(t *testing.T)
// point add at a git repo with a broken profile.yaml (bad package name) →
// error, and the cloned directory is removed again.

func TestProfileAddRefusesExisting(t *testing.T)
// second add of the same name → error mentioning `profile update`.

func TestProfileListShowsStatus(t *testing.T)
// one profile on disk, config activates it → list output has the name and
// "active"; a second profile not in the config shows "inactive"; a directory
// with a broken manifest shows "invalid".

func TestProfileRemoveRefusesActive(t *testing.T)
// name in config.Profiles → error; after removing it from the config, remove
// deletes the directory.

func TestProfileUpdatePullsAndSkipsNonGit(t *testing.T)
// clone via add; commit a v2 marker in the source repo; update → marker file
// content is v2. A hand-made (non-git) profile directory is skipped with a
// note, not an error.
```

Write these out fully — each follows the same skeleton:

```go
	dir := withScratchConfig(t)
	src := makeGitProfile(t)
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil { ... }
	// assertions on out.String() and the filesystem under dir/profiles/
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/cli/ -run TestProfile`
Expected: FAIL — unknown command "profile".

- [ ] **Step 3: Implement `internal/cli/profile.go`**

```go
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
			if _, err := profile.Load(dir, name); err != nil {
				// A broken bundle must not linger: it would fail every
				// loadConfigWithProfiles the moment someone activates it.
				os.RemoveAll(dst)
				return fmt.Errorf("cloned profile is invalid and was removed again: %w", err)
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
			names := args
			if len(names) == 0 {
				entries, err := os.ReadDir(dir)
				if err != nil {
					return fmt.Errorf("no profiles directory at %s", dir)
				}
				for _, e := range entries {
					if e.IsDir() {
						names = append(names, e.Name())
					}
				}
			}
			out := cmd.OutOrStdout()
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
```

Register in `internal/cli/root.go`:

```go
	root.AddCommand(newProfileCmd())
```

For this task, stub `newProfileApplyCmd` minimally (real implementation is Task 9):

```go
func newProfileApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Push the active profiles into the running VM and apply them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("not implemented yet")
		},
	}
}
```

Note on `loadConfig` returns: this plan's Task 5 kept `loadConfig() (config.Config, string, error)` — adjust the `c, _, err :=` destructuring here to match the actual signature.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestProfile -v`
Expected: PASS (except any test targeting `apply`, which belongs to Task 9).

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/cli/
git commit -m "feat: add profile add/update/list/remove commands"
```

---

### Task 9: `code-vm profile apply`

**Files:**
- Modify: `internal/cli/profile.go` (replace the stub)
- Test: `internal/cli/profile_test.go`

**Interfaces:**
- Consumes: `loadConfigWithProfiles` (Task 5), `session.PushProfiles`, `session.ApplyProfiles`, `session.ApplyAllowlist`, `profile.GuestFiles`, `agentDeps`, `clientFor`, and the cli test fake for `newClient` (see `start_test.go` for how tests substitute the runner).

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/profile_test.go`. The cli tests fake limactl through the `newClient` package var (see `start_test.go`'s `recordingRunner`, which records `calls [][]string` and returns `statusOut` from `Output`). Add these helpers and tests:

```go
// installFakeClient substitutes the package's limactl client with a recorder.
func installFakeClient(t *testing.T, status string) *recordingRunner {
	t.Helper()
	r := &recordingRunner{statusOut: status}
	old := newClient
	newClient = func() lima.Client { return lima.Client{R: r} }
	t.Cleanup(func() { newClient = old })
	return r
}

func ranAny(calls [][]string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func TestProfileApplyPushesAndRuns(t *testing.T) {
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(pdir, "profile.yaml"), []byte("domains: [example.net]\n"), 0o644)
	os.WriteFile(filepath.Join(pdir, "files", "marker"), []byte("x\n"), 0o644)
	// Activate it: append to the scratch config written by withScratchConfig.
	cfg := configPath
	b, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(b, []byte("profiles:\n  - p\n")...), 0o600)

	r := installFakeClient(t, "Running")
	root := NewRootCmd()
	root.SetArgs([]string{"profile", "apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile apply: %v", err)
	}
	for _, want := range []string{
		"rm -rf /usr/local/share/sandbox-profiles",
		"install -d -m 0755 /usr/local/share/sandbox-profiles",
		"env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh",
	} {
		if !ranAny(r.calls, want) {
			t.Errorf("missing guest command %q in %v", want, r.calls)
		}
	}
	// manifest.env + marker + files.list pushed, plus the allowlist fragment:
	// each travels through one `limactl copy`. Content is staged via host temp
	// files, so the domain itself is asserted at the session layer, not here.
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != 4 {
		t.Errorf("copies = %d, want 4 (manifest.env, marker, files.list, allowlist fragment)", copies)
	}
}

func TestProfileApplyRequiresRunningVM(t *testing.T) {
	withScratchConfig(t)
	installFakeClient(t, "Stopped")
	root := NewRootCmd()
	root.SetArgs([]string{"profile", "apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("apply on a stopped VM = %v, want a not-running error", err)
	}
}
```

Note: `recordingRunner.Output` returns `statusOut` for every `Output` call, including `ReadFile` of the existing fragment — `"Running"` never equals the wanted fragment content, so `ApplyAllowlist` proceeds to install, which is what the copy count expects.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cli/ -run TestProfileApply`
Expected: FAIL — "not implemented yet".

- [ ] **Step 3: Implement**

Replace the stub (and add `github.com/wetransform/code-vm/internal/session` to profile.go's imports — it becomes used now):

```go
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
				return err
			}
			// Allowlist before hooks run: a profile's own domains must be
			// live before its hook needs them.
			if err := session.ApplyAllowlist(ctx, d); err != nil {
				return fmt.Errorf("apply allowlist: %w", err)
			}
			if err := session.ApplyProfiles(ctx, d); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Profiles applied.")
			return nil
		},
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS.

- [ ] **Step 5: Lint, format, commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check`

```bash
git add internal/cli/
git commit -m "feat: add profile apply for running VMs"
```

---

### Task 10: Integration coverage and documentation

**Files:**
- Modify: `test-vm-sandbox.sh` (new section between "Session setup" and "No workspace file is trusted")
- Modify: `README.md`

**Interfaces:**
- Consumes: the full feature. The suite's conventions: `pass`/`fail`/`assert_ok`/`assert_fails`, `adm` (root in guest), `agent` (through the CLI), `yq` for config edits, `$TEST_CONFIG_DIR` (holds the scratch config — profiles land in `$TEST_CONFIG_DIR/profiles/`).

- [ ] **Step 1: Add the integration section**

Insert after the "Session setup" section:

```bash
echo ""
echo "── Profiles ──────────────────────────────────────────────────────"
# Exercises the `profile apply` path end to end: files, packages, shell,
# domains and a hook. The boot path shares the same guest applier and inputs.
# Config is backed up and restored, and the guest tree cleared by a final
# empty apply, so later sections see pre-profile state (except the login
# shell and installed package, which are documented as not reverting).

cp "$CONFIG_FILE" "$CONFIG_FILE.profiles-backup"
PROFILE_FIXTURE="$TEST_CONFIG_DIR/profiles/test-profile"
mkdir -p "$PROFILE_FIXTURE/files/.claude"
cat > "$PROFILE_FIXTURE/profile.yaml" << 'YAML'
description: integration fixture
packages: [fish]
shell: /usr/bin/fish
domains: [example.org]
hook: hook.sh
YAML
echo "# fixture rules" > "$PROFILE_FIXTURE/files/.claude/CLAUDE.md"
cat > "$PROFILE_FIXTURE/hook.sh" << 'SH'
#!/bin/bash
set -eu
echo hook-ran > "$HOME/.profile-hook-ran"
SH
yq -i '.profiles = ["test-profile"]' "$CONFIG_FILE"

if "${CODE_VM_ARGS[@]}" profile apply > /dev/null 2>&1; then
    pass "profile apply succeeds"
else
    fail "profile apply succeeds"
fi

if adm cat "/home/$AGENT_USER/.claude/CLAUDE.md" 2> /dev/null | grep -q "fixture rules"; then
    pass "profile file lands in the agent home"
else
    fail "profile file lands in the agent home"
fi

if [ "$(adm stat -c '%u' "/home/$AGENT_USER/.claude/CLAUDE.md")" = "$(id -u)" ]; then
    pass "profile file is agent-owned"
else
    fail "profile file is agent-owned"
fi

assert_ok "profile package is installed" adm dpkg -s fish

if adm getent passwd "$AGENT_USER" | grep -q ':/usr/bin/fish$'; then
    pass "profile shell is the agent's login shell"
else
    fail "profile shell is the agent's login shell"
fi

assert_ok "profile hook ran as the agent" \
    adm test -f "/home/$AGENT_USER/.profile-hook-ran"

if [ "$(adm stat -c '%u' "/home/$AGENT_USER/.profile-hook-ran")" = "$(id -u)" ]; then
    pass "hook artifacts are agent-owned (hook did not run as root)"
else
    fail "hook artifacts are agent-owned (hook did not run as root)"
fi

assert_ok "profile domain is allowed live" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

# The agent must not be able to edit what feeds a root-driven apply.
assert_fails "agent cannot write the guest profile tree" \
    agent bash -c 'echo x > /usr/local/share/sandbox-profiles/manifest.env'

# Deactivate: the guest tree is cleared and the domain revoked. Installed
# package and shell deliberately survive (documented non-goal).
mv "$CONFIG_FILE.profiles-backup" "$CONFIG_FILE"
"${CODE_VM_ARGS[@]}" profile apply > /dev/null 2>&1
assert_fails "deactivated profile's guest tree is gone" \
    adm test -d /usr/local/share/sandbox-profiles/test-profile
assert_fails "deactivated profile's domain is revoked" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org
adm rm -f "/home/$AGENT_USER/.profile-hook-ran" > /dev/null 2>&1
rm -rf "$TEST_CONFIG_DIR/profiles"
```

- [ ] **Step 2: Verify script hygiene**

Run: `bash -n test-vm-sandbox.sh && mise run lint && mise run fmt-check`
Expected: clean.

- [ ] **Step 3: Run the integration suite** (requires KVM; skip in constrained environments and say so in the commit/PR)

Run: `mise run test:vm`
Expected: all profile assertions PASS, and every pre-existing assertion still passes — in particular "Restart hygiene" (the profile section restores the config and clears guest state before it runs).

- [ ] **Step 4: Document in README.md**

Add a "Profiles" section after the existing configuration documentation (adapt heading level to the README's structure):

```markdown
## Profiles

Profiles are shareable customization bundles for the VM: ship a `CLAUDE.md`,
install tools, set the agent's shell, run setup as the agent user. They live
in `~/.config/code-vm/profiles/<name>/` and are activated in `config.yaml`:

```yaml
profiles:
  - fish-shell
```

Get one from git, or author it locally:

```
code-vm profile add https://github.com/your-org/fish-shell-profile fish-shell
code-vm profile list
code-vm profile update
code-vm profile apply     # push changes into the running VM; boots apply automatically
code-vm profile remove fish-shell
```

A bundle is a directory:

```
fish-shell/
  profile.yaml      # manifest
  files/            # copied into the agent's home (agent-owned, writable)
    .config/fish/config.fish
  hook.sh           # optional; runs as the agent user, through the proxy
```

`profile.yaml`:

```yaml
description: Fish as the agent's shell, with fisher
packages: [fish]            # apt packages, installed as root
shell: /usr/bin/fish        # agent's login shell
domains:                    # merged into the egress allowlist
  - raw.githubusercontent.com
hook: hook.sh               # runs after files and packages, as the agent
```

Notes:

- Installing a profile means trusting its author with the agent's home, apt
  package selection, egress domains, and code execution as the agent user.
  Hooks never run as root, and a profile cannot touch the locked
  `.claude/settings.json`.
- List order matters: later profiles win file collisions; the last declared
  shell wins.
- Profile-shipped files are canonical — re-installed on every boot and every
  apply, so local edits to them do not survive a restart.
- Hooks run on every boot and every apply: write them idempotent.
- Deactivating a profile stops applying it, but does not uninstall packages,
  revert the shell, or delete files already in the home. `code-vm recreate`
  is the clean-slate path.
```

- [ ] **Step 5: Final verification and commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check && mise run build`

```bash
git add test-vm-sandbox.sh README.md
git commit -m "test: cover profiles in the integration suite; document them"
```
