# Profile Secrets and Templates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Profiles ship credentialed config templates (`${secret:name}`/`${var:name}` placeholders); secrets resolve on the host from the user's own `secrets.yaml` (command or literal), vars from `config.yaml`; rendered files push agent-owned 0600 into the guest home at start/apply/boot only.

**Architecture:** Extends `internal/profile` (declarations, `templates/` tree, placeholder scan, rendering, resolution), `internal/config` (`secrets.yaml` loader, `Config.Vars`), and `internal/session` (a new agent-privilege user-file push that also replaces git-identity's root install). A new guest helper script relays staged content to an agent-identity install. CLI wires resolution into `start`, `profile apply`, and boot-causing invocations, plus a `code-vm secrets` listing command.

**Tech Stack:** Go 1.26.5, Cobra, `gopkg.in/yaml.v3`, bash guest script (shellcheck/shfmt-clean), table-driven tests + fake runners, `test-vm-sandbox.sh`.

**Spec:** `docs/superpowers/specs/2026-08-20-profile-secrets-design.md`

## Global Constraints

- Rendered/secret content must NEVER travel via `mode: data` (persists in `~/.lima/<inst>/lima.yaml`), never enter `/usr/local/share/sandbox-profiles` (world-readable), and never be installed into the agent home by root (the TOCTOU class closed in the profiles PR). Delivery: staged push → root relays to a `root:AGENT_GID 0640` tmpfs drop → agent-identity install, final mode 0600 agent-owned, no exec bit.
- `suggest:`/`description:` are inert display strings: never executed, never substituted, never delivered to the guest.
- Host command execution comes ONLY from the user's `secrets.yaml` (`command:`) — never from profiles, the workspace, or the guest.
- Placeholders: exactly `${secret:<name>}` and `${var:<name>}` with names matching `[a-zA-Z0-9][a-zA-Z0-9-]{0,62}`; every other `${...}` passes through byte-for-byte. Undeclared placeholder = load-time error; declared-but-unmapped = start/apply-time error with a ready-to-paste snippet.
- Resolution/render/push happens ONLY at: `code-vm start`, `code-vm profile apply`, and an invocation whose `ensureRunning` actually booted the VM.
- Verification before every commit: `mise run test:unit && mise run lint && mise run fmt-check` (add `mise run build` before the last commit of a task touching cli).
- Commit style: conventional commits ending `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Comments state constraints and reasons, matching the existing density.
- Golden file: profile declarations/templates must NOT change the Lima template rendering (templates are not DataFiles). Any golden drift is a bug.

## File Structure

| File | Responsibility |
|---|---|
| `internal/profile/profile.go` | Manifest gains `Secrets`/`Vars`; `templates/` tree loading; collision + undeclared-placeholder validation |
| `internal/profile/template.go` (new) | Placeholder scan (`FindRefs`), `RenderTemplates`, `DeclaredSecrets`/`DeclaredVars`, `ResolveSecrets`/`ResolveVars` |
| `internal/profile/profile_test.go`, `template_test.go` (new) | Tests |
| `internal/config/secrets.go` (new) | `SecretSource`, `LoadSecrets`, `SecretsPathFor` |
| `internal/config/config.go` | `Config.Vars` + key validation |
| `internal/guest/files/scripts/install-user-file.sh` (new) | Root relay → agent-identity install of one staged file |
| `internal/session/stage.go` | Factor out `stageFile` |
| `internal/session/userfiles.go` (new) | `PushUserFile` |
| `internal/session/gitidentity.go` | Migrate to `PushUserFile` |
| `internal/cli/start.go` | `ensureRunning` returns `(started bool, err)`; `pushRenderedTemplates` helper |
| `internal/cli/{shell,recreate,mount,profile}.go` | Trigger wiring |
| `internal/cli/secrets.go` (new) | `code-vm secrets` listing |
| `internal/cli/profile.go` | `add` trust warning lists declared secrets; `list` marks secret-declaring profiles |
| `test-vm-sandbox.sh`, `README.md` | Integration + docs |

---

### Task 1: Manifest declarations and the templates tree

**Files:**
- Modify: `internal/profile/profile.go`
- Test: `internal/profile/profile_test.go`

**Interfaces:**
- Consumes: existing `Manifest`, `File`, `Profile`, `loadFiles`, `ValidateName`, `isBlank`, `relPathRe`, `forbiddenFiles`.
- Produces:
  - `type SecretSpec struct { Description string \`yaml:"description"\`; Suggest string \`yaml:"suggest"\` }`
  - `type VarSpec struct { Description string \`yaml:"description"\` }`
  - `Manifest.Secrets map[string]SecretSpec \`yaml:"secrets"\``, `Manifest.Vars map[string]VarSpec \`yaml:"vars"\``
  - `Profile.Templates []File` (loaded from `templates/`, sorted by Rel; `Executable` ignored downstream)
  - `refRe` (exported via Task 2's `FindRefs`; defined here or in template.go — put it in template.go, Task 2; THIS task only loads/validates trees and declarations, the placeholder cross-check moves in with Task 2's scanner via a shared `validateTemplateRefs` call added in Task 2)

- [ ] **Step 1: Write the failing tests**

Append to `internal/profile/profile_test.go` (reuse `writeProfile`; it takes a `files` map keyed by profile-relative path, so `templates/.m2/settings.xml` entries work as-is):

```go
func TestLoadTemplatesAndDeclarations(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "maven", `
description: maven setup
secrets:
  repo-user:
    description: Artifactory user
    suggest: gopass show -o wetf/artifactory-user
  repo-password: {}
vars:
  artifactory-url:
    description: Base URL
`, map[string]string{
		"templates/.m2/settings.xml": "<settings>${secret:repo-user}/${var:artifactory-url}</settings>\n",
	})
	p, err := Load(dir, "maven")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Manifest.Secrets["repo-user"].Suggest != "gopass show -o wetf/artifactory-user" {
		t.Errorf("Suggest not loaded: %+v", p.Manifest.Secrets)
	}
	if _, ok := p.Manifest.Secrets["repo-password"]; !ok {
		t.Error("empty-spec secret not loaded")
	}
	if len(p.Templates) != 1 || p.Templates[0].Rel != ".m2/settings.xml" {
		t.Fatalf("Templates = %+v", p.Templates)
	}
}

// A profile carrying only declarations and templates is a valid profile.
func TestLoadTemplatesOnlyProfileIsNotEmpty(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "secrets:\n  tok: {}\n", map[string]string{
		"templates/.npmrc": "//registry/:_authToken=${secret:tok}\n",
	})
	if _, err := Load(dir, "p"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		files    map[string]string
		wantErr  string
	}{
		{"bad secret name", "secrets:\n  'has space': {}\n", nil, "secret name"},
		{"bad var name", "vars:\n  'has/slash': {}\n", nil, "var name"},
		{"template/file collision", "description: x\n",
			map[string]string{"files/.npmrc": "a\n", "templates/.npmrc": "b\n"},
			"both files/ and templates/"},
		{"template ships locked settings", "description: x\n",
			map[string]string{"templates/.claude/settings.json": "{}\n"},
			"locked Claude settings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProfile(t, dir, "p", tt.manifest, tt.files)
			_, err := Load(dir, "p")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsSymlinkedTemplatesRoot(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "description: x\n", map[string]string{"files/a": "x\n"})
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "p", "templates")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(dir, "p"); err == nil || !strings.Contains(err.Error(), "symlinks are rejected") {
		t.Errorf("Load error = %v, want symlink rejection", err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/profile/` fails (unknown manifest fields, no Templates).

- [ ] **Step 3: Implement**

In `internal/profile/profile.go`:

1. Add the `SecretSpec`/`VarSpec` types and the two `Manifest` fields (after `Hook`).
2. Factor the body of `loadFiles` into `loadTree(dir, subdir string) ([]File, error)` — identical logic, with `subdir` replacing the literal `"files"` in the root join and every error message prefix (use `fmt.Sprintf("%s/%s", subdir, rel)` where messages currently say `files/...`; keep the blank-content rejection for both trees — a blank template is a bundle bug, and one rule is easier to hold than two). `loadFiles(dir)` becomes `loadTree(dir, "files")`; add `loadTemplates(dir)` = `loadTree(dir, "templates")`.
3. In `Load`, after `p.Files`: `p.Templates, err = loadTemplates(dir)` with the same error wrapping.
4. In `validateManifest` (change its signature to `validateManifest(m Manifest) error` stays; add):

```go
	for name := range m.Secrets {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("secret name %q: must look like %q", name, "repo-user")
		}
	}
	for name := range m.Vars {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("var name %q: must look like %q", name, "artifactory-url")
		}
	}
```

(Iteration order does not matter — validation only.)
5. After templates load in `Load`, reject collisions within the bundle:

```go
	fileRels := map[string]bool{}
	for _, f := range p.Files {
		fileRels[f.Rel] = true
	}
	for _, tpl := range p.Templates {
		if fileRels[tpl.Rel] {
			return Profile{}, fmt.Errorf("profile %s: %s is shipped by both files/ and templates/; pick one", name, tpl.Rel)
		}
	}
```

6. Extend the empty-profile check: `... && len(p.Templates) == 0 && len(m.Secrets) == 0 && len(m.Vars) == 0` (and add "templates, secrets or vars" to its message).

- [ ] **Step 4: Run to verify pass** — `go test ./internal/profile/ -v`.
- [ ] **Step 5: Verify no golden drift** — `go test ./internal/lima/` must pass UNCHANGED (templates are not DataFiles).
- [ ] **Step 6: Lint, format, commit**

```bash
git add internal/profile/ && git commit -m "feat: profiles declare secrets/vars and ship templates"
```

---

### Task 2: Placeholder scan, rendering, declaration merge, resolution

**Files:**
- Create: `internal/profile/template.go`
- Modify: `internal/profile/profile.go` (wire undeclared-placeholder validation into `Load`)
- Test: `internal/profile/template_test.go`

**Interfaces:**
- Consumes: `Profile`, `File`, `SecretSpec`, `VarSpec` (Task 1); `config.SecretSource` (Task 3 — see note below).
- Produces:
  - `type Ref struct { Kind string // "secret" | "var"; Name string }`
  - `func FindRefs(content []byte) []Ref` — deduplicated, in first-appearance order
  - `type Rendered struct { Rel string; Content []byte }`
  - `func RenderTemplates(profiles []Profile, secrets, vars map[string]string) []Rendered` — later profiles win Rel collisions; output sorted by Rel
  - `type DeclaredSecret struct { Name string; Profiles []string; Description, Suggest string }` (first non-empty Description/Suggest wins); `func DeclaredSecrets(profiles []Profile) []DeclaredSecret` — sorted by name; analogous `DeclaredVar`/`DeclaredVars`
  - `type CommandRunner func(ctx context.Context, command string) ([]byte, error)` — the host-exec seam
  - `func ResolveSecrets(ctx context.Context, declared []DeclaredSecret, sources map[string]config.SecretSource, run CommandRunner) (map[string]string, error)`
  - `func ResolveVars(declared []DeclaredVar, values map[string]string) (map[string]string, error)`
  - `func MissingSecretSnippet(d DeclaredSecret) string` — the ready-to-paste `secrets.yaml` block

**Ordering note:** this task needs `config.SecretSource{Command, Value string}` from Task 3. Tasks 2 and 3 may land in either order; whichever goes first defines the struct (if Task 2 goes first, add the two-field struct to `internal/config/secrets.go` with a doc comment and let Task 3 build the loader around it). Execute Task 3 first to keep it simple.

- [ ] **Step 1: Write the failing tests**

`internal/profile/template_test.go`:

```go
package profile

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestFindRefs(t *testing.T) {
	content := []byte(`user=${secret:repo-user} url=${var:base-url}
again=${secret:repo-user} passthrough=${env.FOO} ${prop} $secret:no ${secret:BAD NAME}`)
	got := FindRefs(content)
	want := []Ref{{Kind: "secret", Name: "repo-user"}, {Kind: "var", Name: "base-url"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindRefs = %v, want %v", got, want)
	}
}

func TestRenderTemplatesSubstitutesAndPassesThrough(t *testing.T) {
	profiles := []Profile{{
		Name: "a",
		Templates: []File{{Rel: ".m2/settings.xml", Content: []byte(
			"<u>${secret:repo-user}</u><url>${var:base-url}</url><keep>${env.HOME}</keep>")}},
	}}
	out := RenderTemplates(profiles,
		map[string]string{"repo-user": "simon"},
		map[string]string{"base-url": "https://x.example"})
	if len(out) != 1 {
		t.Fatalf("Rendered = %+v", out)
	}
	want := "<u>simon</u><url>https://x.example</url><keep>${env.HOME}</keep>"
	if string(out[0].Content) != want {
		t.Errorf("Content = %q, want %q", out[0].Content, want)
	}
}

func TestRenderTemplatesLaterProfileWins(t *testing.T) {
	profiles := []Profile{
		{Name: "a", Templates: []File{{Rel: ".npmrc", Content: []byte("from-a")}}},
		{Name: "b", Templates: []File{{Rel: ".npmrc", Content: []byte("from-b")}}},
	}
	out := RenderTemplates(profiles, nil, nil)
	if len(out) != 1 || string(out[0].Content) != "from-b" {
		t.Errorf("collision must resolve to the later profile, got %+v", out)
	}
}

func TestDeclaredSecretsMergesAcrossProfiles(t *testing.T) {
	profiles := []Profile{
		{Name: "a", Manifest: Manifest{Secrets: map[string]SecretSpec{
			"tok": {Description: "token", Suggest: "gopass show -o t"}}}},
		{Name: "b", Manifest: Manifest{Secrets: map[string]SecretSpec{"tok": {}}}},
	}
	got := DeclaredSecrets(profiles)
	if len(got) != 1 || got[0].Name != "tok" || got[0].Suggest != "gopass show -o t" ||
		!reflect.DeepEqual(got[0].Profiles, []string{"a", "b"}) {
		t.Errorf("DeclaredSecrets = %+v", got)
	}
}

func TestResolveSecrets(t *testing.T) {
	declared := []DeclaredSecret{
		{Name: "from-cmd", Profiles: []string{"p"}},
		{Name: "from-val", Profiles: []string{"p"}},
	}
	sources := map[string]config.SecretSource{
		"from-cmd": {Command: "get-it"},
		"from-val": {Value: "literal"},
	}
	calls := 0
	run := func(_ context.Context, command string) ([]byte, error) {
		calls++
		if command != "get-it" {
			t.Errorf("command = %q", command)
		}
		return []byte("resolved\n"), nil
	}
	got, err := ResolveSecrets(context.Background(), declared, sources, run)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	// Exactly one trailing newline stripped; command runs once per secret.
	if got["from-cmd"] != "resolved" || got["from-val"] != "literal" || calls != 1 {
		t.Errorf("got %v, calls=%d", got, calls)
	}
}

func TestResolveSecretsMissingMappingHasSnippet(t *testing.T) {
	declared := []DeclaredSecret{{
		Name: "repo-user", Profiles: []string{"maven"},
		Description: "Artifactory user", Suggest: "gopass show -o wetf/user",
	}}
	_, err := ResolveSecrets(context.Background(), declared, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an unmapped secret")
	}
	for _, want := range []string{"repo-user", "maven", "Artifactory user",
		"secrets:", "command: gopass show -o wetf/user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}

func TestResolveSecretsCommandFailure(t *testing.T) {
	declared := []DeclaredSecret{{Name: "tok", Profiles: []string{"p"}}}
	sources := map[string]config.SecretSource{"tok": {Command: "boom"}}
	run := func(context.Context, string) ([]byte, error) {
		return []byte("stderr text"), errors.New("exit status 1")
	}
	_, err := ResolveSecrets(context.Background(), declared, sources, run)
	if err == nil || !strings.Contains(err.Error(), "tok") || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("ResolveSecrets error = %v", err)
	}
}

func TestResolveVars(t *testing.T) {
	declared := []DeclaredVar{{Name: "url", Profiles: []string{"p"}, Description: "Base URL"}}
	got, err := ResolveVars(declared, map[string]string{"url": "https://x"})
	if err != nil || got["url"] != "https://x" {
		t.Errorf("ResolveVars = %v, %v", got, err)
	}
	_, err = ResolveVars(declared, nil)
	if err == nil || !strings.Contains(err.Error(), "vars:") || !strings.Contains(err.Error(), "url") {
		t.Errorf("missing var must produce a config.yaml snippet, got %v", err)
	}
}

// Load must reject a template referencing an undeclared name (wired in this task).
func TestLoadRejectsUndeclaredPlaceholder(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "secrets:\n  known: {}\n", map[string]string{
		"templates/.npmrc": "a=${secret:known} b=${var:never-declared}\n",
	})
	_, err := Load(dir, "p")
	if err == nil || !strings.Contains(err.Error(), "never-declared") {
		t.Errorf("Load = %v, want undeclared-placeholder rejection", err)
	}
}

```

(Import list for this test file: `context`, `errors`, `reflect`, `strings`, `testing`, and the config package — no `fmt`.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/profile/`.

- [ ] **Step 3: Implement `internal/profile/template.go`**

```go
package profile

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
)

// refRe matches exactly the two placeholder forms templates may use. The name
// charset mirrors ValidateName, so anything else — Maven properties,
// ${env.FOO} — is left untouched by both the scanner and the renderer.
var refRe = regexp.MustCompile(`\$\{(secret|var):([a-zA-Z0-9][a-zA-Z0-9-]{0,62})\}`)

// Ref is one placeholder occurrence kind+name.
type Ref struct {
	Kind string // "secret" or "var"
	Name string
}

// FindRefs returns the distinct placeholder references in content, in first-
// appearance order.
func FindRefs(content []byte) []Ref {
	seen := map[Ref]bool{}
	var out []Ref
	for _, m := range refRe.FindAllSubmatch(content, -1) {
		r := Ref{Kind: string(m[1]), Name: string(m[2])}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// Rendered is one template after substitution, destined for the agent home.
type Rendered struct {
	Rel     string
	Content []byte
}

// RenderTemplates substitutes secret and var values into every active
// profile's templates. Later profiles win Rel collisions, matching the files/
// rule. Values are opaque bytes: no escaping layer, exactly as the spec
// states — a value that breaks the target format is the user's own.
func RenderTemplates(profiles []Profile, secrets, vars map[string]string) []Rendered {
	byRel := map[string][]byte{}
	for _, p := range profiles {
		for _, tpl := range p.Templates {
			byRel[tpl.Rel] = refRe.ReplaceAllFunc(tpl.Content, func(m []byte) []byte {
				sub := refRe.FindSubmatch(m)
				if string(sub[1]) == "secret" {
					return []byte(secrets[string(sub[2])])
				}
				return []byte(vars[string(sub[2])])
			})
		}
	}
	out := make([]Rendered, 0, len(byRel))
	for rel, content := range byRel {
		out = append(out, Rendered{Rel: rel, Content: content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// DeclaredSecret is one secret name unioned across the active profiles.
type DeclaredSecret struct {
	Name        string
	Profiles    []string // declaring profiles, in activation order
	Description string   // first non-empty wins
	Suggest     string   // first non-empty wins; inert display string
}

// DeclaredVar is the var analog.
type DeclaredVar struct {
	Name        string
	Profiles    []string
	Description string
}

// DeclaredSecrets unions secret declarations across profiles, sorted by name.
func DeclaredSecrets(profiles []Profile) []DeclaredSecret {
	byName := map[string]*DeclaredSecret{}
	for _, p := range profiles {
		names := make([]string, 0, len(p.Manifest.Secrets))
		for n := range p.Manifest.Secrets {
			names = append(names, n)
		}
		sort.Strings(names) // map order is random; keep Profiles deterministic
		for _, n := range names {
			spec := p.Manifest.Secrets[n]
			d, ok := byName[n]
			if !ok {
				d = &DeclaredSecret{Name: n}
				byName[n] = d
			}
			d.Profiles = append(d.Profiles, p.Name)
			if d.Description == "" {
				d.Description = spec.Description
			}
			if d.Suggest == "" {
				d.Suggest = spec.Suggest
			}
		}
	}
	out := make([]DeclaredSecret, 0, len(byName))
	for _, d := range byName {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeclaredVars unions var declarations across profiles, sorted by name.
func DeclaredVars(profiles []Profile) []DeclaredVar {
	byName := map[string]*DeclaredVar{}
	for _, p := range profiles {
		names := make([]string, 0, len(p.Manifest.Vars))
		for n := range p.Manifest.Vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			spec := p.Manifest.Vars[n]
			d, ok := byName[n]
			if !ok {
				d = &DeclaredVar{Name: n}
				byName[n] = d
			}
			d.Profiles = append(d.Profiles, p.Name)
			if d.Description == "" {
				d.Description = spec.Description
			}
		}
	}
	out := make([]DeclaredVar, 0, len(byName))
	for _, d := range byName {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CommandRunner executes a user-authored secrets.yaml command on the host and
// returns its combined output. Injectable for tests.
type CommandRunner func(ctx context.Context, command string) ([]byte, error)

// MissingSecretSnippet renders the ready-to-paste secrets.yaml block for an
// unmapped secret. The suggest hint is copied verbatim as the command —
// display only until the user adopts it by saving this snippet themselves.
func MissingSecretSnippet(d DeclaredSecret) string {
	cmd := d.Suggest
	if cmd == "" {
		cmd = "<command printing the value>"
	}
	return fmt.Sprintf("secrets:\n  %s:\n    command: %s\n", d.Name, cmd)
}

// ResolveSecrets resolves every declared secret from the user's sources. Each
// command runs exactly once per resolve pass with one trailing newline
// stripped (the gopass/pass convention). A missing mapping fails with an
// actionable snippet rather than prompting or falling back to hints: hints
// never execute.
func ResolveSecrets(ctx context.Context, declared []DeclaredSecret, sources map[string]config.SecretSource, run CommandRunner) (map[string]string, error) {
	out := make(map[string]string, len(declared))
	for _, d := range declared {
		src, ok := sources[d.Name]
		if !ok {
			desc := d.Description
			if desc == "" {
				desc = "no description"
			}
			return nil, fmt.Errorf(
				"profile %s needs secret %q (%s), but secrets.yaml does not map it.\nAdd to ~/.config/code-vm/secrets.yaml:\n\n%s",
				strings.Join(d.Profiles, ", "), d.Name, desc, MissingSecretSnippet(d))
		}
		if src.Command != "" {
			b, err := run(ctx, src.Command)
			if err != nil {
				return nil, fmt.Errorf("secret %q: command failed: %w: %s", d.Name, err, strings.TrimSpace(string(b)))
			}
			out[d.Name] = strings.TrimSuffix(string(b), "\n")
			continue
		}
		out[d.Name] = src.Value
	}
	return out, nil
}

// ResolveVars resolves declared vars from config.yaml's literal map.
func ResolveVars(declared []DeclaredVar, values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(declared))
	for _, d := range declared {
		v, ok := values[d.Name]
		if !ok {
			desc := d.Description
			if desc == "" {
				desc = "no description"
			}
			return nil, fmt.Errorf(
				"profile %s needs var %q (%s), but config.yaml does not set it.\nAdd to config.yaml:\n\nvars:\n  %s: <value>\n",
				strings.Join(d.Profiles, ", "), d.Name, desc, d.Name)
		}
		out[d.Name] = v
	}
	return out, nil
}
```

Wire undeclared-placeholder validation into `Load` (profile.go), after templates load and before the empty check:

```go
	for _, tpl := range p.Templates {
		for _, ref := range FindRefs(tpl.Content) {
			declared := false
			if ref.Kind == "secret" {
				_, declared = m.Secrets[ref.Name]
			} else {
				_, declared = m.Vars[ref.Name]
			}
			if !declared {
				return Profile{}, fmt.Errorf(
					"profile %s: templates/%s references ${%s:%s}, which the manifest does not declare",
					name, tpl.Rel, ref.Kind, ref.Name)
			}
		}
	}
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/profile/ -v` (Task 3's `config.SecretSource` must exist first — execute Task 3 before this one if it hasn't landed; see Ordering note).
- [ ] **Step 5: Lint, format, commit**

```bash
git add internal/profile/ && git commit -m "feat: template rendering and secret/var resolution"
```

---

### Task 3: `secrets.yaml` loader and `Config.Vars`

**Files:**
- Create: `internal/config/secrets.go`
- Modify: `internal/config/config.go` (Vars field + validation)
- Test: `internal/config/secrets_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type SecretSource struct { Command string \`yaml:"command"\`; Value string \`yaml:"value"\` }`
  - `func SecretsPathFor(configPath string) string` — `secrets.yaml` next to the config file
  - `func LoadSecrets(path string) (map[string]SecretSource, []string, error)` — `(sources, warnings, err)`; missing file → empty map, no error; unknown keys rejected (KnownFields); per-entry exactly one of command/value; group/world-readable file → warning string
  - `Config.Vars map[string]string \`yaml:"vars,omitempty"\`` — keys validated against the name pattern in `Config.Validate`

- [ ] **Step 1: Write the failing tests**

`internal/config/secrets_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsPathFor(t *testing.T) {
	if got := SecretsPathFor("/home/st/.config/code-vm/config.yaml"); got != "/home/st/.config/code-vm/secrets.yaml" {
		t.Errorf("SecretsPathFor = %q", got)
	}
}

func TestLoadSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets.yaml")
	content := "secrets:\n  a:\n    command: gopass show -o x\n  b:\n    value: literal\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, warnings, err := LoadSecrets(p)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for 0600", warnings)
	}
	if sources["a"].Command != "gopass show -o x" || sources["b"].Value != "literal" {
		t.Errorf("sources = %+v", sources)
	}
}

func TestLoadSecretsMissingFileIsEmpty(t *testing.T) {
	sources, warnings, err := LoadSecrets(filepath.Join(t.TempDir(), "secrets.yaml"))
	if err != nil || len(sources) != 0 || len(warnings) != 0 {
		t.Errorf("missing file must load empty: %v %v %v", sources, warnings, err)
	}
}

func TestLoadSecretsRejectsBadEntries(t *testing.T) {
	tests := []struct{ name, content, wantErr string }{
		{"both command and value", "secrets:\n  a:\n    command: c\n    value: v\n", "exactly one"},
		{"neither", "secrets:\n  a: {}\n", "exactly one"},
		{"bad name", "secrets:\n  'has space':\n    value: v\n", "secret name"},
		{"unknown key", "secrets:\n  a:\n    comand: typo\n", "not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "secrets.yaml")
			if err := os.WriteFile(p, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := LoadSecrets(p)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadSecrets = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadSecretsWarnsOnLoosePermissions(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(p, []byte("secrets:\n  a:\n    value: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := LoadSecrets(p)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "0600") {
		t.Errorf("want a permissions warning recommending 0600, got %v %v", warnings, err)
	}
}
```

Append to `internal/config/config_test.go`:

```go
func TestValidateVars(t *testing.T) {
	c := Default()
	c.ProjectsRoot = "/home/st/projects"
	c.Vars = map[string]string{"artifactory-url": "https://x"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	c.Vars = map[string]string{"has space": "v"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "vars") {
		t.Errorf("Validate = %v, want vars key rejection", err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/config/`.

- [ ] **Step 3: Implement**

`internal/config/secrets.go`:

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SecretSource is one user-authored mapping in secrets.yaml: exactly one of a
// host command (stdout is the value) or a literal. This file is host-trusted
// like config.yaml — it lives in the same mount-guarded tree and only the
// user writes it; profiles can only *suggest* entries, never install them.
type SecretSource struct {
	Command string `yaml:"command"`
	Value   string `yaml:"value"`
}

// secretsFile is the secrets.yaml schema.
type secretsFile struct {
	Secrets map[string]SecretSource `yaml:"secrets"`
}

// SecretsPathFor returns the secrets file belonging to a config file: a
// secrets.yaml next to it, protected by the same mount-exclusion guards.
func SecretsPathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "secrets.yaml")
}

// LoadSecrets reads and validates secrets.yaml. A missing file is an empty
// mapping — profiles without secrets must not require one. Warnings (not
// errors) report loose file permissions: the file holds commands and possibly
// literal credentials.
func LoadSecrets(path string) (map[string]SecretSource, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]SecretSource{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var warnings []string
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s is readable by group/others; recommend chmod 0600", path))
	}
	var f secretsFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty file decodes to io.EOF, and an empty secrets.yaml is as valid
	// as a missing one.
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for name, src := range f.Secrets {
		if !instanceRe.MatchString(name) {
			return nil, nil, fmt.Errorf("%s: secret name %q: must look like %q", path, name, "repo-user")
		}
		if (src.Command == "") == (src.Value == "") {
			return nil, nil, fmt.Errorf("%s: secret %q: exactly one of command or value must be set", path, name)
		}
	}
	if f.Secrets == nil {
		f.Secrets = map[string]SecretSource{}
	}
	return f.Secrets, warnings, nil
}
```

(`bytesReader` = `bytes.NewReader`; import `bytes` directly — the helper name above is illustrative, use `bytes.NewReader(data)`. An empty file decodes to `io.EOF`: handle it like profile.go does, treating EOF as an empty document.)

`internal/config/config.go`: add after `Profiles`:

```go
	// Vars are non-secret literal values available to profile templates as
	// ${var:name}. Secrets never belong here — they go in secrets.yaml.
	Vars map[string]string `yaml:"vars,omitempty"`
```

and in `Validate`, after the profiles loop:

```go
	for name := range c.Vars {
		if !instanceRe.MatchString(name) {
			return fmt.Errorf("vars: key %q must be a name like %q", name, "artifactory-url")
		}
	}
```

- [ ] **Step 4: Run to verify pass**, **Step 5: Lint, format, commit**

```bash
git add internal/config/ && git commit -m "feat: user-side secrets.yaml and config vars"
```

---

### Task 4: Guest relay script and `session.PushUserFile`; migrate git identity

**Files:**
- Create: `internal/guest/files/scripts/install-user-file.sh`
- Create: `internal/session/userfiles.go`
- Modify: `internal/session/stage.go` (factor `stageFile`), `internal/session/gitidentity.go`
- Test: `internal/session/userfiles_test.go`, update `gitidentity_test.go`, `internal/guest/embed_test.go`

**Interfaces:**
- Consumes: staging plumbing in `stage.go`; the hardened setpriv pattern (see `apply-profiles.sh`'s `run_as_agent_sh`).
- Produces:
  - `func stageFile(ctx context.Context, d Deps, content []byte) (string, error)` — the temp-file + `install -d` staging dir + `Copy` half of today's `installContent`, returning the staged guest path
  - `func PushUserFile(ctx context.Context, d Deps, content []byte, rel, mode string) error` — stages content, then `Admin(["/usr/local/lib/sandbox/install-user-file.sh", staged, rel, mode])`
  - Guest contract: `install-user-file.sh <staged-src> <home-relative-dst> <mode>` — root relays the staged file to a `root:AGENT_GID 0640` drop under `/run/sandbox/user-files/`, then an agent-identity install places it (mkdir -p, rm -f, install -m), then the drop and staged copies are removed.

**Why the relay:** the staging dir is limaadmin-0700 (agent cannot read it), and a direct root `install` into the agent home is the TOCTOU class the profiles PR closed. The drop directory gives the agent read access without world-readability, and the final write runs with agent privileges only.

- [ ] **Step 1: Write the failing session tests**

`internal/session/userfiles_test.go` (reuse `fakeRunner`/`testDeps`):

```go
package session

import (
	"context"
	"testing"
)

func TestPushUserFileStagesAndRelays(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := PushUserFile(context.Background(), d, []byte("content"), ".m2/settings.xml", "0600"); err != nil {
		t.Fatalf("PushUserFile: %v", err)
	}
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != 1 {
		t.Errorf("staged copies = %d, want 1", copies)
	}
	if !r.ranAny("/usr/local/lib/sandbox/install-user-file.sh") {
		t.Errorf("relay script not invoked: %v", r.calls)
	}
	if !r.ranAny(".m2/settings.xml") || !r.ranAny("0600") {
		t.Errorf("dst/mode not passed to the relay: %v", r.calls)
	}
	// The old direct-to-home root install must NOT happen for user files.
	if r.ranAny("install -D -m 0600") {
		t.Errorf("user files must not be root-installed into the home: %v", r.calls)
	}
}

func TestGitIdentityUsesUserFilePush(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.Host = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("simon\n"), nil
	}
	if err := ApplyGitIdentity(context.Background(), d); err != nil {
		t.Fatalf("ApplyGitIdentity: %v", err)
	}
	if !r.ranAny("install-user-file.sh") || !r.ranAny(".gitconfig") {
		t.Errorf("git identity must go through the relay: %v", r.calls)
	}
	if r.ranAny("install -D -m 0644") {
		t.Errorf("git identity must no longer be root-installed: %v", r.calls)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/session/`.

- [ ] **Step 3: Implement**

In `stage.go`, split `installContent`: extract everything up to and including the `Copy` into `stageFile(ctx, d, content) (staged string, err)`; `installContent` calls it then keeps its root `install -D` + `rm -f` (still used by the allowlist fragment and profile-tree pushes, whose destinations are root-owned paths — the comment should say that is exactly why root install remains correct there).

`internal/session/userfiles.go`:

```go
package session

import (
	"context"
	"fmt"
)

// PushUserFile delivers content into the agent's home at rel with the given
// mode, without ever writing there as root. The staged copy is relayed by
// install-user-file.sh: root moves it to an agent-group-readable tmpfs drop,
// and an agent-identity install places it — a symlink the agent plants can
// only redirect a write the agent could already make (the same posture as
// profile file installs). Used for rendered templates (0600) and the git
// identity (0644); rel comes from host-validated input only.
func PushUserFile(ctx context.Context, d Deps, content []byte, rel, mode string) error {
	staged, err := stageFile(ctx, d, content)
	if err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{
		"/usr/local/lib/sandbox/install-user-file.sh", staged, rel, mode,
	}); err != nil {
		return fmt.Errorf("install %s: %w", rel, err)
	}
	return nil
}
```

`internal/guest/files/scripts/install-user-file.sh` (auto-delivered 0755 by the existing scripts mapping):

```bash
#!/bin/bash
###############################################################################
# install-user-file.sh — place one host-staged file into the agent home
#
# Invoked as root by code-vm: install-user-file.sh <staged-src> <rel-dst> <mode>
#
# The staged source sits in limaadmin's 0700 staging dir, unreadable by the
# agent; and a root write into the agent-owned home is the TOCTOU class the
# profile applier already closed. So root only RELAYS: the file moves to a
# root:AGENT_GID 0640 drop on tmpfs, and the final install runs with agent
# privileges — a planted symlink can only redirect a write the agent could
# already make. The drop is removed afterwards; rendered secrets exist there
# only for the moment between relay and install.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

src="$1"
rel="$2"
mode="$3"
AGENT_HOME="/home/${AGENT_USER}"

DROP_DIR=/run/sandbox/user-files
install -d -m 0750 -o root -g "$AGENT_GID" "$DROP_DIR"
drop=$(mktemp "$DROP_DIR/file-XXXXXXXX")
install -m 0640 -o root -g "$AGENT_GID" "$src" "$drop"
rm -f "$src"

cleanup() { rm -f "$drop"; }
trap cleanup EXIT

# Same hardened pattern as the profile applier's agent runner: no login
# shell, system PATH only, BASH_ENV/ENV cleared. Positional args, not string
# interpolation.
setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
    env -u BASH_ENV -u ENV \
    HOME="$AGENT_HOME" \
    USER="$AGENT_USER" \
    XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
    PATH=/usr/local/bin:/usr/bin:/bin \
    bash -c 'dst="$1/$2"; mkdir -p "$(dirname "$dst")" && rm -f "$dst" && install -m "$3" "$4" "$dst"' \
    _ "$AGENT_HOME" "$rel" "$mode" "$drop"
```

Migrate `gitidentity.go`: replace the `installContent(...)` call with `PushUserFile(ctx, d, []byte(GitConfigContent(name, email)), ".gitconfig", "0644")` and drop the now-unused numeric-id comment/imports (the relay script owns identity now).

Add an embed test asserting `install-user-file.sh` is delivered 0755 (mirror `TestApplyProfilesScriptIsDelivered`).

- [ ] **Step 4: Run to verify pass** — `go test ./internal/session/ ./internal/guest/` plus `bash -n` on the new script.
- [ ] **Step 5: Golden regen check** — the new script is a DataFile in `guest.DataFiles()` but the golden test passes explicit files, so the golden should be UNCHANGED; if `TestRenderInstanceFileIsPrivateAndComplete`-style assertions or the golden do move, only the new script's mode:data entry may appear.
- [ ] **Step 6: Lint (shellcheck/shfmt cover the script), format, commit**

```bash
git add internal/session/ internal/guest/ && git commit -m "feat: agent-privilege user-file push; migrate git identity to it"
```

---

### Task 5: Trigger plumbing and orchestration

**Files:**
- Modify: `internal/cli/start.go` (`ensureRunning` returns `(bool, error)`; new `pushRenderedTemplates`), `internal/cli/shell.go`, `internal/cli/recreate.go`, `internal/cli/mount.go`, `internal/cli/profile.go` (apply)
- Test: `internal/cli/start_test.go`, `internal/cli/secrets_push_test.go` (new)

**Interfaces:**
- Consumes: Tasks 1–4 (`DeclaredSecrets/Vars`, `ResolveSecrets/Vars`, `RenderTemplates`, `config.LoadSecrets/SecretsPathFor`, `session.PushUserFile`).
- Produces:
  - `ensureRunning(ctx, cl, c, profiles) (started bool, err error)` — `started` is true only when this call actually booted the VM (status was not "Running").
  - `func pushRenderedTemplates(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile, cfgPath string, out io.Writer) error` — fast no-op when no active profile has templates/secrets/vars; otherwise LoadSecrets (print warnings to out), resolve (host runner = `exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()`), render, `PushUserFile(..., rel, "0600")` per rendered file, and print one `Rendered N template(s).` line.
  - Trigger contract: `start` always pushes after `ensureRunning`; `runDefault`, `recreate`, `mount` push only when `started` came back true; `profile apply` pushes after `ApplyAllowlist` and BEFORE `ApplyProfiles` (hooks may consume rendered configs). Note: at boot, hooks run before the post-readiness push — hooks must tolerate absent templates on first boot; the README documents this (Task 7).

- [ ] **Step 1: Write the failing tests**

Update `internal/cli/start_test.go`: `ensureRunning` calls gain the second return; add assertions that status "Running" → `started == false` and statuses ""/"Stopped" → `started == true`.

`internal/cli/secrets_push_test.go` — drive `pushRenderedTemplates` directly with a `recordingRunner`-backed client and a scratch config tree:

```go
func TestPushRenderedTemplatesNoOpWithoutDeclarations(t *testing.T) {
	r := &recordingRunner{statusOut: "Running"}
	c := testCfg(t)
	profiles := []profile.Profile{{Name: "plain", Manifest: profile.Manifest{Packages: []string{"git"}}}}
	if err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, filepath.Join(t.TempDir(), "config.yaml"), io.Discard); err != nil {
		t.Fatalf("pushRenderedTemplates: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("no declarations must mean no guest traffic, got %v", r.calls)
	}
}

func TestPushRenderedTemplatesResolvesAndPushes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("secrets:\n  tok:\n    value: sekrit\n"), 0o600)
	r := &recordingRunner{statusOut: "Running"}
	c := testCfg(t)
	c.Vars = map[string]string{"url": "https://x"}
	profiles := []profile.Profile{{
		Name: "p",
		Manifest: profile.Manifest{
			Secrets: map[string]profile.SecretSpec{"tok": {}},
			Vars:    map[string]profile.VarSpec{"url": {}},
		},
		Templates: []profile.File{{Rel: ".npmrc", Content: []byte("t=${secret:tok};u=${var:url}")}},
	}}
	if err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, cfgPath, io.Discard); err != nil {
		t.Fatalf("pushRenderedTemplates: %v", err)
	}
	if !ranAny(r.calls, "install-user-file.sh") || !ranAny(r.calls, ".npmrc") || !ranAny(r.calls, "0600") {
		t.Errorf("expected a relay push of .npmrc at 0600, got %v", r.calls)
	}
}

func TestPushRenderedTemplatesMissingMappingFails(t *testing.T) {
	r := &recordingRunner{}
	c := testCfg(t)
	profiles := []profile.Profile{{
		Name:      "p",
		Manifest:  profile.Manifest{Secrets: map[string]profile.SecretSpec{"tok": {Suggest: "gopass show -o t"}}},
		Templates: []profile.File{{Rel: ".npmrc", Content: []byte("${secret:tok}")}},
	}}
	err := pushRenderedTemplates(context.Background(), lima.Client{R: r}, c, profiles, filepath.Join(t.TempDir(), "config.yaml"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Errorf("missing mapping must fail with the snippet, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("nothing may reach the guest on resolution failure, got %v", r.calls)
	}
}
```

- [ ] **Step 2: Run to verify failure**, then **Step 3: Implement**

`ensureRunning`: change the three `return ...` exits — "Running" → `(false, nil)`; the two start paths → `(true, cl.Start(...))` / `(true, cl.StartExisting(...))` (return `(false, err)` on pre-start errors). Update every caller:

- `start.go` `newStartCmd`: `if _, err := ensureRunning(...); err != nil { return err }` then ALWAYS `pushRenderedTemplates(...)` (needs the config path — `loadConfigWithProfiles` already returns it).
- `shell.go` `runDefault`: capture `started`; after `session.Setup`, `if started { pushRenderedTemplates(...) }` — before `cl.Agent(...)` so the first command sees the configs.
- `recreate.go`: capture both; push when `started` (recreate always boots, so effectively always).
- `mount.go`: same pattern after its restart.
- `profile.go` apply: insert `pushRenderedTemplates` between `ApplyAllowlist` and `session.ApplyProfiles`.

`pushRenderedTemplates` (in start.go, near `agentDeps`):

```go
// pushRenderedTemplates resolves secrets/vars and pushes rendered templates
// into the agent home. Callers gate it to start, apply, and boot-causing
// invocations only: resolution may invoke the user's secret manager
// (pinentry), so it must never run on every command.
func pushRenderedTemplates(ctx context.Context, cl lima.Client, c config.Config, profiles []profile.Profile, cfgPath string, out io.Writer) error {
	secretsDecl := profile.DeclaredSecrets(profiles)
	varsDecl := profile.DeclaredVars(profiles)
	templated := false
	for _, p := range profiles {
		if len(p.Templates) > 0 {
			templated = true
		}
	}
	if !templated && len(secretsDecl) == 0 && len(varsDecl) == 0 {
		return nil
	}
	sources, warnings, err := config.LoadSecrets(config.SecretsPathFor(cfgPath))
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
	secrets, err := profile.ResolveSecrets(ctx, secretsDecl, sources, hostCommand)
	if err != nil {
		return err
	}
	vars, err := profile.ResolveVars(varsDecl, c.Vars)
	if err != nil {
		return err
	}
	rendered := profile.RenderTemplates(profiles, secrets, vars)
	d := agentDeps(cl, c, profiles)
	for _, r := range rendered {
		if err := session.PushUserFile(ctx, d, r.Content, r.Rel, "0600"); err != nil {
			return err
		}
	}
	if len(rendered) > 0 {
		fmt.Fprintf(out, "Rendered %d template(s) into the sandbox.\n", len(rendered))
	}
	return nil
}

// hostCommand runs a secrets.yaml command through the user's shell on the
// host. CombinedOutput so a failure's stderr reaches the error message.
func hostCommand(ctx context.Context, command string) ([]byte, error) {
	return exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
}
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/cli/ -v` (fix any callers/tests still using the one-value `ensureRunning`).
- [ ] **Step 5: Lint, format, build, commit**

```bash
git add internal/cli/ && git commit -m "feat: resolve and push rendered templates at start, apply, and boot"
```

---

### Task 6: `code-vm secrets` and profile CLI surfacing

**Files:**
- Create: `internal/cli/secrets.go`
- Modify: `internal/cli/profile.go` (`add` warning, `list` marker), `internal/cli/root.go` (register)
- Test: `internal/cli/secrets_test.go`, extend `internal/cli/profile_test.go`

**Interfaces:**
- Consumes: `profile.DeclaredSecrets/Vars`, `MissingSecretSnippet`, `config.LoadSecrets/SecretsPathFor`, `loadConfigWithProfiles`.
- Produces: `newSecretsCmd()` registered on root. Output: one line per declared secret/var — `name  mapped|UNMAPPED  (profiles)  description` — names and status only, never values; unmapped secrets with a hint get the snippet printed after the table. Exit code 0 either way (it is a report, not a gate).

- [ ] **Step 1: Write the failing tests**

`internal/cli/secrets_test.go` (reuse `withScratchConfig` + a scratch profile fixture as in profile_test.go):

```go
func TestSecretsListsMappedAndUnmapped(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	os.MkdirAll(filepath.Join(pdir, "templates"), 0o755)
	os.WriteFile(filepath.Join(pdir, "profile.yaml"), []byte(
		"secrets:\n  mapped-one:\n    description: has a mapping\n  missing-one:\n    suggest: gopass show -o x\nvars:\n  url: {}\n"), 0o644)
	os.WriteFile(filepath.Join(pdir, "templates", ".npmrc"), []byte("${secret:mapped-one}${secret:missing-one}${var:url}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("secrets:\n  mapped-one:\n    value: v\n"), 0o600)
	appendConfig(t, "profiles:\n  - p\n") // helper: append to the scratch config file (add it if profile_test.go lacks one)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"secrets"})
	if err := root.Execute(); err != nil {
		t.Fatalf("secrets: %v", err)
	}
	s := out.String()
	for _, want := range []string{"mapped-one", "missing-one", "UNMAPPED", "url",
		"command: gopass show -o x"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "v\n") && strings.Contains(s, "value") {
		t.Errorf("secret values must never be printed:\n%s", s)
	}
}
```

Extend profile tests: `TestProfileAddWarnsAboutDeclaredSecrets` (add a git fixture whose profile.yaml declares a secret + a matching template; assert the add output contains the secret name) and extend `TestProfileListShowsStatus` (a secret-declaring profile's row contains a `secrets` marker).

- [ ] **Step 2: Run to verify failure**, **Step 3: Implement**

`newSecretsCmd`: `loadConfigWithProfiles()`; collect `DeclaredSecrets`/`DeclaredVars`; `config.LoadSecrets(config.SecretsPathFor(path))`; print the table (`%-24s %-9s %-20s %s` name/status/profiles/description; vars check `c.Vars`); after the table, for each unmapped secret print a blank line, `# add to <secrets path>:` and `profile.MissingSecretSnippet(d)`. Register in `root.go`.

`profile.go` `add`: after `profile.Load`, when the loaded manifest declares secrets, append to the printed output: `This profile declares secrets: <names>. Mapping them in secrets.yaml makes their values readable by the agent (and every active profile's hook).`

`profile.go` `list`: append a ` secrets` marker column entry for profiles with declarations (keep the existing column layout stable: add the marker to the description column's line end, e.g. `desc + "  [secrets: a, b]"` — simplest change that satisfies "marks profiles that declare secrets").

- [ ] **Step 4: Run to verify pass**, **Step 5: Lint, format, build, commit**

```bash
git add internal/cli/ && git commit -m "feat: add code-vm secrets and surface secret declarations in profile add/list"
```

---

### Task 7: Integration coverage and documentation

**Files:**
- Modify: `test-vm-sandbox.sh` (inside the existing "Profiles" section), `README.md`

**Interfaces:**
- Consumes: everything; suite conventions (`pass`/`fail`/`assert_ok`/`assert_fails`, `adm`, `agent`, `yq -i`, `$TEST_CONFIG_DIR`).

- [ ] **Step 1: Extend the suite's Profiles section**

Extend the existing fixture profile (`$PROFILE_FIXTURE`) BEFORE the `profile apply` call: add to its `profile.yaml`:

```yaml
secrets:
  test-token:
    description: integration fixture token
vars:
  test-url: {}
```

plus `mkdir -p "$PROFILE_FIXTURE/templates/.config"` and a template:

```bash
printf 'token=${secret:test-token}\nurl=${var:test-url}\nkeep=${env.HOME}\n' \
    > "$PROFILE_FIXTURE/templates/.config/fixture.conf"
```

Map the inputs in the scratch config tree:

```bash
printf 'secrets:\n  test-token:\n    value: sekrit-value\n' > "$TEST_CONFIG_DIR/secrets.yaml"
chmod 0600 "$TEST_CONFIG_DIR/secrets.yaml"
yq -i '.vars = {"test-url": "https://fixture.example"}' "$CONFIG_FILE"
```

After the existing `profile apply` assertions, add:

```bash
RENDERED="/home/$AGENT_USER/.config/fixture.conf"
if adm cat "$RENDERED" 2> /dev/null | grep -q 'token=sekrit-value'; then
    pass "template secret is substituted in the guest"
else
    fail "template secret is substituted in the guest"
fi
assert_ok "template var is substituted" \
    adm grep -q 'url=https://fixture.example' "$RENDERED"
assert_ok "unrelated placeholders pass through" \
    adm grep -qF 'keep=${env.HOME}' "$RENDERED"
if [ "$(adm stat -c '%u %a' "$RENDERED")" = "$(id -u) 600" ]; then
    pass "rendered template is agent-owned 0600"
else
    fail "rendered template is agent-owned 0600 (got $(adm stat -c '%u %a' "$RENDERED"))"
fi

# Rotation: change the mapped value, re-apply, and the rendered file updates.
printf 'secrets:\n  test-token:\n    value: rotated-value\n' > "$TEST_CONFIG_DIR/secrets.yaml"
"${CODE_VM_ARGS[@]}" profile apply > /dev/null 2>&1
assert_ok "a mapping change plus apply updates the rendered template" \
    adm grep -q 'token=rotated-value' "$RENDERED"

# A declared-but-unmapped secret must fail apply with the snippet, before
# anything reaches the guest. yq, not printf-append: a second top-level
# `secrets:` key would be a YAML duplicate-key parse error, a different
# failure than the one under test.
yq -i '.secrets.extra-unmapped = {"suggest": "gopass show -o nope"}' "$PROFILE_FIXTURE/profile.yaml"
printf 'x=${secret:extra-unmapped}\n' > "$PROFILE_FIXTURE/templates/.config/extra.conf"
UNMAPPED_OUT=$("${CODE_VM_ARGS[@]}" profile apply 2>&1)
if echo "$UNMAPPED_OUT" | grep -q 'gopass show -o nope'; then
    pass "unmapped secret fails apply with the ready-to-paste snippet"
else
    fail "unmapped secret fails apply with the ready-to-paste snippet (got: $UNMAPPED_OUT)"
fi
# Restore the fixture to the mapped-only state for the deactivation steps.
rm -f "$PROFILE_FIXTURE/templates/.config/extra.conf"
yq -i 'del(.secrets.extra-unmapped)' "$PROFILE_FIXTURE/profile.yaml"
"${CODE_VM_ARGS[@]}" profile apply > /dev/null 2>&1

adm rm -f "$RENDERED" > /dev/null 2>&1  # cleanup with the other fixture artifacts
```

(Place the `adm rm -f "$RENDERED"` with the section's existing cleanup block, after the deactivation assertions; the fixture profile dir removal already covers the host side. Deliberate omission: the spec's "survives a restart" integration bullet is not asserted — persistence of a regular file in the ext4 home exercises no mechanism of this feature, and a dedicated restart would add minutes to the suite; the start-time push path is the same code the apply path already covers.)

Run `bash -n test-vm-sandbox.sh` and `mise run lint`.

- [ ] **Step 2: README**

Add a `### Credentials` subsection under Profiles documenting: the trust model (hints never execute; mapping a secret exposes it to the agent and every active profile's hook), `secrets.yaml` with the gopass example, `vars:` in config.yaml, the Maven `settings.xml` walkthrough (placeholders for `<username>`/`<password>`, static proxies/mirrors shipped as-is), when resolution happens (start/apply/boot — never per invocation), the boot-ordering caveat (hooks run before the first push after a cold boot; hooks must tolerate absent templates), and rotation (rotate at source → restart or `profile apply`). Also add `code-vm secrets` to the command list.

- [ ] **Step 3: Full verification**

`mise run test:unit && mise run lint && mise run fmt-check && mise run build`, then `mise run test:vm` (controller may run this; all profile-section assertions plus the pre-existing 111 must pass).

- [ ] **Step 4: Commit**

```bash
git add test-vm-sandbox.sh README.md && git commit -m "test: cover profile secrets/templates in the suite; document credentials"
```
