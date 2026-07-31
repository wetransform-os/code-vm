# VM-based Agent Sandbox Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `code-vm`, a Go CLI that runs Claude Code inside a hardened Lima VM with real rootless Docker, replacing the container sandbox's rootless-Podman DinD mode.

**Architecture:** One long-lived Lima VM (`code-sandbox`) with two guest users: `limaadmin` (UID 60000, passwordless sudo, used only by the CLI for privileged setup) and `devuser` (host UID/GID, no sudo, runs the agent and rootless dockerd). Guest scripts are embedded in the Go binary and delivered into the VM as Lima `mode: data` provision entries on every boot, so CLI and guest side can never skew. Egress filtering stays in-guest: Squid domain allowlist plus iptables default-deny with `--uid-owner devuser` REJECT.

**Tech Stack:** Go + Cobra v1.10.2, Lima v2.2.0, `gopkg.in/yaml.v3`, bash guest scripts, Squid, iptables, rootless Docker, mise for toolchain, bash test suite.

## Global Constraints

- Repository: `/workspace/vm-sandbox` — standalone git history, gitignored from the parent container-sandbox repo. Never commit to the parent repo.
- Spec: `docs/superpowers/specs/2026-07-31-vm-sandbox-design.md`. Read it before Task 1.
- Lima minimum version: `2.2.0`. Set `minimumLimaVersion: 2.2.0` in the template.
- Cobra: `v1.10.2`.
- Go, golangci-lint, shellcheck, shfmt: pin exact latest releases in `mise.toml` at Task 1 (resolve with `mise ls-remote <tool> | tail -1`). Never `latest` as a pinned value.
- VM instance name: `code-sandbox`. Binary name: `code-vm`. Host config: `~/.config/code-vm/config.yaml`.
- Guest users: `limaadmin` UID `60000`; `devuser` UID and primary GID equal to the **host** user's UID/GID.
- `devuser` must never have sudo, and must never be in the `sudo` group.
- Mounts: `mountType: virtiofs`, `writable: true`. Never `reverse-sshfs` (it maps ownership to the SSH user and breaks the UID-matching scheme).
- The Lima template must declare `mounts:` explicitly and must NOT inherit `template:_default/mounts` (that mounts host `$HOME` read-only, exposing `~/.ssh`, `~/.aws`, `~/.config`).
- Squid listens on `3128`. Allowlist fragments live in `/run/sandbox/squid-allow.d/` (tmpfs); `00-base.conf` is always written at boot so the wildcard `include` never matches an empty set.
- Locked config files and rendered credentials: owner `root:devuser`, mode `0444`.
- Guest script install root: `/usr/local/lib/sandbox/`. `sandbox-exec` goes to `/usr/local/bin/sandbox-exec`.
- Provisioning values are passed to guest scripts via `/etc/sandbox/provision.env` (a `mode: data` file), never via undocumented Lima `provision.env` fields.
- Container proxy env injection is opt-in: config key `containerProxy`, default `false`. See Task 6 for why.
- Every CI step invokes `mise run <task>`. Tasks are the canonical entry point for local and CI.
- Commits: Conventional Commits. No `Co-authored-by` footer. No JIRA issue is identifiable for this work, so omit the footer.

---

## File Structure

```
vm-sandbox/
  mise.toml                          # toolchain pins + tasks (canonical entry points)
  go.mod / go.sum
  .gitignore
  README.md
  .github/workflows/ci.yml
  cmd/code-vm/main.go                # thin main: calls cli.Execute()
  internal/config/
    config.go                        # Config struct, Default, Load, Save, Validate, Mounts
    paths.go                         # ExpandPath, CoveringMount, DefaultPath
    config_test.go / paths_test.go
  internal/guest/
    embed.go                         # go:embed of files/, DataFiles(), LimaTemplate()
    embed_test.go
    files/
      lima/code-sandbox.yaml.tpl
      scripts/provision-system.sh
      scripts/provision-user-docker.sh
      scripts/sandbox-boot.sh
      scripts/init-firewall.sh
      scripts/lock-settings.sh
      scripts/render-credentials.sh
      scripts/proxy-log.sh
      scripts/sandbox-exec
      systemd/sandbox-boot.service
      config/.claude/settings.json
      sandbox-templates/*.tpl
  internal/lima/
    limactl.go                       # Runner, ExecRunner, Client + argv builders
    template.go                      # Render(cfg, params) -> Lima YAML
    limactl_test.go / template_test.go
    testdata/golden-template.yaml
  internal/session/
    session.go                       # Setup orchestration
    allowlist.go                      # .sandbox-domains -> Squid fragment
    gitidentity.go
    credentials.go                   # .sandbox-secrets.yaml -> payload + deny rules
    *_test.go
  internal/cli/
    root.go doctor.go shell.go mount.go status.go stop.go recreate.go proxylog.go
    *_test.go
  test-vm-sandbox.sh                 # VM integration suite (needs KVM)
```

Each `internal/cli/*.go` holds exactly one subcommand plus its flag wiring, so adding a command is one new file and one `rootCmd.AddCommand` call.

---

### Task 1: Toolchain, Go module, and host config package

**Files:**
- Create: `mise.toml`, `go.mod`, `.gitignore`, `.github/workflows/ci.yml`
- Create: `internal/config/config.go`, `internal/config/paths.go`
- Test: `internal/config/config_test.go`, `internal/config/paths_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `config.Config` struct with fields `ProjectsRoot string`, `ExtraMounts []string`, `CPUs int`, `Memory string`, `Disk string`, `ExtraDomains []string`, `ContainerProxy bool`
  - `config.Default() Config`
  - `config.Load(path string) (Config, error)` — missing file returns `Default()` with no error
  - `func (c Config) Save(path string) error`
  - `func (c Config) Validate() error`
  - `func (c Config) Mounts() []string` — `ProjectsRoot` first, then `ExtraMounts`, all absolute and cleaned
  - `config.DefaultPath() (string, error)` — `~/.config/code-vm/config.yaml`
  - `config.ExpandPath(p string) (string, error)`
  - `config.CoveringMount(mounts []string, path string) (string, bool)`

- [ ] **Step 1: Pin the toolchain and create mise tasks**

Resolve exact latest versions first:

```bash
cd /workspace/vm-sandbox
mise ls-remote go | tail -1
mise ls-remote golangci-lint | tail -1
mise ls-remote shellcheck | tail -1
mise ls-remote shfmt | tail -1
```

Write `mise.toml`, substituting the resolved versions for `<X>`:

```toml
[tools]
go = "<resolved go version>"
golangci-lint = "<resolved version>"
shellcheck = "<resolved version>"
shfmt = "<resolved version>"

[tasks.build]
description = "Build the code-vm binary"
run = "go build -o dist/code-vm ./cmd/code-vm"

[tasks."test:unit"]
description = "Run Go unit tests"
run = "go test ./..."

[tasks."test:vm"]
description = "Run the VM integration suite (requires KVM)"
run = "bash test-vm-sandbox.sh"

[tasks.lint]
description = "Lint Go and shell sources"
run = [
  "golangci-lint run",
  "shellcheck internal/guest/files/scripts/*.sh internal/guest/files/scripts/sandbox-exec test-vm-sandbox.sh",
]

[tasks."fmt-check"]
description = "Verify formatting"
run = [
  "test -z \"$(gofmt -l .)\"",
  "shfmt -d internal/guest/files/scripts",
]
```

- [ ] **Step 2: Initialise the Go module**

```bash
cd /workspace/vm-sandbox
mise install
mise x -- go mod init github.com/wetransform/code-vm
mise x -- go get github.com/spf13/cobra@v1.10.2
mise x -- go get gopkg.in/yaml.v3
printf 'dist/\n.mise.local.toml\n' > .gitignore
```

- [ ] **Step 3: Write the failing tests**

`internal/config/paths_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty stays empty", "", "", false},
		{"tilde alone", "~", home, false},
		{"tilde slash", "~/projects", filepath.Join(home, "projects"), false},
		{"absolute cleaned", "/home/x/../x/projects/", "/home/x/projects", false},
		{"relative rejected", "projects", "", true},
		{"tilde user rejected", "~other/projects", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ExpandPath(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandPath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCoveringMount(t *testing.T) {
	mounts := []string{"/home/st/projects", "/home/st/work/other"}
	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"exact match", "/home/st/projects", "/home/st/projects", true},
		{"nested subpath", "/home/st/projects/repo/sub", "/home/st/projects", true},
		{"sibling prefix not covered", "/home/st/projects2/repo", "", false},
		{"parent not covered", "/home/st", "", false},
		{"unrelated", "/etc", "", false},
		{"second mount", "/home/st/work/other/x", "/home/st/work/other", true},
		{"trailing slash normalised", "/home/st/projects/repo/", "/home/st/projects", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CoveringMount(mounts, tc.path)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("CoveringMount(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCoveringMountPrefersLongestMatch(t *testing.T) {
	mounts := []string{"/home/st", "/home/st/projects"}
	got, ok := CoveringMount(mounts, "/home/st/projects/repo")
	if !ok || got != "/home/st/projects" {
		t.Errorf("got (%q, %v), want (\"/home/st/projects\", true)", got, ok)
	}
}
```

`internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultValues(t *testing.T) {
	d := Default()
	if d.CPUs != 4 || d.Memory != "12GiB" || d.Disk != "100GiB" {
		t.Errorf("unexpected defaults: %+v", d)
	}
	if d.ContainerProxy {
		t.Error("containerProxy must default to false")
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CPUs != Default().CPUs {
		t.Errorf("CPUs = %d, want %d", got.CPUs, Default().CPUs)
	}
}

func TestLoadOverridesAndExpands(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "projectsRoot: ~/code\nextraMounts:\n  - ~/work/thing\ncpus: 8\nextraDomains:\n  - registry.example.com\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProjectsRoot != filepath.Join(home, "code") {
		t.Errorf("ProjectsRoot = %q", got.ProjectsRoot)
	}
	if got.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8", got.CPUs)
	}
	if got.Memory != Default().Memory {
		t.Errorf("Memory = %q, want default %q", got.Memory, Default().Memory)
	}
	mounts := got.Mounts()
	if len(mounts) != 2 || mounts[0] != filepath.Join(home, "code") || mounts[1] != filepath.Join(home, "work/thing") {
		t.Errorf("Mounts() = %v", mounts)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults expanded are valid", func(c *Config) { c.ProjectsRoot = "/home/st/projects" }, false},
		{"empty projectsRoot", func(c *Config) { c.ProjectsRoot = "" }, true},
		{"relative projectsRoot", func(c *Config) { c.ProjectsRoot = "projects" }, true},
		{"zero cpus", func(c *Config) { c.ProjectsRoot = "/p"; c.CPUs = 0 }, true},
		{"bad memory", func(c *Config) { c.ProjectsRoot = "/p"; c.Memory = "lots" }, true},
		{"bad disk", func(c *Config) { c.ProjectsRoot = "/p"; c.Disk = "12" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	c := Default()
	c.ProjectsRoot = "/home/st/projects"
	c.ExtraMounts = []string{"/home/st/other"}
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ProjectsRoot != c.ProjectsRoot || len(got.ExtraMounts) != 1 || got.ExtraMounts[0] != "/home/st/other" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `internal/config` has no non-test Go files, so the package does not compile (`undefined: ExpandPath`, `undefined: Default`, …).

- [ ] **Step 5: Implement `internal/config/paths.go`**

```go
// Package config loads and validates the code-vm host configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath returns the standard host config location.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "code-vm", "config.yaml"), nil
}

// ExpandPath resolves a leading "~/" and returns a cleaned absolute path.
// An empty input returns an empty result. "~user" forms are rejected: they
// would resolve to a different user's home and have no meaning here.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute or start with %q: %q", "~/", p)
	}
	return filepath.Clean(p), nil
}

// CoveringMount returns the longest mount that contains path, if any.
// A mount covers path when it equals path or is a parent directory of it.
// A shared string prefix is not enough: /home/st/projects does not cover
// /home/st/projects2.
func CoveringMount(mounts []string, path string) (string, bool) {
	p := filepath.Clean(path)
	best := ""
	for _, m := range mounts {
		m = filepath.Clean(m)
		if p == m || strings.HasPrefix(p, m+string(filepath.Separator)) {
			if len(m) > len(best) {
				best = m
			}
		}
	}
	return best, best != ""
}
```

- [ ] **Step 6: Implement `internal/config/config.go`**

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// sizeRe matches Lima-style size strings such as "12GiB" or "512MiB".
var sizeRe = regexp.MustCompile(`^[0-9]+(B|KiB|MiB|GiB|TiB)$`)

// Config is the code-vm host configuration. It is the entire knob surface:
// the Lima instance is rendered from it, so the VM shape stays reproducible.
type Config struct {
	ProjectsRoot   string   `yaml:"projectsRoot"`
	ExtraMounts    []string `yaml:"extraMounts,omitempty"`
	CPUs           int      `yaml:"cpus"`
	Memory         string   `yaml:"memory"`
	Disk           string   `yaml:"disk"`
	ExtraDomains   []string `yaml:"extraDomains,omitempty"`
	ContainerProxy bool     `yaml:"containerProxy"`
}

// Default returns the built-in configuration. Disk is large because Docker
// image layers accumulate inside the guest.
func Default() Config {
	return Config{
		ProjectsRoot:   "~/projects",
		CPUs:           4,
		Memory:         "12GiB",
		Disk:           "100GiB",
		ContainerProxy: false,
	}
}

// Load reads the config at path, layered over Default. A missing file is not
// an error: the defaults are usable on their own. Paths are expanded but not
// validated; call Validate for that.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fall through with defaults
	case err != nil:
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	default:
		if err := yaml.Unmarshal(data, &c); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if c.ProjectsRoot, err = ExpandPath(c.ProjectsRoot); err != nil {
		return Config{}, fmt.Errorf("projectsRoot: %w", err)
	}
	for i, m := range c.ExtraMounts {
		if c.ExtraMounts[i], err = ExpandPath(m); err != nil {
			return Config{}, fmt.Errorf("extraMounts[%d]: %w", i, err)
		}
	}
	return c, nil
}

// Save writes the config as YAML, creating parent directories as needed.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Validate checks the config is usable for rendering a Lima instance.
func (c Config) Validate() error {
	if c.ProjectsRoot == "" {
		return errors.New("projectsRoot must be set")
	}
	if !filepath.IsAbs(c.ProjectsRoot) {
		return fmt.Errorf("projectsRoot must be an absolute path, got %q", c.ProjectsRoot)
	}
	for i, m := range c.ExtraMounts {
		if !filepath.IsAbs(m) {
			return fmt.Errorf("extraMounts[%d] must be an absolute path, got %q", i, m)
		}
	}
	if c.CPUs < 1 {
		return fmt.Errorf("cpus must be at least 1, got %d", c.CPUs)
	}
	if !sizeRe.MatchString(c.Memory) {
		return fmt.Errorf("memory must look like \"12GiB\", got %q", c.Memory)
	}
	if !sizeRe.MatchString(c.Disk) {
		return fmt.Errorf("disk must look like \"100GiB\", got %q", c.Disk)
	}
	return nil
}

// Mounts returns every host directory shared into the guest, projects root
// first, cleaned and de-duplicated while preserving order.
func (c Config) Mounts() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range append([]string{c.ProjectsRoot}, c.ExtraMounts...) {
		if m == "" {
			continue
		}
		m = filepath.Clean(m)
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `mise run test:unit`
Expected: PASS — all tests in `internal/config`.

Then check formatting and lint:

Run: `mise run fmt-check && mise run lint`
Expected: both succeed. `golangci-lint` may report no issues; the shellcheck invocation will find no script files yet, which is fine — if shellcheck errors on missing files, leave the lint task as written and note that Task 4 creates them.

- [ ] **Step 8: Add the CI workflow**

`.github/workflows/ci.yml`:

```yaml
name: ci

on:
  push:
  pull_request:

jobs:
  checks:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: jdx/mise-action@v3
      - run: mise run fmt-check
      - run: mise run lint
      - run: mise run test:unit
      - run: mise run build
```

The VM suite is deliberately absent: it needs nested KVM, which GitHub-hosted runners do not reliably provide. It runs via `mise run test:vm`.

- [ ] **Step 9: Commit**

```bash
cd /workspace/vm-sandbox
git add mise.toml go.mod go.sum .gitignore .github internal/config
git commit -m "feat: add host config package and mise toolchain

Config is the whole knob surface for the VM: the Lima instance is
rendered from it. CoveringMount deliberately rejects shared string
prefixes so /home/st/projects does not cover /home/st/projects2."
```

---

### Task 2: Embedded guest assets and Lima template rendering

**Files:**
- Create: `internal/guest/embed.go`, `internal/guest/files/lima/code-sandbox.yaml.tpl`
- Create: `internal/lima/template.go`
- Test: `internal/guest/embed_test.go`, `internal/lima/template_test.go`, `internal/lima/testdata/golden-template.yaml`

**Interfaces:**
- Consumes: `config.Config` (fields, `Mounts()`) from Task 1.
- Produces:
  - `guest.DataFile` struct: `Path string`, `Permissions string`, `Content string`
  - `guest.DataFiles() ([]guest.DataFile, error)` — every file under `files/scripts/`, `files/systemd/`, `files/config/`, `files/sandbox-templates/`, mapped to its guest install path, sorted by `Path`
  - `guest.LimaTemplate() (string, error)`
  - `lima.RenderParams` struct: `AgentUser string`, `AgentUID int`, `AgentGID int`, `DataFiles []guest.DataFile`
  - `lima.Render(c config.Config, p lima.RenderParams) (string, error)`

Guest install path mapping used by `DataFiles()`:

| Embedded path | Guest path | Permissions |
|---|---|---|
| `files/scripts/sandbox-exec` | `/usr/local/bin/sandbox-exec` | `0755` |
| `files/scripts/<name>.sh` | `/usr/local/lib/sandbox/<name>.sh` | `0755` |
| `files/systemd/<name>` | `/etc/systemd/system/<name>` | `0644` |
| `files/config/<rel>` | `/usr/local/share/sandbox-config/<rel>` | `0444` |
| `files/sandbox-templates/<name>` | `/usr/local/share/sandbox-templates/<name>` | `0444` |

- [ ] **Step 1: Write the failing tests**

`internal/guest/embed_test.go`:

```go
package guest

import (
	"sort"
	"testing"
)

func TestDataFilesMapsGuestPaths(t *testing.T) {
	files, err := DataFiles()
	if err != nil {
		t.Fatalf("DataFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("DataFiles returned nothing")
	}
	byPath := map[string]DataFile{}
	paths := []string{}
	for _, f := range files {
		byPath[f.Path] = f
		paths = append(paths, f.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Errorf("DataFiles must be sorted by Path, got %v", paths)
	}
	for _, want := range []struct {
		path  string
		perms string
	}{
		{"/usr/local/bin/sandbox-exec", "0755"},
		{"/usr/local/lib/sandbox/provision-system.sh", "0755"},
		{"/etc/systemd/system/sandbox-boot.service", "0644"},
		{"/usr/local/share/sandbox-config/.claude/settings.json", "0444"},
	} {
		f, ok := byPath[want.path]
		if !ok {
			t.Errorf("missing guest path %s", want.path)
			continue
		}
		if f.Permissions != want.perms {
			t.Errorf("%s permissions = %s, want %s", want.path, f.Permissions, want.perms)
		}
		if f.Content == "" {
			t.Errorf("%s has empty content", want.path)
		}
	}
}

func TestLimaTemplateIsPresent(t *testing.T) {
	tpl, err := LimaTemplate()
	if err != nil {
		t.Fatalf("LimaTemplate: %v", err)
	}
	if tpl == "" {
		t.Fatal("LimaTemplate is empty")
	}
}
```

`internal/lima/template_test.go`:

```go
package lima

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
)

func testConfig() config.Config {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	c.ExtraMounts = []string{"/home/st/work/other"}
	c.ExtraDomains = []string{"registry.example.com"}
	return c
}

func testParams(files []guest.DataFile) RenderParams {
	return RenderParams{AgentUser: "devuser", AgentUID: 1000, AgentGID: 1000, DataFiles: files}
}

func TestRenderSecurityInvariants(t *testing.T) {
	out, err := Render(testConfig(), testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "_default/mounts") {
		t.Error("template must not inherit _default/mounts: it exposes host $HOME")
	}
	if strings.Contains(out, "reverse-sshfs") {
		t.Error("reverse-sshfs breaks UID-matched ownership and must not appear")
	}
	if strings.Contains(out, "host.docker.internal") {
		t.Error("host.docker.internal must not be defined: the agent has no reason to reach host services")
	}
	if strings.Contains(out, "docker.sock") {
		t.Error("the Docker socket must not be forwarded to the host")
	}
	for _, want := range []string{
		"minimumLimaVersion: 2.2.0",
		"mountType: virtiofs",
		"name: limaadmin",
		"uid: 60000",
		"loadDotSSHPubKeys: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered template missing %q", want)
		}
	}
}

func TestRenderMountsAndSizing(t *testing.T) {
	c := testConfig()
	c.CPUs = 8
	c.Memory = "16GiB"
	out, err := Render(c, testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"cpus: 8",
		`memory: "16GiB"`,
		`location: "/home/st/projects"`,
		`mountPoint: "/home/st/projects"`,
		`location: "/home/st/work/other"`,
		"writable: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered template missing %q", want)
		}
	}
}

// Script bodies are embedded as YAML block scalars. Content with quotes,
// colons and backslashes must survive indentation without corrupting the
// document, so this asserts the indent helper is applied.
func TestRenderIndentsDataFileContent(t *testing.T) {
	files := []guest.DataFile{{
		Path:        "/usr/local/lib/sandbox/tricky.sh",
		Permissions: "0755",
		Content:     "#!/bin/bash\nkey: \"value\"\nprintf '%s\\n' \"a: b\"\n",
	}}
	out, err := Render(testConfig(), testParams(files))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "\n    #!/bin/bash\n") {
		t.Error("data file content must be indented under the content block scalar")
	}
	if !strings.Contains(out, "\n    printf '%s\\n' \"a: b\"\n") {
		t.Error("data file content must be reproduced verbatim inside the block scalar")
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	out, err := Render(testConfig(), testParams([]guest.DataFile{{
		Path: "/usr/local/lib/sandbox/example.sh", Permissions: "0755", Content: "#!/bin/bash\necho hi\n",
	}}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	golden := filepath.Join("testdata", "golden-template.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if out != string(want) {
		t.Errorf("rendered template differs from golden file.\nRegenerate with UPDATE_GOLDEN=1 go test ./internal/lima/ after reviewing the diff.\n--- got ---\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `internal/guest` and `internal/lima` have no implementation (`undefined: DataFiles`, `undefined: Render`).

- [ ] **Step 3: Write the Lima template**

Create `internal/guest/files/lima/code-sandbox.yaml.tpl`. `{{.Agent*}}` values come from `RenderParams`; `{{.Config.*}}` from the host config.

```yaml
# Rendered by code-vm from ~/.config/code-vm/config.yaml. Do not edit by hand:
# `code-vm` regenerates this file on every start.
minimumLimaVersion: 2.2.0

# Only the image is inherited. _default/mounts is deliberately NOT inherited:
# it mounts the host $HOME read-only, which would expose ~/.ssh and ~/.aws.
base:
- template:_images/ubuntu-lts

cpus: {{.Config.CPUs}}
memory: "{{.Config.Memory}}"
disk: "{{.Config.Disk}}"

# containerd is not used; Docker is installed by provisioning instead.
containerd:
  system: false
  user: false

# Lima's own guest user. Privileged, used only by code-vm for session setup.
# The agent runs as {{.AgentUser}} (UID {{.AgentUID}}), created by provisioning
# with no sudo. UID 60000 avoids colliding with the host UID.
user:
  name: limaadmin
  uid: 60000
  comment: "code-vm admin"
  passwordlessSudo: true

ssh:
  # Do not copy host SSH public keys into the guest.
  loadDotSSHPubKeys: false

mounts:
{{- range .Config.Mounts}}
- location: "{{.}}"
  mountPoint: "{{.}}"
  writable: true
{{- end}}
# virtiofs preserves host UIDs, which is what makes the agent's UID match the
# host user's and keeps workspace files host-owned. reverse-sshfs would map
# ownership to the SSH user and break that.
mountType: virtiofs

# No automatic port forwarding: nothing in the guest needs to be reachable
# from the host, and the Docker socket is deliberately not exposed.
portForwards:
- guestPortRange: [1, 65535]
  ignore: true

hostResolver:
  hosts: {}

provision:
{{- range .DataFiles}}
- mode: data
  path: "{{.Path}}"
  owner: "root:root"
  permissions: "{{.Permissions}}"
  overwrite: true
  content: |
{{indent 4 .Content}}
{{- end}}
- mode: system
  script: |
    #!/bin/bash
    set -euo pipefail
    exec /usr/local/lib/sandbox/provision-system.sh

probes:
# init-firewall.sh writes /run/firewall-verify last. Waiting on it makes
# `limactl start` return only once the egress firewall is actually up, so a
# code-vm session can never land in an unfiltered VM.
- mode: readiness
  description: sandbox boot sequence to finish
  script: |
    #!/bin/bash
    set -eu
    timeout 300s bash -c 'until [ -f /run/firewall-verify ]; do sleep 2; done'
  hint: |
    The sandbox boot sequence did not finish. Inspect it with:
      limactl shell code-sandbox sudo journalctl -u sandbox-boot.service
```

Also create the `provision.env` data file source at `internal/guest/files/config/../` — no: `provision.env` is rendered per-instance from config, so it is produced by `Render`, not embedded. Add it in Step 5 as a synthetic data file.

- [ ] **Step 4: Implement `internal/guest/embed.go`**

```go
// Package guest holds the assets installed into the sandbox VM: the Lima
// template and every script, systemd unit and config file delivered to the
// guest. They are embedded in the binary and shipped as Lima `mode: data`
// provision entries, so the CLI and the guest side can never fall out of sync.
package guest

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed all:files
var assets embed.FS

// DataFile is one file to materialise in the guest.
type DataFile struct {
	Path        string
	Permissions string
	Content     string
}

// LimaTemplate returns the unrendered Lima instance template.
func LimaTemplate() (string, error) {
	b, err := assets.ReadFile("files/lima/code-sandbox.yaml.tpl")
	if err != nil {
		return "", fmt.Errorf("read embedded Lima template: %w", err)
	}
	return string(b), nil
}

// guestPath maps an embedded path to its install location and permissions.
// An empty path means the file is not delivered to the guest.
func guestPath(p string) (string, string) {
	rel, ok := trimPrefix(p, "files/scripts/")
	if ok {
		if rel == "sandbox-exec" {
			return "/usr/local/bin/sandbox-exec", "0755"
		}
		return "/usr/local/lib/sandbox/" + rel, "0755"
	}
	if rel, ok := trimPrefix(p, "files/systemd/"); ok {
		return "/etc/systemd/system/" + rel, "0644"
	}
	if rel, ok := trimPrefix(p, "files/config/"); ok {
		return "/usr/local/share/sandbox-config/" + rel, "0444"
	}
	if rel, ok := trimPrefix(p, "files/sandbox-templates/"); ok {
		return "/usr/local/share/sandbox-templates/" + rel, "0444"
	}
	return "", ""
}

func trimPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	return strings.TrimPrefix(s, prefix), true
}

// DataFiles returns every embedded asset that is delivered to the guest,
// sorted by guest path so rendering is deterministic.
func DataFiles() ([]DataFile, error) {
	var out []DataFile
	err := fs.WalkDir(assets, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		dst, perms := guestPath(p)
		if dst == "" {
			return nil // the Lima template is rendered, not delivered
		}
		b, err := assets.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		out = append(out, DataFile{Path: path.Clean(dst), Permissions: perms, Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
```

- [ ] **Step 5: Implement `internal/lima/template.go`**

```go
// Package lima renders the sandbox Lima instance and drives limactl.
package lima

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
)

// InstanceName is the Lima instance code-vm manages.
const InstanceName = "code-sandbox"

// RenderParams carries the host-derived values the template needs.
type RenderParams struct {
	AgentUser string
	AgentUID  int
	AgentGID  int
	DataFiles []guest.DataFile
}

// provisionEnv is delivered as a data file so the guest scripts read their
// inputs from one well-known place instead of relying on Lima passing env.
func provisionEnv(c config.Config, p RenderParams) guest.DataFile {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by code-vm. Sourced by the guest provisioning scripts.\n")
	fmt.Fprintf(&b, "AGENT_USER=%s\n", p.AgentUser)
	fmt.Fprintf(&b, "AGENT_UID=%d\n", p.AgentUID)
	fmt.Fprintf(&b, "AGENT_GID=%d\n", p.AgentGID)
	fmt.Fprintf(&b, "EXTRA_ALLOWED_DOMAINS=%q\n", strings.Join(c.ExtraDomains, " "))
	fmt.Fprintf(&b, "CONTAINER_PROXY=%t\n", c.ContainerProxy)
	return guest.DataFile{Path: "/etc/sandbox/provision.env", Permissions: "0444", Content: b.String()}
}

// indent prefixes every non-empty line with n spaces. YAML block scalars
// reproduce content verbatim, so this is all that is needed to embed
// arbitrary script bodies safely.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// Render produces the Lima instance YAML for the given config.
func Render(c config.Config, p RenderParams) (string, error) {
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("invalid config: %w", err)
	}
	raw, err := guest.LimaTemplate()
	if err != nil {
		return "", err
	}
	tpl, err := template.New("lima").Funcs(template.FuncMap{"indent": indent}).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse Lima template: %w", err)
	}
	files := append([]guest.DataFile{provisionEnv(c, p)}, p.DataFiles...)
	data := struct {
		Config    config.Config
		AgentUser string
		AgentUID  int
		AgentGID  int
		DataFiles []guest.DataFile
	}{c, p.AgentUser, p.AgentUID, p.AgentGID, files}

	var out strings.Builder
	if err := tpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render Lima template: %w", err)
	}
	return out.String(), nil
}
```

- [ ] **Step 6: Create placeholder guest assets so the embed compiles**

`go:embed all:files` fails if a referenced directory is absent, and `DataFiles` tests expect specific paths. Create minimal versions now; Tasks 4–8 fill them in.

```bash
cd /workspace/vm-sandbox
mkdir -p internal/guest/files/{scripts,systemd,config/.claude,sandbox-templates}
printf '#!/bin/bash\nset -euo pipefail\necho "[provision] placeholder"\n' > internal/guest/files/scripts/provision-system.sh
printf '#!/bin/bash\nset -a\n[ -f /etc/environment ] && . /etc/environment\nset +a\nexec "$@"\n' > internal/guest/files/scripts/sandbox-exec
printf '[Unit]\nDescription=code-vm sandbox boot sequence\n\n[Service]\nType=oneshot\nExecStart=/bin/true\n' > internal/guest/files/systemd/sandbox-boot.service
printf '{}\n' > internal/guest/files/config/.claude/settings.json
```

- [ ] **Step 7: Generate the golden file and run the tests**

```bash
cd /workspace/vm-sandbox
UPDATE_GOLDEN=1 mise x -- go test ./internal/lima/
```

Read `internal/lima/testdata/golden-template.yaml` and confirm by eye that it contains no `_default/mounts`, no `docker.sock`, `mountType: virtiofs`, `uid: 60000`, and both mount entries. Then:

Run: `mise run test:unit`
Expected: PASS — all tests in `internal/config`, `internal/guest`, `internal/lima`.

- [ ] **Step 8: Commit**

```bash
cd /workspace/vm-sandbox
git add internal/guest internal/lima
git commit -m "feat: embed guest assets and render the Lima instance template

Guest scripts ship as Lima mode:data entries with overwrite:true, so a
CLI upgrade refreshes the guest side on the next start and the two can
never skew. A readiness probe on /run/firewall-verify makes limactl
start block until the egress firewall is actually up."
```

---

### Task 3: limactl wrapper and `code-vm doctor`

**Files:**
- Create: `internal/lima/limactl.go`, `internal/cli/root.go`, `internal/cli/doctor.go`, `cmd/code-vm/main.go`
- Test: `internal/lima/limactl_test.go`, `internal/cli/doctor_test.go`

**Interfaces:**
- Consumes: `config.Load`, `config.DefaultPath` (Task 1); `lima.InstanceName` (Task 2).
- Produces:
  - `lima.Runner` interface: `Run(ctx context.Context, args ...string) error`, `Output(ctx context.Context, args ...string) ([]byte, error)`
  - `lima.ExecRunner` struct: `Bin string`, `Stdin io.Reader`, `Stdout io.Writer`, `Stderr io.Writer`; implements `Runner`
  - `lima.Client` struct: `R Runner`
  - `lima.NewClient() lima.Client` — an `ExecRunner` wired to `limactl` and the process std streams
  - `func (c Client) Status(ctx) (string, error)` — `"Running"`, `"Stopped"`, or `""` when the instance does not exist
  - `func (c Client) Start(ctx, tplPath string) error`, `Stop(ctx) error`, `Delete(ctx) error`
  - `func (c Client) Copy(ctx, localPath, guestPath string) error`
  - `func (c Client) AgentArgs(workdir string, cmd []string) []string`
  - `func (c Client) AdminArgs(cmd []string) []string`
  - `func (c Client) Agent(ctx, workdir string, cmd []string) error`
  - `func (c Client) Admin(ctx context.Context, cmd []string) error`
  - `func (c Client) AdminOutput(ctx context.Context, cmd []string) ([]byte, error)`
  - `func (c Client) Version(ctx) (string, error)`
  - `cli.Execute() error`, `cli.NewRootCmd() *cobra.Command`
  - `cli.parseLimaVersion(out string) (string, error)`, `cli.atLeastLimaVersion(got string) error`

- [ ] **Step 1: Write the failing tests**

`internal/lima/limactl_test.go`:

```go
package lima

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  [][]string
	out    []byte
	outErr error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out, f.outErr
}

func TestAgentArgsRunsThroughSandboxExecAsDevuser(t *testing.T) {
	c := Client{R: &fakeRunner{}}
	got := c.AgentArgs("/home/st/projects/repo", []string{"claude", "-p", "fix the bug"})
	want := []string{
		"shell", "--workdir", "/home/st/projects/repo", InstanceName,
		"sudo", "/usr/local/bin/sandbox-exec", "claude", "-p", "fix the bug",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentArgs() = %v, want %v", got, want)
	}
}

func TestAdminArgsHasNoWorkdirAndNoSandboxExec(t *testing.T) {
	c := Client{R: &fakeRunner{}}
	got := c.AdminArgs([]string{"squid", "-k", "reconfigure"})
	want := []string{"shell", InstanceName, "sudo", "squid", "-k", "reconfigure"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AdminArgs() = %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "sandbox-exec") {
			t.Error("admin commands must not go through sandbox-exec: they run as limaadmin, not the agent")
		}
	}
}

func TestStatusParsesListOutput(t *testing.T) {
	f := &fakeRunner{out: []byte("Running\n")}
	got, err := Client{R: f}.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != "Running" {
		t.Errorf("Status() = %q, want \"Running\"", got)
	}
	want := []string{"list", InstanceName, "--format", "{{.Status}}"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestStatusEmptyWhenInstanceAbsent(t *testing.T) {
	got, err := Client{R: &fakeRunner{out: []byte("\n")}}.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != "" {
		t.Errorf("Status() = %q, want \"\"", got)
	}
}

func TestStartIsNonInteractive(t *testing.T) {
	f := &fakeRunner{}
	if err := (Client{R: f}).Start(context.Background(), "/tmp/code-sandbox.yaml"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"start", "--tty=false", "--name", InstanceName, "/tmp/code-sandbox.yaml"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestCopyUsesInstancePrefixedDestination(t *testing.T) {
	f := &fakeRunner{}
	if err := (Client{R: f}).Copy(context.Background(), "/tmp/local.json", "/tmp/guest.json"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	want := []string{"copy", "/tmp/local.json", InstanceName + ":/tmp/guest.json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}
```

`internal/cli/doctor_test.go`:

```go
package cli

import "testing"

func TestParseLimaVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"standard output", "limactl version 2.2.0\n", "2.2.0", false},
		{"with v prefix", "limactl version v2.3.1\n", "2.3.1", false},
		{"unparseable", "some other tool\n", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimaVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLimaVersion(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLimaVersion(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseLimaVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAtLeastLimaVersion(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"2.2.0", false},
		{"2.2.1", false},
		{"2.3.0", false},
		{"3.0.0", false},
		{"2.1.9", true},
		{"1.9.9", true},
		{"garbage", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			err := atLeastLimaVersion(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("atLeastLimaVersion(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `undefined: Client`, `undefined: parseLimaVersion`.

- [ ] **Step 3: Implement `internal/lima/limactl.go`**

```go
package lima

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner executes limactl. It is an interface so command construction can be
// tested without a VM.
type Runner interface {
	Run(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the real limactl binary.
type ExecRunner struct {
	Bin    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (e ExecRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	bin := e.Bin
	if bin == "" {
		bin = "limactl"
	}
	return exec.CommandContext(ctx, bin, args...)
}

// Run streams the command's output to the configured writers.
func (e ExecRunner) Run(ctx context.Context, args ...string) error {
	cmd := e.command(ctx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Output captures stdout. Stderr is streamed so Lima's progress and errors
// stay visible.
func (e ExecRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := e.command(ctx, args...)
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, &out, e.Stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("limactl %s: %w", strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// Client drives the sandbox Lima instance.
type Client struct {
	R Runner
}

// NewClient returns a Client wired to the real limactl and this process's
// standard streams.
func NewClient() Client {
	return Client{R: ExecRunner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}}
}

// AgentArgs builds the argv that runs cmd as the agent user in workdir.
// sandbox-exec sources /etc/environment and then drops from limaadmin to the
// agent user, preserving the working directory.
func (c Client) AgentArgs(workdir string, cmd []string) []string {
	args := []string{"shell", "--workdir", workdir, InstanceName, "sudo", "/usr/local/bin/sandbox-exec"}
	return append(args, cmd...)
}

// AdminArgs builds the argv that runs cmd as root via limaadmin's sudo.
func (c Client) AdminArgs(cmd []string) []string {
	args := []string{"shell", InstanceName, "sudo"}
	return append(args, cmd...)
}

// Agent runs cmd as the agent user in workdir.
func (c Client) Agent(ctx context.Context, workdir string, cmd []string) error {
	return c.R.Run(ctx, c.AgentArgs(workdir, cmd)...)
}

// Admin runs cmd as root in the guest.
func (c Client) Admin(ctx context.Context, cmd []string) error {
	return c.R.Run(ctx, c.AdminArgs(cmd)...)
}

// AdminOutput runs cmd as root in the guest and captures stdout.
func (c Client) AdminOutput(ctx context.Context, cmd []string) ([]byte, error) {
	return c.R.Output(ctx, c.AdminArgs(cmd)...)
}

// Status returns the instance status, or "" when the instance does not exist.
func (c Client) Status(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "list", InstanceName, "--format", "{{.Status}}")
	if err != nil {
		// A missing instance is not an error condition for callers.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Start creates or starts the instance from the rendered template.
func (c Client) Start(ctx context.Context, tplPath string) error {
	return c.R.Run(ctx, "start", "--tty=false", "--name", InstanceName, tplPath)
}

// Stop shuts the instance down.
func (c Client) Stop(ctx context.Context) error {
	return c.R.Run(ctx, "stop", InstanceName)
}

// Delete removes the instance and its disk.
func (c Client) Delete(ctx context.Context) error {
	return c.R.Run(ctx, "delete", "--force", InstanceName)
}

// Copy copies a host file into the guest.
func (c Client) Copy(ctx context.Context, localPath, guestPath string) error {
	return c.R.Run(ctx, "copy", localPath, InstanceName+":"+guestPath)
}

// Version returns limactl's reported version string.
func (c Client) Version(ctx context.Context) (string, error) {
	out, err := c.R.Output(ctx, "--version")
	return string(out), err
}
```

- [ ] **Step 4: Implement `internal/cli/root.go`**

```go
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
	}
	root.PersistentFlags().StringVar(&configPath, "config", "", "path to config.yaml (default ~/.config/code-vm/config.yaml)")
	root.AddCommand(newDoctorCmd())
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
```

- [ ] **Step 5: Implement `internal/cli/doctor.go`**

```go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
)

// minLima is the lowest Lima version code-vm supports. 2.2.0 is the version
// the template and mode:data provisioning were developed against.
var minLima = [3]int{2, 2, 0}

var limaVersionRe = regexp.MustCompile(`version\s+v?([0-9]+\.[0-9]+\.[0-9]+)`)

// parseLimaVersion extracts the semantic version from `limactl --version`.
func parseLimaVersion(out string) (string, error) {
	m := limaVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cannot parse limactl version from %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// atLeastLimaVersion reports whether got satisfies the minimum.
func atLeastLimaVersion(got string) error {
	parts := strings.Split(got, ".")
	if len(parts) != 3 {
		return fmt.Errorf("unrecognised version %q", got)
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("unrecognised version %q", got)
		}
		v[i] = n
	}
	for i := range v {
		if v[i] > minLima[i] {
			return nil
		}
		if v[i] < minLima[i] {
			return fmt.Errorf("Lima %s is too old; need %d.%d.%d or newer",
				got, minLima[0], minLima[1], minLima[2])
		}
	}
	return nil
}

type check struct {
	name string
	err  error
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			checks := []check{
				{"limactl on PATH", checkBinary("limactl")},
				{"Lima version", checkLimaVersion(ctx)},
				{"virtiofsd on PATH", checkBinary("virtiofsd")},
				{"KVM accessible", checkKVM()},
				{"config valid", checkConfig()},
			}
			failed := 0
			for _, c := range checks {
				if c.err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  OK   %s\n", c.name)
					continue
				}
				failed++
				fmt.Fprintf(cmd.OutOrStdout(), "  FAIL %s: %v\n", c.name, c.err)
			}
			if failed > 0 {
				return fmt.Errorf("%d prerequisite check(s) failed", failed)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nAll prerequisites satisfied.")
			return nil
		},
	}
}

func checkBinary(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("not found on PATH (install it, e.g. via your package manager)")
	}
	return nil
}

func checkLimaVersion(ctx context.Context) error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return errors.New("limactl not found; skipping version check")
	}
	out, err := lima.NewClient().Version(ctx)
	if err != nil {
		return err
	}
	v, err := parseLimaVersion(out)
	if err != nil {
		return err
	}
	return atLeastLimaVersion(v)
}

func checkKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm not usable (%v); add your user to the kvm group or enable virtualisation in firmware", err)
	}
	return f.Close()
}

func checkConfig() error {
	c, path, err := loadConfig()
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if fi, err := os.Stat(c.ProjectsRoot); err != nil {
		return fmt.Errorf("projectsRoot %s: %w", c.ProjectsRoot, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("projectsRoot %s is not a directory", c.ProjectsRoot)
	}
	return nil
}
```

- [ ] **Step 6: Implement `cmd/code-vm/main.go`**

```go
// Command code-vm runs coding agents in a hardened Lima VM.
package main

import (
	"fmt"
	"os"

	"github.com/wetransform/code-vm/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "code-vm: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `mise run test:unit`
Expected: PASS.

Then exercise the real command:

Run: `mise run build && ./dist/code-vm doctor`
Expected: a check list. Failures for `virtiofsd` or `limactl` are legitimate findings about this host, not test failures — record which ones failed. `config valid` will fail if `~/projects` does not exist; create it or set `projectsRoot` in `~/.config/code-vm/config.yaml`.

- [ ] **Step 8: Commit**

```bash
cd /workspace/vm-sandbox
git add internal/lima internal/cli cmd
git commit -m "feat: add limactl wrapper and code-vm doctor

Agent commands go through sudo /usr/local/bin/sandbox-exec so they run
as the non-sudo agent user with the working directory preserved; admin
commands run as root and deliberately bypass sandbox-exec."
```

---

### Task 4: Guest provisioning — non-sudo agent user with rootless Docker

**Files:**
- Create: `internal/guest/files/scripts/provision-user-docker.sh`
- Create: `internal/cli/start.go`
- Create: `test-vm-sandbox.sh`
- Modify: `internal/guest/files/scripts/provision-system.sh` (replace the Task 2 placeholder)
- Test: `internal/cli/start_test.go`, `test-vm-sandbox.sh`

**Interfaces:**
- Consumes: `lima.Client`, `lima.Render`, `lima.RenderParams`, `guest.DataFiles`, `config.Config`.
- Produces:
  - `cli.newClient` package variable of type `func() lima.Client`, defaulting to `lima.NewClient`, so tests can substitute a fake
  - `cli.renderParams() (lima.RenderParams, error)` — agent user `devuser`, UID/GID from `os.Getuid()`/`os.Getgid()`, data files from `guest.DataFiles()`
  - `cli.renderInstanceFile(c config.Config) (string, error)` — writes the rendered YAML to a temp file, mode `0600`, returns its path
  - `cli.ensureRunning(ctx context.Context, cl lima.Client, c config.Config) error` — starts the instance if absent or stopped, no-op when running
  - `code-vm start` subcommand

- [ ] **Step 1: Write the failing Go tests**

`internal/cli/start_test.go`:

```go
package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

type recordingRunner struct {
	statusOut string
	calls     [][]string
}

func (r *recordingRunner) Run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, args)
	return nil
}

func (r *recordingRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return []byte(r.statusOut), nil
}

func (r *recordingRunner) started() bool {
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "start" {
			return true
		}
	}
	return false
}

func testCfg(t *testing.T) config.Config {
	t.Helper()
	c := config.Default()
	c.ProjectsRoot = t.TempDir()
	return c
}

func TestEnsureRunningStartsWhenAbsentOrStopped(t *testing.T) {
	for _, status := range []string{"", "Stopped", "Broken"} {
		t.Run("status="+status, func(t *testing.T) {
			r := &recordingRunner{statusOut: status}
			if err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t)); err != nil {
				t.Fatalf("ensureRunning: %v", err)
			}
			if !r.started() {
				t.Errorf("expected a start call for status %q, calls=%v", status, r.calls)
			}
		})
	}
}

func TestEnsureRunningIsNoOpWhenRunning(t *testing.T) {
	r := &recordingRunner{statusOut: "Running"}
	if err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t)); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if r.started() {
		t.Errorf("must not start an already-running instance, calls=%v", r.calls)
	}
}

func TestRenderInstanceFileIsPrivateAndComplete(t *testing.T) {
	c := testCfg(t)
	path, err := renderInstanceFile(c)
	if err != nil {
		t.Fatalf("renderInstanceFile: %v", err)
	}
	defer os.Remove(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("rendered instance file mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "/etc/sandbox/provision.env") {
		t.Error("rendered instance must include the provision.env data file")
	}
	if !strings.Contains(s, "/usr/local/lib/sandbox/provision-system.sh") {
		t.Error("rendered instance must deliver and invoke provision-system.sh")
	}
	if !strings.Contains(s, c.ProjectsRoot) {
		t.Error("rendered instance must mount the projects root")
	}
}

func TestRenderParamsUsesHostIdentity(t *testing.T) {
	p, err := renderParams()
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	if p.AgentUser != "devuser" {
		t.Errorf("AgentUser = %q, want \"devuser\"", p.AgentUser)
	}
	if p.AgentUID != os.Getuid() || p.AgentGID != os.Getgid() {
		t.Errorf("agent identity = %d:%d, want host %d:%d", p.AgentUID, p.AgentGID, os.Getuid(), os.Getgid())
	}
	if len(p.DataFiles) == 0 {
		t.Error("DataFiles must be populated from the embedded assets")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `undefined: ensureRunning`, `undefined: renderInstanceFile`, `undefined: renderParams`.

- [ ] **Step 3: Implement `internal/cli/start.go`**

```go
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/lima"
)

// newClient is a package variable so tests can substitute a fake runner.
var newClient = lima.NewClient

// agentUser is the guest account the agent runs as. Its UID and GID mirror
// the host user's so virtiofs-shared files are owned by it.
const agentUser = "devuser"

// renderParams gathers the host-derived values the Lima template needs.
func renderParams() (lima.RenderParams, error) {
	files, err := guest.DataFiles()
	if err != nil {
		return lima.RenderParams{}, err
	}
	return lima.RenderParams{
		AgentUser: agentUser,
		AgentUID:  os.Getuid(),
		AgentGID:  os.Getgid(),
		DataFiles: files,
	}, nil
}

// renderInstanceFile writes the rendered Lima instance to a temp file and
// returns its path. The caller is responsible for removing it.
func renderInstanceFile(c config.Config) (string, error) {
	p, err := renderParams()
	if err != nil {
		return "", err
	}
	body, err := lima.Render(c, p)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "code-sandbox-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp instance file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod temp instance file: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		return "", fmt.Errorf("write temp instance file: %w", err)
	}
	return f.Name(), nil
}

// ensureRunning brings the sandbox VM up if it is not already running.
// The rendered template is passed on every start, so a code-vm upgrade or a
// config change is picked up without a separate migration step: the
// mode:data guest files carry overwrite: true.
func ensureRunning(ctx context.Context, cl lima.Client, c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	status, err := cl.Status(ctx)
	if err != nil {
		return err
	}
	if status == "Running" {
		return nil
	}
	path, err := renderInstanceFile(c)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	return cl.Start(ctx, path)
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the sandbox VM (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), newClient(), c)
		},
	}
}
```

Register it in `NewRootCmd`, next to the existing `newDoctorCmd()` line:

```go
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newStartCmd())
```

- [ ] **Step 4: Write `provision-system.sh`**

Replace the Task 2 placeholder at `internal/guest/files/scripts/provision-system.sh`:

```bash
#!/bin/bash
###############################################################################
# provision-system.sh — root provisioning for the code-vm sandbox VM
#
# Runs on every boot via Lima `provision: mode: system`, after the mode:data
# files have been written. Every step is idempotent.
#
# Inputs come from /etc/sandbox/provision.env, a mode:data file rendered by
# code-vm: AGENT_USER, AGENT_UID, AGENT_GID, EXTRA_ALLOWED_DOMAINS,
# CONTAINER_PROXY.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

log() { echo "[provision] $*"; }

export DEBIAN_FRONTEND=noninteractive

# ── Agent user ───────────────────────────────────────────────────────────────
# UID/GID mirror the host user so virtiofs-shared workspace files are genuinely
# owned by the agent, and stay host-owned when viewed from the host.
if ! getent group "$AGENT_GID" > /dev/null; then
    groupadd -g "$AGENT_GID" "$AGENT_USER"
fi
if ! id -u "$AGENT_USER" > /dev/null 2>&1; then
    useradd -m -u "$AGENT_UID" -g "$AGENT_GID" -s /bin/bash "$AGENT_USER"
    log "Created $AGENT_USER (uid=$AGENT_UID gid=$AGENT_GID)"
fi

# The agent must never hold sudo. Re-asserted on every boot, not just creation.
deluser "$AGENT_USER" sudo > /dev/null 2>&1 || true
rm -f "/etc/sudoers.d/${AGENT_USER}" "/etc/sudoers.d/99-${AGENT_USER}"

# Subordinate ID ranges for rootless Docker's user namespaces.
grep -q "^${AGENT_USER}:" /etc/subuid || echo "${AGENT_USER}:100000:65536" >> /etc/subuid
grep -q "^${AGENT_USER}:" /etc/subgid || echo "${AGENT_USER}:100000:65536" >> /etc/subgid

# Keep the agent's systemd user instance alive without a login session, so
# rootless dockerd survives between code-vm invocations.
loginctl enable-linger "$AGENT_USER"

# ── Packages ─────────────────────────────────────────────────────────────────
NEEDED=(uidmap dbus-user-session iptables squid util-linux git jq curl ca-certificates)
MISSING=()
for p in "${NEEDED[@]}"; do
    dpkg -s "$p" > /dev/null 2>&1 || MISSING+=("$p")
done
if [ ${#MISSING[@]} -gt 0 ]; then
    log "Installing packages: ${MISSING[*]}"
    apt-get update -qq
    apt-get install -y -qq "${MISSING[@]}"
fi

# Rootless Docker manages iptables inside its own network namespace.
modprobe ip_tables > /dev/null 2>&1 || true
modprobe iptable_nat > /dev/null 2>&1 || true
modprobe ip6_tables > /dev/null 2>&1 || true

# ── mise, yq, gomplate ───────────────────────────────────────────────────────
if [ ! -x /usr/local/bin/mise ]; then
    log "Installing mise"
    curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh
fi
for tool in yq gomplate; do
    if [ ! -x "/usr/local/bin/$tool" ]; then
        log "Installing $tool via mise"
        /usr/local/bin/mise use -g -y "$tool"
        ln -sf "$(/usr/local/bin/mise which "$tool")" "/usr/local/bin/$tool"
    fi
done

# ── Docker ───────────────────────────────────────────────────────────────────
if ! command -v docker > /dev/null 2>&1; then
    log "Installing Docker"
    curl -fsSL https://get.docker.com | sh
fi
# The rootful daemon is never used: the agent runs its own rootless dockerd,
# which is what keeps guest root separated from the agent.
systemctl disable --now docker.service docker.socket containerd.service containerd.socket > /dev/null 2>&1 || true
systemctl mask docker.service docker.socket containerd.service containerd.socket > /dev/null 2>&1 || true

# ── Sandbox-managed environment ──────────────────────────────────────────────
# Single source of truth for proxy and Docker env. sandbox-exec sources this
# because `limactl shell` launches a non-login shell that would never read it.
cat > /etc/environment <<EOF
http_proxy=http://localhost:3128
https_proxy=http://localhost:3128
HTTP_PROXY=http://localhost:3128
HTTPS_PROXY=http://localhost:3128
no_proxy=localhost,127.0.0.1
NO_PROXY=localhost,127.0.0.1
JAVA_TOOL_OPTIONS="-Dhttp.proxyHost=localhost -Dhttp.proxyPort=3128 -Dhttps.proxyHost=localhost -Dhttps.proxyPort=3128 -Dhttp.nonProxyHosts=localhost|127.0.0.1"
DOCKER_HOST=unix:///run/user/${AGENT_UID}/docker.sock
EOF

# ── Resource limits ──────────────────────────────────────────────────────────
# Replaces the container sandbox's --pids-limit. MemoryMax leaves headroom for
# the guest OS and Squid.
TOTAL_KB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
MEM_MAX_MB=$((TOTAL_KB / 1024 - 2048))
[ "$MEM_MAX_MB" -lt 1024 ] && MEM_MAX_MB=1024
install -d "/etc/systemd/system/user-${AGENT_UID}.slice.d"
cat > "/etc/systemd/system/user-${AGENT_UID}.slice.d/50-sandbox.conf" <<EOF
[Slice]
TasksMax=2048
MemoryMax=${MEM_MAX_MB}M
EOF
systemctl daemon-reload

# ── Rootless Docker for the agent ────────────────────────────────────────────
# Lima's `mode: user` scripts run as limaadmin, so the agent's rootless setup is
# driven from here into the agent's own systemd user session.
if [ ! -S "/run/user/${AGENT_UID}/docker.sock" ]; then
    log "Setting up rootless Docker for $AGENT_USER"
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups --reset-env \
        env HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin \
        CONTAINER_PROXY="$CONTAINER_PROXY" \
        bash /usr/local/lib/sandbox/provision-user-docker.sh
fi

# ── Boot sequence ────────────────────────────────────────────────────────────
# sandbox-boot.service is ordered after cloud-final.service so on later boots it
# runs only once provisioning has finished — provisioning needs unrestricted
# egress for apt and get.docker.com, and the firewall closes at the end of the
# boot sequence. Enabling a unit mid-boot does not queue it for this boot, so
# start it explicitly here too.
systemctl enable sandbox-boot.service > /dev/null 2>&1 || true
systemctl start --no-block sandbox-boot.service

log "Provisioning complete"
```

Note on `mise use -g -y yq`: if `mise ls-remote yq` or `mise ls-remote gomplate` does not resolve, use the explicit backend names `aqua:mikefarah/yq` and `aqua:hairyhenderson/gomplate` and keep the `mise which` symlink step unchanged.

- [ ] **Step 5: Write `provision-user-docker.sh`**

Create `internal/guest/files/scripts/provision-user-docker.sh`:

```bash
#!/bin/bash
###############################################################################
# provision-user-docker.sh — rootless Docker setup, runs as the agent user
#
# Invoked from provision-system.sh via setpriv. Expects HOME, USER,
# XDG_RUNTIME_DIR and CONTAINER_PROXY in the environment.
###############################################################################
set -euo pipefail

log() { echo "[provision-user] $*"; }

systemctl --user start dbus.service > /dev/null 2>&1 || true

if ! systemctl --user is-enabled docker.service > /dev/null 2>&1; then
    log "Running dockerd-rootless-setuptool.sh"
    dockerd-rootless-setuptool.sh install
fi

# dockerd pulls images itself, so the daemon needs the proxy.
install -d "$HOME/.config/systemd/user/docker.service.d"
cat > "$HOME/.config/systemd/user/docker.service.d/http-proxy.conf" <<'EOF'
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
Environment="NO_PROXY=localhost,127.0.0.1"
EOF

# Container *runtime* proxy env is opt-in. Injecting it by default would also
# apply to compose services, where a bare service name such as "db" matches no
# noProxy entry and would be sent to Squid — breaking service-to-service
# traffic, which is exactly what this sandbox exists to fix.
install -d "$HOME/.config/docker"
if [ "${CONTAINER_PROXY:-false}" = "true" ]; then
    GUEST_IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)
    cat > "$HOME/.config/docker/config.json" <<EOF
{
  "proxies": {
    "default": {
      "httpProxy": "http://${GUEST_IP}:3128",
      "httpsProxy": "http://${GUEST_IP}:3128",
      "noProxy": "localhost,127.0.0.1,172.16.0.0/12,10.0.0.0/8,192.168.0.0/16"
    }
  }
}
EOF
    log "Container proxy env enabled (containerProxy=true)"
else
    printf '{}\n' > "$HOME/.config/docker/config.json"
fi

systemctl --user daemon-reload
systemctl --user enable --now docker.service
docker context use rootless > /dev/null
log "Rootless Docker ready"
```

- [ ] **Step 6: Replace the placeholder `sandbox-boot.service`**

`internal/guest/files/systemd/sandbox-boot.service` — a minimal version that will grow in Task 6:

```ini
[Unit]
Description=code-vm sandbox boot sequence
# cloud-final.service is where Lima's provisioning runs. Provisioning needs
# unrestricted egress, so the boot sequence (which closes the firewall) must
# not start before it finishes.
After=cloud-final.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/lib/sandbox/sandbox-boot.sh
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
```

And a minimal `internal/guest/files/scripts/sandbox-boot.sh`. Task 6 adds the lock and firewall steps; for now it must create `/run/firewall-verify` or the readiness probe will time out:

```bash
#!/bin/bash
###############################################################################
# sandbox-boot.sh — the VM's equivalent of the container's entrypoint.sh
#
# Ordered sequence, run as root after provisioning completes:
#   1. update the agent CLIs      (needs unrestricted egress)
#   2. lock the Claude settings   (Task 6)
#   3. initialise the firewall    (Task 6 — closes egress, so it goes last)
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

echo "[boot] Sandbox boot sequence starting"

# Placeholder until Task 6 lands the firewall. The readiness probe waits on
# this file, so it must exist for `limactl start` to return.
echo "PLACEHOLDER=yes" > /run/firewall-verify
chmod 0444 /run/firewall-verify

echo "[boot] Sandbox boot sequence complete"
```

- [ ] **Step 7: Write the VM integration suite with its first assertions**

Create `test-vm-sandbox.sh`. The helpers mirror the container sandbox's `test-sandbox.sh` so output is familiar. At this stage assertions run through `limactl` directly — the `code-vm` exec path arrives in Task 5.

```bash
#!/bin/bash
###############################################################################
# test-vm-sandbox.sh — VM sandbox security and functionality suite
#
# Requires KVM. Run via: mise run test:vm
###############################################################################
set -uo pipefail

INSTANCE=code-sandbox
CODE_VM=./dist/code-vm
AGENT_USER=devuser

PASS=0
FAIL=0

pass() {
    PASS=$((PASS + 1))
    echo "  PASS: $1"
}

fail() {
    FAIL=$((FAIL + 1))
    echo "  FAIL: $1"
}

# Run a command as root in the guest.
adm() { limactl shell "$INSTANCE" sudo "$@"; }

# Run a command as the agent user in the guest.
agent() { limactl shell "$INSTANCE" sudo /usr/local/bin/sandbox-exec "$@"; }

assert_ok() {
    local desc="$1"
    shift
    if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

assert_fails() {
    local desc="$1"
    shift
    if "$@" > /dev/null 2>&1; then fail "$desc (command unexpectedly succeeded)"; else pass "$desc"; fi
}

echo ""
echo "================================================================"
echo "  VM Sandbox Test Suite"
echo "================================================================"
echo ""

echo "[test] Building code-vm..."
if mise run build; then pass "code-vm builds"; else fail "code-vm builds"; exit 1; fi

echo "[test] Starting the sandbox VM (first boot provisions; this takes minutes)..."
if "$CODE_VM" start; then pass "VM starts"; else fail "VM starts"; exit 1; fi

echo ""
echo "── Agent user isolation ──────────────────────────────────────────"

assert_fails "agent cannot sudo" \
    limactl shell "$INSTANCE" sudo -u "$AGENT_USER" sudo -n true

if [ "$(adm id -u "$AGENT_USER")" = "$(id -u)" ]; then
    pass "agent UID matches the host user"
else
    fail "agent UID matches the host user (guest=$(adm id -u "$AGENT_USER") host=$(id -u))"
fi

if adm id -nG "$AGENT_USER" | tr ' ' '\n' | grep -qx sudo; then
    fail "agent is not in the sudo group"
else
    pass "agent is not in the sudo group"
fi

echo ""
echo "── Host filesystem exposure ──────────────────────────────────────"

assert_fails "host \$HOME is not mounted in the guest" \
    adm test -d "$HOME/.ssh"

assert_ok "projects root is mounted" \
    adm test -d "$(mise x -- go run ./cmd/code-vm status --print-projects-root 2>/dev/null || echo "$HOME/projects")"

echo ""
echo "── Docker ────────────────────────────────────────────────────────"

assert_ok "rootless docker responds to the agent" agent docker info
assert_fails "rootful docker.service is masked" adm systemctl is-enabled docker.service

if adm test -S "/run/user/$(id -u)/docker.sock"; then
    pass "rootless docker socket exists at the agent's runtime dir"
else
    fail "rootless docker socket exists at the agent's runtime dir"
fi

echo ""
echo "================================================================"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "================================================================"
[ "$FAIL" -eq 0 ]
```

The `projects root is mounted` assertion above depends on a flag that does not exist yet. Replace that single assertion with a direct check against the configured value:

```bash
PROJECTS_ROOT=$(sed -n 's/^projectsRoot: *//p' "${HOME}/.config/code-vm/config.yaml" 2>/dev/null)
PROJECTS_ROOT=${PROJECTS_ROOT:-$HOME/projects}
PROJECTS_ROOT=${PROJECTS_ROOT/#\~/$HOME}
assert_ok "projects root is mounted" adm test -d "$PROJECTS_ROOT"
```

- [ ] **Step 8: Run the Go tests, then the VM suite**

Run: `mise run test:unit && mise run lint && mise run fmt-check`
Expected: PASS. `shellcheck` now has real scripts to check; fix anything it reports (quote expansions, `SC2086`) before continuing.

Run: `mise run test:vm`
Expected: all assertions PASS. First boot takes several minutes.

If the readiness probe times out, read the boot log:

```bash
limactl shell code-sandbox sudo journalctl -u sandbox-boot.service --no-pager
limactl shell code-sandbox sudo cat /var/log/cloud-init-output.log
```

- [ ] **Step 9: Commit**

```bash
cd /workspace/vm-sandbox
git add internal/guest internal/cli test-vm-sandbox.sh
git commit -m "feat: provision a non-sudo agent user with rootless Docker

The agent's UID/GID mirror the host user's so virtiofs keeps workspace
files host-owned. Lima's mode:user scripts run as limaadmin, so the
agent's rootless dockerd is set up from the system stage via setpriv
into the agent's own systemd user session. Container runtime proxy env
is opt-in: injecting it by default breaks compose service-to-service
traffic, since a bare service name matches no noProxy entry."
```

---

### Task 5: `sandbox-exec` and the default exec path

**Files:**
- Modify: `internal/guest/files/scripts/sandbox-exec` (replace the Task 2 placeholder)
- Create: `internal/cli/shell.go`
- Modify: `internal/cli/root.go` (wire the default action)
- Test: `internal/cli/shell_test.go`, `test-vm-sandbox.sh`

**Interfaces:**
- Consumes: `cli.ensureRunning`, `cli.newClient`, `config.CoveringMount`, `config.Config.Mounts`.
- Produces:
  - `cli.agentCommand(args []string) []string` — returns `args`, or `["bash", "-l"]` when empty
  - `cli.resolveWorkdir(c config.Config, cwd string) (string, error)` — validates the cwd is under a declared mount and returns it
  - the root command's default action: `code-vm` and `code-vm -- cmd args...`

- [ ] **Step 1: Write the failing tests**

`internal/cli/shell_test.go`:

```go
package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestAgentCommandDefaultsToLoginShell(t *testing.T) {
	if got := agentCommand(nil); !reflect.DeepEqual(got, []string{"bash", "-l"}) {
		t.Errorf("agentCommand(nil) = %v, want [bash -l]", got)
	}
	if got := agentCommand([]string{}); !reflect.DeepEqual(got, []string{"bash", "-l"}) {
		t.Errorf("agentCommand([]) = %v, want [bash -l]", got)
	}
}

func TestAgentCommandPassesArgsThrough(t *testing.T) {
	in := []string{"claude", "-p", "fix the bug"}
	got := agentCommand(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("agentCommand(%v) = %v, want unchanged", in, got)
	}
}

func TestResolveWorkdirAcceptsCoveredPath(t *testing.T) {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	got, err := resolveWorkdir(c, "/home/st/projects/repo/sub")
	if err != nil {
		t.Fatalf("resolveWorkdir: %v", err)
	}
	if got != "/home/st/projects/repo/sub" {
		t.Errorf("resolveWorkdir = %q", got)
	}
}

func TestResolveWorkdirRejectsUncoveredPathWithActionableError(t *testing.T) {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	_, err := resolveWorkdir(c, "/tmp/elsewhere")
	if err == nil {
		t.Fatal("expected an error for a path outside every mount")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/tmp/elsewhere") {
		t.Errorf("error must name the offending path, got %q", msg)
	}
	if !strings.Contains(msg, "code-vm mount") {
		t.Errorf("error must tell the user how to fix it, got %q", msg)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `undefined: agentCommand`, `undefined: resolveWorkdir`.

- [ ] **Step 3: Implement `internal/cli/shell.go`**

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
)

// agentCommand returns the command to run in the guest, defaulting to an
// interactive login shell.
func agentCommand(args []string) []string {
	if len(args) == 0 {
		return []string{"bash", "-l"}
	}
	return args
}

// resolveWorkdir checks that cwd is inside a declared mount. Lima declares
// mounts in the instance config, so a directory that is not shared cannot be
// reached from the guest at all.
func resolveWorkdir(c config.Config, cwd string) (string, error) {
	mounts := c.Mounts()
	if _, ok := config.CoveringMount(mounts, cwd); ok {
		return cwd, nil
	}
	return "", fmt.Errorf(
		"%s is not shared with the sandbox VM.\nShared directories:\n  %s\nAdd it with:  code-vm mount %s",
		cwd, strings.Join(mounts, "\n  "), cwd)
}

// runDefault is the root command's action: bring the VM up, verify the current
// directory is shared, then run the command as the agent user at that path.
func runDefault(ctx context.Context, args []string) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	workdir, err := resolveWorkdir(c, cwd)
	if err != nil {
		return err
	}
	cl := newClient()
	if err := ensureRunning(ctx, cl, c); err != nil {
		return err
	}
	return cl.Agent(ctx, workdir, agentCommand(args))
}
```

- [ ] **Step 4: Wire the default action in `internal/cli/root.go`**

Add to the root command literal, after `SilenceErrors: true`:

```go
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDefault(cmd.Context(), args)
		},
```

Cobra treats everything after `--` as positional args, so `code-vm -- claude -p "..."` arrives in `args` without flag parsing. Verify in Step 7 that `code-vm -- claude --help` passes `--help` through to Claude rather than printing code-vm's help.

- [ ] **Step 5: Implement the real `sandbox-exec`**

Replace `internal/guest/files/scripts/sandbox-exec`:

```bash
#!/bin/bash
###############################################################################
# sandbox-exec — drop from root to the agent user, preserving cwd
#
# Invoked as `sudo /usr/local/bin/sandbox-exec <cmd...>` by code-vm. Sources
# /etc/environment because `limactl shell` starts a non-login shell that would
# never read it, then hands off to the agent user.
#
# Note: setpriv is deliberately used WITHOUT --reset-env, so the proxy and
# DOCKER_HOST values sourced below survive into the agent's process.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

set -a
# shellcheck source=/dev/null
[ -f /etc/environment ] && . /etc/environment
set +a

cmd=("$@")
if [ ${#cmd[@]} -eq 0 ]; then
    cmd=(bash -l)
fi

# Already unprivileged (e.g. invoked directly by the agent): just exec.
if [ "$(id -u)" -ne 0 ]; then
    exec "${cmd[@]}"
fi

exec setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
    env \
    HOME="/home/${AGENT_USER}" \
    USER="$AGENT_USER" \
    LOGNAME="$AGENT_USER" \
    XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
    PATH="/home/${AGENT_USER}/.local/bin:/home/${AGENT_USER}/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
    "${cmd[@]}"
```

- [ ] **Step 6: Add VM assertions for the exec path**

In `test-vm-sandbox.sh`, replace the `agent()` helper so it exercises the real CLI, and add a section. Keep the `limactl`-based `adm()` helper as is — admin assertions must not depend on the CLI.

```bash
# Run a command as the agent user through the real CLI.
agent() { "$CODE_VM" -- "$@"; }
```

Add after the Docker section:

```bash
echo ""
echo "── Exec path ─────────────────────────────────────────────────────"

if [ "$(agent id -u)" = "$(id -u)" ]; then
    pass "code-vm runs as the agent user with the host UID"
else
    fail "code-vm runs as the agent user with the host UID"
fi

if [ "$(agent id -un)" = "$AGENT_USER" ]; then
    pass "code-vm runs as $AGENT_USER"
else
    fail "code-vm runs as $AGENT_USER (got $(agent id -un))"
fi

WORK_SUBDIR="$PROJECTS_ROOT/.code-vm-test-cwd"
mkdir -p "$WORK_SUBDIR"
if [ "$(cd "$WORK_SUBDIR" && agent pwd)" = "$WORK_SUBDIR" ]; then
    pass "working directory is preserved into the guest"
else
    fail "working directory is preserved into the guest"
fi
rmdir "$WORK_SUBDIR"

if agent env | grep -q '^DOCKER_HOST=unix:///run/user/'; then
    pass "DOCKER_HOST is exported to the agent"
else
    fail "DOCKER_HOST is exported to the agent"
fi

if agent env | grep -q '^https_proxy=http://localhost:3128$'; then
    pass "proxy env is exported to the agent"
else
    fail "proxy env is exported to the agent"
fi

if (cd /tmp && "$CODE_VM" -- true) 2>&1 | grep -q "code-vm mount"; then
    pass "running outside a shared directory fails with actionable advice"
else
    fail "running outside a shared directory fails with actionable advice"
fi
```

- [ ] **Step 7: Run the tests**

Run: `mise run test:unit && mise run lint && mise run fmt-check`
Expected: PASS.

Run: `mise run test:vm`
Expected: all assertions PASS.

Then check argument pass-through manually:

```bash
cd "$(sed -n 's/^projectsRoot: *//p' ~/.config/code-vm/config.yaml)" 2>/dev/null || cd ~/projects
./dist/code-vm -- bash -c 'echo "$@"' _ --help
```
Expected: prints `--help`, proving code-vm did not intercept it.

- [ ] **Step 8: Commit**

```bash
cd /workspace/vm-sandbox
git add internal/guest internal/cli test-vm-sandbox.sh
git commit -m "feat: run agent commands in the guest at the current path

sandbox-exec sources /etc/environment and drops root to the agent user
with setpriv, deliberately without --reset-env so the proxy and
DOCKER_HOST values survive. Running outside a shared directory fails
with the exact code-vm mount command to fix it."
```

---

### Task 6: Firewall, settings lock, and the boot sequence

**Files:**
- Create: `internal/guest/files/scripts/init-firewall.sh`, `internal/guest/files/scripts/lock-settings.sh`, `internal/guest/files/scripts/update-agent-clis.sh`
- Modify: `internal/guest/files/scripts/sandbox-boot.sh` (replace the Task 4 placeholder)
- Modify: `internal/guest/files/config/.claude/settings.json` (replace the Task 2 placeholder)
- Test: `test-vm-sandbox.sh`

**Interfaces:**
- Consumes: `/etc/sandbox/provision.env` (`AGENT_USER`, `AGENT_UID`, `EXTRA_ALLOWED_DOMAINS`).
- Produces:
  - `/run/firewall-verify` — `KEY=value` lines consumed by the readiness probe and the test suite: `OUTPUT_POLICY`, `UDP_DROP`, `PROXY_UID_RULE`, `AGENT_GATEWAY_REJECT`, `SQUID_RUNNING`, `FRAGMENT_DIR`
  - `/run/sandbox/squid-allow.d/` — tmpfs fragment directory with `00-base.conf`, consumed by Task 7
  - locked `/home/devuser/.claude/settings.json` and `settings.local.json`, owner `root:devuser`, mode `0444`

- [ ] **Step 1: Build the base settings profile from the container sandbox**

The container sandbox keeps its Docker-mode permissions in a separate overrides file because it has two modes. The VM always has Docker, so there is one merged profile.

```bash
cd /workspace/vm-sandbox
jq -s '
  .[0] as $b | .[1] as $o |
  $b
  | .permissions.allow = (($b.permissions.allow // []) + ($o.permissions_allow_add // []) | unique)
  | .permissions.deny  = (($b.permissions.deny  // []) - ($o.permissions_deny_remove // []))
  | ._profile = "vm-sandbox"
  | ._description = "code-vm: permissive profile; Docker always available; egress firewall is the primary defense"
' /workspace/config/.claude/settings.json \
  /workspace/config-dind/.claude/settings.overrides.json \
  > internal/guest/files/config/.claude/settings.json

jq -e '.permissions.allow | index("Bash(docker *)")' internal/guest/files/config/.claude/settings.json
jq -e '.permissions.deny | map(test("docker")) | any | not' internal/guest/files/config/.claude/settings.json
```

Both `jq -e` checks must exit 0: Docker is allowed and no Docker deny rule survives. If the overrides file uses different key names than `permissions_allow_add`/`permissions_deny_remove`, read `/workspace/lock-settings.sh` lines 84-91 for the authoritative merge expression and adapt.

- [ ] **Step 2: Write `lock-settings.sh`**

Create `internal/guest/files/scripts/lock-settings.sh`. This is the container sandbox's script with the DinD overlay branch removed and the agent user parameterised.

```bash
#!/bin/bash
###############################################################################
# lock-settings.sh — restore and lock the canonical Claude config
#
# Runs as root from sandbox-boot.sh, before the agent can start. Copies the
# canonical tree delivered by code-vm into the agent's home and makes every
# file root-owned and read-only, so the agent cannot rewrite its own
# permission rules.
#
# /usr/local/share/sandbox-config mirrors the agent's home:
#   .claude/settings.json -> /home/devuser/.claude/settings.json
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

CONFIG_SRC=/usr/local/share/sandbox-config
CONFIG_DST="/home/${AGENT_USER}"
CLAUDE_DIR="$CONFIG_DST/.claude"
SETTINGS="$CLAUDE_DIR/settings.json"
SETTINGS_LOCAL="$CLAUDE_DIR/settings.local.json"
CRED_DENY=/run/sandbox-secrets/deny-rules.json

if [ ! -d "$CONFIG_SRC" ]; then
    echo "[lock-settings] ERROR: canonical config missing at $CONFIG_SRC"
    exit 1
fi

apply_tree() {
    local src="$1" src_file rel dst
    while IFS= read -r src_file; do
        rel="${src_file#"$src"/}"
        dst="$CONFIG_DST/$rel"
        install -d "$(dirname "$dst")"
        cp "$src_file" "$dst"
        chown "root:${AGENT_USER}" "$dst"
        chmod 0444 "$dst"
        echo "[lock-settings]   Locked: $rel"
    done < <(find "$src" -type f)
}

# Claude Code records plugin enablement in settings.json under enabledPlugins.
# The copy below would silently disable every plugin the user installed, even
# though the plugin files persist on the guest disk. Capture and re-merge it.
PREV_ENABLED_PLUGINS='{}'
if [ -f "$SETTINGS" ]; then
    PREV_ENABLED_PLUGINS="$(jq -c '.enabledPlugins // {}' "$SETTINGS" 2> /dev/null || echo '{}')"
fi

install -d -o "$AGENT_USER" -g "$AGENT_USER" "$CLAUDE_DIR"
apply_tree "$CONFIG_SRC"

merge_into_settings() {
    # $1: jq program, remaining args passed to jq
    local prog="$1"
    shift
    chmod 0644 "$SETTINGS"
    jq "$@" "$prog" "$SETTINGS" > "${SETTINGS}.tmp"
    mv "${SETTINGS}.tmp" "$SETTINGS"
    chown "root:${AGENT_USER}" "$SETTINGS"
    chmod 0444 "$SETTINGS"
}

if [ "$PREV_ENABLED_PLUGINS" != "{}" ] && [ "$PREV_ENABLED_PLUGINS" != "null" ]; then
    merge_into_settings '.enabledPlugins = ($ep + (.enabledPlugins // {}))' \
        --argjson ep "$PREV_ENABLED_PLUGINS"
    echo "[lock-settings] Preserved enabledPlugins across restart"
fi

# Claim settings.local.json: Claude Code treats it as an override file, so an
# unclaimed path is a permission-bypass vector.
echo '{}' > "$SETTINGS_LOCAL"
chown "root:${AGENT_USER}" "$SETTINGS_LOCAL"
chmod 0444 "$SETTINGS_LOCAL"

# Credential deny rules, when a session has injected credentials. Written by
# code-vm before this script runs; see render-credentials.sh.
if [ -f "$CRED_DENY" ]; then
    merge_into_settings '.permissions.deny += $extra[0]' --slurpfile extra "$CRED_DENY"
    echo "[lock-settings] Merged $(jq 'length' "$CRED_DENY") credential deny rules"
fi

echo "[lock-settings] Config restored from canonical and locked"
```

- [ ] **Step 3: Write `init-firewall.sh`**

Create `internal/guest/files/scripts/init-firewall.sh`. This is the container sandbox's script with four changes: registry domains are unconditional, the allowlist gains a tmpfs fragment include, Squid runs under systemd, and two agent-specific iptables rules are added.

```bash
#!/bin/bash
###############################################################################
# init-firewall.sh — Squid allowlist + iptables default-deny egress firewall
#
#   agent → http_proxy=localhost:3128 → Squid (domain ACL) → internet
#   iptables: default-deny OUTPUT; only root and Squid exit directly
#
# Runs as root from sandbox-boot.sh, last in the sequence: closing egress
# before the CLI updates would break them.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

SQUID_CONF=/etc/squid/squid.conf
FRAGMENT_DIR=/run/sandbox/squid-allow.d
VERIFY_FILE=/run/firewall-verify

echo "[firewall] Initializing egress firewall..."

if ! iptables -L OUTPUT -n > /dev/null 2>&1; then
    echo "[firewall] ERROR: iptables is not functional; the VM would have NO egress restrictions."
    exit 1
fi

# ── Domain allowlist ────────────────────────────────────────────────────────
# Container registries are unconditional: this sandbox always has Docker.
DEFAULT_DOMAINS=(
    .anthropic.com .claude.ai .platform.claude.com .code.claude.com .docs.claude.com
    .opencode.ai .models.dev .opncd.ai
    .github.com .githubusercontent.com .githubassets.com
    .pypi.org .pythonhosted.org
    .npmjs.org .npmjs.com .nodejs.org
    .crates.io .rust-lang.org
    proxy.golang.org sum.golang.org pkg.go.dev
    .google.com .bing.com .duckduckgo.com .wikipedia.org
    .stackoverflow.com .readthedocs.io .docs.rs .developer.mozilla.org
    .cloudflare.com .fastly.net
    .json-schema.org .schemastore.org
    .mise.jdx.dev .mise-versions.jdx.dev .mise-java.jdx.dev .mise.run .fnox.jdx.dev
    .dl.k8s.io .releases.hashicorp.com .get.helm.sh
    .opentofu.org .registry.opentofu.org
    .services.gradle.org .plugins.gradle.org .plugins-artifacts.gradle.org
    .repo1.maven.org .repo.maven.apache.org
    .dl-cdn.alpinelinux.org .awscli.amazonaws.com
    # Container registries
    .docker.io .docker.com .hub.docker.com
    .production.cloudflare.docker.com .r2.cloudflarestorage.com
    .ghcr.io .gcr.io .quay.io .registry.k8s.io
)

DOMAIN_LIST=("${DEFAULT_DOMAINS[@]}")
if [ -n "${EXTRA_ALLOWED_DOMAINS:-}" ]; then
    read -ra EXTRA <<< "$EXTRA_ALLOWED_DOMAINS"
    [ ${#EXTRA[@]} -gt 0 ] && DOMAIN_LIST+=("${EXTRA[@]}")
fi

# ── Fragment directory ──────────────────────────────────────────────────────
# Per-workspace allowlists live here, written by code-vm session setup. It is
# tmpfs-backed, so stale entries from projects no longer in use cannot widen
# the allowlist beyond one VM lifetime. 00-base.conf is always present so the
# wildcard include never matches an empty set.
mount | grep -q " /run/sandbox " || {
    install -d /run/sandbox
    mount -t tmpfs -o mode=0755,nosuid,nodev tmpfs /run/sandbox
}
install -d -m 0755 "$FRAGMENT_DIR"
printf '# base fragment; per-workspace fragments are added by code-vm\n' > "$FRAGMENT_DIR/00-base.conf"
chmod 0444 "$FRAGMENT_DIR/00-base.conf"

# ── squid.conf ──────────────────────────────────────────────────────────────
# Order matters: Squid reads linearly, so every `acl allowed_domains` line —
# including the fragments — must precede the http_access rules.
{
    echo "http_port 3128"
    echo ""
    echo "# Security proxy, not a caching proxy"
    echo "cache_dir null /tmp"
    echo "cache deny all"
    echo ""
    echo "access_log /var/log/squid/access.log squid"
    echo ""
    echo "# Domain allowlist — .domain matches the domain and all subdomains"
    for domain in "${DOMAIN_LIST[@]}"; do
        echo "acl allowed_domains dstdomain $domain"
    done
    echo ""
    echo "# Per-workspace fragments (tmpfs; cleared on every boot)"
    echo "include $FRAGMENT_DIR/*.conf"
    echo ""
    echo "acl CONNECT method CONNECT"
    echo "http_access allow CONNECT allowed_domains"
    echo "http_access allow allowed_domains"
    echo "http_access deny all"
} > "$SQUID_CONF"

echo "[firewall] Generated $SQUID_CONF (${#DOMAIN_LIST[@]} base entries)"

# ── Start Squid ─────────────────────────────────────────────────────────────
# World-readable log dir so proxy-log works without granting write access.
chmod o+rx /var/log/squid/
install -m 0644 -o proxy -g proxy /dev/null /var/log/squid/access.log
systemctl enable squid.service > /dev/null 2>&1 || true
systemctl restart squid.service

READY=false
for _ in $(seq 1 20); do
    if (echo > /dev/tcp/localhost/3128) 2> /dev/null; then
        READY=true
        break
    fi
    sleep 0.5
done
if [ "$READY" != "true" ]; then
    echo "[firewall] ERROR: Squid did not start within 10 seconds."
    exit 1
fi
echo "[firewall] Squid ready on :3128"

# ── iptables ────────────────────────────────────────────────────────────────
GUEST_IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)
GATEWAY=$(ip route show default | awk '{print $3; exit}')

iptables -F OUTPUT
iptables -F INPUT
iptables -F FORWARD
iptables -P INPUT ACCEPT
iptables -P OUTPUT DROP
iptables -P FORWARD DROP

iptables -A OUTPUT -o lo -j ACCEPT
iptables -A INPUT -i lo -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

# DNS first: Lima's host resolver may live on the gateway, which the agent
# rule below rejects. First match wins, so these must be appended earlier.
DNS_SERVERS=$(grep -oP '^\s*nameserver\s+\K\S+' /etc/resolv.conf || true)
for dns in $DNS_SERVERS; do
    iptables -A OUTPUT -d "$dns" -p udp --dport 53 -j ACCEPT
    iptables -A OUTPUT -d "$dns" -p tcp --dport 53 -j ACCEPT
    echo "[firewall]   Allowed DNS: $dns"
done

# Block DNS tunneling to any other resolver.
iptables -A OUTPUT -p udp -j DROP

# Rootless Docker NATs container traffic out as the agent UID, so containers
# reach Squid at the guest's own address. This is the only non-loopback proxy
# path the agent needs.
iptables -A OUTPUT -m owner --uid-owner "$AGENT_UID" -d "$GUEST_IP" -p tcp --dport 3128 -j ACCEPT

# The agent has no business reaching host services: Squid runs in the guest.
if [ -n "$GATEWAY" ]; then
    iptables -A OUTPUT -m owner --uid-owner "$AGENT_UID" -d "$GATEWAY" -j REJECT
    echo "[firewall]   Rejected: agent -> host gateway $GATEWAY"
fi

# Anthropic API CIDR — direct, and a fallback if Squid is unavailable.
iptables -A OUTPUT -d 160.79.104.0/23 -p tcp --dport 443 -j ACCEPT

# Root (boot sequence, provisioning) and Squid's own workers exit directly.
iptables -A OUTPUT -m owner --uid-owner 0 -j ACCEPT
iptables -A OUTPUT -m owner --uid-owner proxy -j ACCEPT

iptables -A OUTPUT -m limit --limit 5/min -j LOG --log-prefix "[FIREWALL-BLOCKED] " --log-level 4
iptables -A OUTPUT -j REJECT --reject-with icmp-port-unreachable

# ── Self-verify ─────────────────────────────────────────────────────────────
# The Lima readiness probe waits for this file, so `limactl start` cannot
# return before the firewall is up.
VERIFY_OK=true
OUTPUT_POLICY=$(iptables -L OUTPUT -n | head -1 | grep -o "DROP" || echo "NOT_DROP")
[ "$OUTPUT_POLICY" = "DROP" ] || VERIFY_OK=false

udp_drop=no
iptables -L OUTPUT -n | grep -qE "DROP[[:space:]]+17|DROP.*udp" && udp_drop=yes
[ "$udp_drop" = yes ] || VERIFY_OK=false

PROXY_UID=$(id -u proxy 2> /dev/null || echo 13)
proxy_rule=no
iptables -L OUTPUT -n -v | grep -q "owner UID match $PROXY_UID" && proxy_rule=yes
[ "$proxy_rule" = yes ] || VERIFY_OK=false

gw_reject=no
if [ -n "$GATEWAY" ]; then
    iptables -L OUTPUT -n -v | grep -q "owner UID match $AGENT_UID" && gw_reject=yes
    [ "$gw_reject" = yes ] || VERIFY_OK=false
fi

squid_running=no
(echo > /dev/tcp/localhost/3128) 2> /dev/null && squid_running=yes

{
    echo "OUTPUT_POLICY=$OUTPUT_POLICY"
    echo "UDP_DROP=$udp_drop"
    echo "PROXY_UID_RULE=$proxy_rule"
    echo "AGENT_GATEWAY_REJECT=$gw_reject"
    echo "SQUID_RUNNING=$squid_running"
    echo "FRAGMENT_DIR=$FRAGMENT_DIR"
} > "$VERIFY_FILE"
chmod 0444 "$VERIFY_FILE"

if [ "$VERIFY_OK" != true ]; then
    echo "[firewall] ERROR: verification failed; rules are incorrect."
    exit 1
fi
echo "[firewall] Active. DEFAULT DENY + Squid allowlist on :3128"
```

- [ ] **Step 4: Write `update-agent-clis.sh` and the real `sandbox-boot.sh`**

Create `internal/guest/files/scripts/update-agent-clis.sh`:

```bash
#!/bin/bash
###############################################################################
# update-agent-clis.sh — install or update Claude Code and OpenCode
#
# Runs as root from sandbox-boot.sh BEFORE the firewall closes, because the
# installers fetch from the network. Failures are warnings, not errors: an
# offline boot must not brick the VM.
###############################################################################
set -uo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

run_as_agent() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH="/home/${AGENT_USER}/.local/bin:/usr/local/bin:/usr/bin:/bin" \
        bash -lc "$1"
}

echo "[boot] Updating agent CLIs"
run_as_agent 'curl -fsSL https://claude.ai/install.sh | bash' \
    || echo "[boot] WARNING: Claude Code install/update failed"
run_as_agent 'curl -fsSL https://opencode.ai/install | bash' \
    || echo "[boot] WARNING: OpenCode install/update failed"
```

Verify both installer URLs during implementation (`curl -fsSLI <url>`); if either has moved, use the current documented one-liner and keep the surrounding structure.

Replace `internal/guest/files/scripts/sandbox-boot.sh`:

```bash
#!/bin/bash
###############################################################################
# sandbox-boot.sh — the VM's equivalent of the container's entrypoint.sh
#
# Ordered sequence, run as root once provisioning has finished:
#   1. update the agent CLIs   — needs unrestricted egress
#   2. lock the Claude config  — must precede any agent process
#   3. initialise the firewall — closes egress, so it goes last
#
# The order is load-bearing. It is the same order entrypoint.sh uses in the
# container sandbox, for the same reason.
###############################################################################
set -euo pipefail

echo "[boot] Sandbox boot sequence starting"

/usr/local/lib/sandbox/update-agent-clis.sh
/usr/local/lib/sandbox/lock-settings.sh
/usr/local/lib/sandbox/init-firewall.sh

echo "[boot] Sandbox boot sequence complete"
```

- [ ] **Step 5: Add VM assertions**

Append to `test-vm-sandbox.sh`:

```bash
echo ""
echo "── Firewall ──────────────────────────────────────────────────────"

VERIFY=$(adm cat /run/firewall-verify)
for kv in "OUTPUT_POLICY=DROP" "UDP_DROP=yes" "PROXY_UID_RULE=yes" \
          "AGENT_GATEWAY_REJECT=yes" "SQUID_RUNNING=yes"; do
    if echo "$VERIFY" | grep -qx "$kv"; then
        pass "firewall self-verify: $kv"
    else
        fail "firewall self-verify: $kv (got: $(echo "$VERIFY" | tr '\n' ' '))"
    fi
done

assert_ok "allowlisted domain reachable through the proxy" \
    agent curl -fsS -o /dev/null --max-time 20 https://api.anthropic.com
assert_fails "non-allowlisted domain blocked" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "direct egress bypassing the proxy is blocked" \
    agent env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
        curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "DNS tunneling to an external resolver is blocked" \
    agent timeout 10 nslookup example.org 1.1.1.1

if [ "$(adm sh -c 'ls /run/sandbox/squid-allow.d | tr "\n" " "')" = "00-base.conf " ]; then
    pass "allowlist fragment dir holds only the base fragment after boot"
else
    fail "allowlist fragment dir holds only the base fragment after boot"
fi

echo ""
echo "── Settings lock ─────────────────────────────────────────────────"

SETTINGS="/home/$AGENT_USER/.claude/settings.json"
if [ "$(adm stat -c '%U:%G %a' "$SETTINGS")" = "root:$AGENT_USER 444" ]; then
    pass "settings.json is root-owned and read-only"
else
    fail "settings.json is root-owned and read-only (got $(adm stat -c '%U:%G %a' "$SETTINGS"))"
fi

assert_fails "agent cannot write settings.json" \
    agent bash -c "echo '{}' > $SETTINGS"
assert_fails "agent cannot write settings.local.json" \
    agent bash -c "echo '{}' > /home/$AGENT_USER/.claude/settings.local.json"
assert_fails "agent cannot write /etc" \
    agent bash -c "echo x > /etc/code-vm-probe"

echo ""
echo "── Docker networking ─────────────────────────────────────────────"

assert_ok "docker build works" \
    agent bash -c 'printf "FROM alpine:3.23\nRUN echo ok\n" | docker build -q -t code-vm-test:latest -'
assert_fails "privileged containers are refused" \
    agent docker run --rm --privileged alpine:3.23 true

COMPOSE_DIR="$PROJECTS_ROOT/.code-vm-compose-test"
mkdir -p "$COMPOSE_DIR"
cat > "$COMPOSE_DIR/compose.yaml" <<'YAML'
services:
  server:
    image: alpine:3.23
    command: ["sleep", "60"]
  client:
    image: alpine:3.23
    command: ["sleep", "60"]
YAML
(cd "$COMPOSE_DIR" && agent docker compose up -d > /dev/null 2>&1)
if (cd "$COMPOSE_DIR" && agent docker compose exec -T client getent hosts server > /dev/null 2>&1); then
    pass "compose service-name DNS resolves"
else
    fail "compose service-name DNS resolves"
fi
(cd "$COMPOSE_DIR" && agent docker compose down -v > /dev/null 2>&1)
rm -rf "$COMPOSE_DIR"

echo ""
echo "── Resource limits ───────────────────────────────────────────────"

if adm systemctl show "user-$(id -u).slice" -p TasksMax | grep -q "TasksMax=2048"; then
    pass "TasksMax is applied to the agent slice"
else
    fail "TasksMax is applied to the agent slice"
fi
```

- [ ] **Step 6: Recreate the VM and run the suite**

Provisioning changed, and `mode: data` files refresh on start, but the boot sequence must be exercised from a clean guest:

```bash
cd /workspace/vm-sandbox
limactl delete --force code-sandbox
mise run lint && mise run fmt-check && mise run test:unit
mise run test:vm
```

Expected: all assertions PASS. If the readiness probe times out, the firewall failed its self-verify — read `limactl shell code-sandbox sudo journalctl -u sandbox-boot.service --no-pager` and fix the reported rule before proceeding. Do not weaken the verify checks to make the probe pass.

If `compose service-name DNS resolves` fails, confirm rootless dockerd is using its own network namespace (`agent docker network inspect bridge`) rather than host networking; that is the whole reason this design replaces Podman.

- [ ] **Step 7: Commit**

```bash
cd /workspace/vm-sandbox
git add internal/guest test-vm-sandbox.sh
git commit -m "feat: port the egress firewall, settings lock and boot sequence

sandbox-boot.service runs after cloud-final so provisioning keeps
unrestricted egress and the firewall closes last. Squid's allowlist
gains a tmpfs fragment include, so per-workspace domains cannot widen
the allowlist beyond one VM lifetime. DNS accepts are appended before
the agent-to-gateway reject because Lima's host resolver can live on
the gateway and first match wins."
```

---

### Task 7: Session setup — allowlist fragments and git identity

**Files:**
- Create: `internal/session/allowlist.go`, `internal/session/gitidentity.go`, `internal/session/session.go`
- Modify: `internal/cli/shell.go` (call `session.Setup` before exec)
- Test: `internal/session/allowlist_test.go`, `internal/session/gitidentity_test.go`, `test-vm-sandbox.sh`

**Interfaces:**
- Consumes: `lima.Client` (`Copy`, `Admin`, `AdminOutput`), `config.Config`.
- Produces:
  - `session.Deps` struct: `Client lima.Client`, `Config config.Config`, `Workspace string`, `AgentUser string`, `Host session.HostRunner`
  - `session.HostRunner` type: `func(ctx context.Context, name string, args ...string) ([]byte, error)`
  - `session.Setup(ctx context.Context, d Deps) error`
  - `session.ReadDomains(workspace string) ([]string, error)`
  - `session.FragmentName(workspace string) string`
  - `session.FragmentContent(workspace string, domains []string) string`
  - `session.ApplyAllowlist(ctx context.Context, d Deps) error`
  - `session.GitConfigContent(name, email string) string`
  - `session.ApplyGitIdentity(ctx context.Context, d Deps) error`

- [ ] **Step 1: Write the failing tests**

`internal/session/allowlist_test.go`:

```go
package session

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

func TestReadDomains(t *testing.T) {
	dir := t.TempDir()
	body := "# a comment\n\nregistry.example.com\n  .internal.example  \nregistry.example.com\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadDomains(dir)
	if err != nil {
		t.Fatalf("ReadDomains: %v", err)
	}
	want := []string{"registry.example.com", ".internal.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDomains = %v, want %v (comments and blanks dropped, trimmed, de-duplicated in first-seen order)", got, want)
	}
}

func TestReadDomainsMissingFileIsEmpty(t *testing.T) {
	got, err := ReadDomains(t.TempDir())
	if err != nil {
		t.Fatalf("ReadDomains: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadDomains = %v, want empty", got)
	}
}

func TestFragmentNameIsStableAndWorkspaceSpecific(t *testing.T) {
	a := FragmentName("/home/st/projects/one")
	b := FragmentName("/home/st/projects/two")
	if a != FragmentName("/home/st/projects/one") {
		t.Error("FragmentName must be stable for the same workspace")
	}
	if a == b {
		t.Error("different workspaces must get different fragments")
	}
	if !strings.HasPrefix(a, "10-") || !strings.HasSuffix(a, ".conf") {
		t.Errorf("FragmentName = %q, want 10-<hash>.conf", a)
	}
}

func TestFragmentContentEmitsOneACLPerDomain(t *testing.T) {
	got := FragmentContent("/home/st/projects/one", []string{"a.example", ".b.example"})
	if !strings.Contains(got, "acl allowed_domains dstdomain a.example\n") {
		t.Error("missing ACL line for a.example")
	}
	if !strings.Contains(got, "acl allowed_domains dstdomain .b.example\n") {
		t.Error("missing ACL line for .b.example")
	}
	if !strings.Contains(got, "/home/st/projects/one") {
		t.Error("fragment should record which workspace it came from")
	}
}

type fakeRunner struct {
	calls [][]string
	out   map[string][]byte
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out[strings.Join(args, " ")], nil
}

func (f *fakeRunner) ranAny(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func testDeps(t *testing.T, r lima.Runner, ws string) Deps {
	t.Helper()
	c := config.Default()
	c.ProjectsRoot = filepath.Dir(ws)
	return Deps{Client: lima.Client{R: r}, Config: c, Workspace: ws, AgentUser: "devuser"}
}

func TestApplyAllowlistInstallsFragmentAndReloadsSquid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte("registry.example.com\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := &fakeRunner{}
	if err := ApplyAllowlist(context.Background(), testDeps(t, r, dir)); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if !r.ranAny("copy") {
		t.Error("fragment must be copied into the guest")
	}
	if !r.ranAny("install") {
		t.Error("fragment must be installed into the allowlist directory as root")
	}
	if !r.ranAny("squid -k reconfigure") {
		t.Error("Squid must be reloaded after the allowlist changes")
	}
}

func TestApplyAllowlistSkipsReloadWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte("registry.example.com\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d := testDeps(t, nil, dir)
	existing := FragmentContent(dir, []string{"registry.example.com"})
	r := &fakeRunner{out: map[string][]byte{
		strings.Join(lima.Client{}.AdminArgs([]string{"cat", "/run/sandbox/squid-allow.d/" + FragmentName(dir)}), " "): []byte(existing),
	}}
	d.Client = lima.Client{R: r}
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if r.ranAny("squid -k reconfigure") {
		t.Error("Squid must not be reloaded when the fragment is unchanged")
	}
}

func TestApplyAllowlistNoDomainsIsNoOp(t *testing.T) {
	r := &fakeRunner{}
	if err := ApplyAllowlist(context.Background(), testDeps(t, r, t.TempDir())); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no guest calls without a .sandbox-domains file, got %v", r.calls)
	}
}
```

`internal/session/gitidentity_test.go`:

```go
package session

import (
	"context"
	"strings"
	"testing"
)

func TestGitConfigContent(t *testing.T) {
	got := GitConfigContent("Ada Lovelace", "ada@example.com")
	for _, want := range []string{"[user]", "name = Ada Lovelace", "email = ada@example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("GitConfigContent missing %q, got:\n%s", want, got)
		}
	}
}

func TestGitConfigContentOmitsMissingFields(t *testing.T) {
	got := GitConfigContent("", "ada@example.com")
	if strings.Contains(got, "name =") {
		t.Errorf("empty name must be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "email = ada@example.com") {
		t.Error("email should still be written")
	}
}

func TestApplyGitIdentitySkipsWhenHostHasNoIdentity(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r, t.TempDir())
	d.Host = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, context.Canceled // simulate `git config --get` exiting non-zero
	}
	if err := ApplyGitIdentity(context.Background(), d); err != nil {
		t.Fatalf("ApplyGitIdentity: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no guest calls when the host has no git identity, got %v", r.calls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `internal/session` has no implementation.

- [ ] **Step 3: Implement `internal/session/allowlist.go`**

```go
// Package session performs the privileged per-invocation setup that must
// happen before the agent runs: allowlist fragments, git identity and
// credential injection.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

// fragmentDir mirrors init-firewall.sh. It is tmpfs-backed, so fragments do
// not survive a VM restart and cannot widen the allowlist indefinitely.
const fragmentDir = "/run/sandbox/squid-allow.d"

// HostRunner executes a command on the host. Injectable for tests.
type HostRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Deps carries everything session setup needs.
type Deps struct {
	Client    lima.Client
	Config    config.Config
	Workspace string
	AgentUser string
	Host      HostRunner
}

// ReadDomains parses the workspace's .sandbox-domains file. Comments and blank
// lines are dropped, entries trimmed, duplicates removed in first-seen order.
// A missing file yields no domains and no error.
func ReadDomains(workspace string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(workspace, ".sandbox-domains"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .sandbox-domains: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		d := strings.TrimSpace(line)
		if d == "" || strings.HasPrefix(d, "#") || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out, nil
}

// FragmentName returns the per-workspace Squid fragment filename. The 10-
// prefix orders it after the base fragment written at boot.
func FragmentName(workspace string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(workspace)))
	return "10-" + hex.EncodeToString(sum[:])[:12] + ".conf"
}

// FragmentContent renders the Squid ACL lines for a workspace.
func FragmentContent(workspace string, domains []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# code-vm allowlist fragment for %s\n", filepath.Clean(workspace))
	for _, d := range domains {
		fmt.Fprintf(&b, "acl allowed_domains dstdomain %s\n", d)
	}
	return b.String()
}

// ApplyAllowlist installs the workspace's fragment and reloads Squid when the
// content changed. Reloading unconditionally would drop in-flight connections
// on every invocation.
func ApplyAllowlist(ctx context.Context, d Deps) error {
	domains, err := ReadDomains(d.Workspace)
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		return nil
	}
	name := FragmentName(d.Workspace)
	dst := fragmentDir + "/" + name
	want := FragmentContent(d.Workspace, domains)

	// A read failure means the fragment is absent, which is a change.
	current, _ := d.Client.AdminOutput(ctx, []string{"cat", dst})
	if string(current) == want {
		return nil
	}

	tmp, err := os.CreateTemp("", "code-vm-allow-*.conf")
	if err != nil {
		return fmt.Errorf("create temp fragment: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(want); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp fragment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp fragment: %w", err)
	}

	staged := "/tmp/" + name
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"install", "-m", "0444", "-o", "root", "-g", "root", staged, dst}); err != nil {
		return err
	}
	if err := d.Client.Admin(ctx, []string{"rm", "-f", staged}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"squid", "-k", "reconfigure"})
}
```

- [ ] **Step 4: Implement `internal/session/gitidentity.go`**

```go
package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ExecHost runs a command on the host and returns its stdout.
func ExecHost(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// GitConfigContent renders a minimal gitconfig. Empty fields are omitted so
// git falls back to its own resolution rather than seeing a blank identity.
func GitConfigContent(name, email string) string {
	var b strings.Builder
	b.WriteString("# Written by code-vm from the host's git config.\n[user]\n")
	if name != "" {
		fmt.Fprintf(&b, "\tname = %s\n", name)
	}
	if email != "" {
		fmt.Fprintf(&b, "\temail = %s\n", email)
	}
	return b.String()
}

// ApplyGitIdentity copies the host's git identity into the guest so commits
// made by the agent are attributed correctly. A host with no identity
// configured is not an error.
func ApplyGitIdentity(ctx context.Context, d Deps) error {
	host := d.Host
	if host == nil {
		host = ExecHost
	}
	get := func(key string) string {
		out, err := host(ctx, "git", "config", "--get", key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	name, email := get("user.name"), get("user.email")
	if name == "" && email == "" {
		return nil
	}

	tmp, err := os.CreateTemp("", "code-vm-gitconfig-*")
	if err != nil {
		return fmt.Errorf("create temp gitconfig: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(GitConfigContent(name, email)); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp gitconfig: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp gitconfig: %w", err)
	}

	staged := "/tmp/code-vm-gitconfig"
	if err := d.Client.Copy(ctx, tmp.Name(), staged); err != nil {
		return err
	}
	dst := "/home/" + d.AgentUser + "/.gitconfig"
	owner := d.AgentUser + ":" + d.AgentUser
	if err := d.Client.Admin(ctx, []string{"install", "-m", "0644", "-o", owner, staged, dst}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"rm", "-f", staged})
}
```

Note: `install -o user:group` is not valid; `install` takes `-o owner -g group`. Use `[]string{"install", "-m", "0644", "-o", d.AgentUser, "-g", d.AgentUser, staged, dst}` and drop the `owner` variable.

- [ ] **Step 5: Implement `internal/session/session.go`**

```go
package session

import "context"

// Setup performs every privileged per-invocation step, in order. Credential
// rendering is added in Task 8 and must run after ApplyAllowlist, because
// lock-settings.sh consumes the credential deny rules.
func Setup(ctx context.Context, d Deps) error {
	if err := ApplyAllowlist(ctx, d); err != nil {
		return err
	}
	return ApplyGitIdentity(ctx, d)
}
```

- [ ] **Step 6: Call `session.Setup` from the exec path**

In `internal/cli/shell.go`, inside `runDefault`, after `ensureRunning` succeeds and before `cl.Agent(...)`:

```go
	if err := session.Setup(ctx, session.Deps{
		Client:    cl,
		Config:    c,
		Workspace: workdir,
		AgentUser: agentUser,
	}); err != nil {
		return fmt.Errorf("session setup: %w", err)
	}
```

Add `"github.com/wetransform/code-vm/internal/session"` to the imports.

Note that `Workspace` is the current directory, not the covering mount: `.sandbox-domains` is a per-project file and lives in the project root the user invoked `code-vm` from.

- [ ] **Step 7: Add VM assertions**

Append to `test-vm-sandbox.sh`:

```bash
echo ""
echo "── Session setup ─────────────────────────────────────────────────"

DOMAIN_TEST_DIR="$PROJECTS_ROOT/.code-vm-domains-test"
mkdir -p "$DOMAIN_TEST_DIR"
echo ".example.org" > "$DOMAIN_TEST_DIR/.sandbox-domains"

if (cd "$DOMAIN_TEST_DIR" && "$CODE_VM" -- curl -fsS -o /dev/null --max-time 20 https://example.org); then
    pass "per-workspace .sandbox-domains widens the allowlist"
else
    fail "per-workspace .sandbox-domains widens the allowlist"
fi

FRAGMENTS=$(adm sh -c 'ls /run/sandbox/squid-allow.d')
if echo "$FRAGMENTS" | grep -q '^10-.*\.conf$'; then
    pass "workspace fragment is installed"
else
    fail "workspace fragment is installed (got: $(echo "$FRAGMENTS" | tr '\n' ' '))"
fi

if [ "$(adm stat -c '%U:%G %a' "/run/sandbox/squid-allow.d/$(echo "$FRAGMENTS" | grep '^10-' | head -1)")" = "root:root 444" ]; then
    pass "fragment is root-owned and read-only"
else
    fail "fragment is root-owned and read-only"
fi

HOST_GIT_EMAIL=$(git config --get user.email || true)
if [ -n "$HOST_GIT_EMAIL" ]; then
    if agent git config --get user.email | grep -qx "$HOST_GIT_EMAIL"; then
        pass "host git identity is seeded into the guest"
    else
        fail "host git identity is seeded into the guest"
    fi
fi

rm -rf "$DOMAIN_TEST_DIR"
```

Note: the widened domain persists for the VM's lifetime, so run this section last among egress tests, or recreate the VM before re-running the "non-allowlisted domain blocked" assertion. Add a comment to that effect in the script.

- [ ] **Step 8: Run the tests and commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check && mise run test:vm`
Expected: PASS.

```bash
cd /workspace/vm-sandbox
git add internal/session internal/cli test-vm-sandbox.sh
git commit -m "feat: apply per-workspace allowlist and git identity per session

Squid is reloaded only when a fragment's content actually changes, so
repeated invocations do not drop in-flight connections."
```

---

### Task 8: Credential injection

**Files:**
- Create: `internal/session/credentials.go`, `internal/guest/files/scripts/render-credentials.sh`
- Create: `internal/guest/files/sandbox-templates/{gradle-properties,dotenv,npmrc,netrc}.tpl`
- Modify: `internal/session/session.go` (add the credential step)
- Test: `internal/session/credentials_test.go`, `test-vm-sandbox.sh`

**Interfaces:**
- Consumes: `session.Deps`, `session.HostRunner`, `lima.Client`.
- Produces:
  - `session.SecretRef` struct: `Name string`, `As string`, with `UnmarshalYAML` accepting either a scalar or a `{name, as}` mapping
  - `session.Target` struct: `Template string`, `Dest string`, `Secrets []SecretRef`
  - `session.SecretsFile` struct: `Secrets map[string]struct{ Source string }`, `Targets []Target`
  - `session.ParseSecretsFile(path string) (SecretsFile, bool, error)` — the bool reports whether the file exists
  - `session.ResolveSecrets(ctx context.Context, host HostRunner, sf SecretsFile) (map[string]string, error)`
  - `session.DenyRules(targets []Target) []string`
  - `session.BuildPayload(workspace string, secrets map[string]string, targets []Target) ([]byte, error)`
  - `session.ApplyCredentials(ctx context.Context, d Deps) error`

- [ ] **Step 1: Copy the built-in templates**

```bash
cd /workspace/vm-sandbox
cp /workspace/sandbox-templates/*.tpl internal/guest/files/sandbox-templates/
ls internal/guest/files/sandbox-templates/
```
Expected: `dotenv.tpl gradle-properties.tpl netrc.tpl npmrc.tpl`.

- [ ] **Step 2: Write the failing tests**

`internal/session/credentials_test.go`:

```go
package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const secretsYAML = `
secrets:
  NEXUS_USER:
    source: printf ci-user
  NEXUS_PASS:
    source: printf 'ci-pass\n'
targets:
  - template: gradle-properties
    dest: /home/devuser/.gradle/gradle.properties
    secrets:
      - name: NEXUS_USER
        as: nexusUser
      - name: NEXUS_PASS
        as: nexusPassword
  - template: dotenv
    dest: /workspace/.env.sandbox
    secrets:
      - NEXUS_USER
`

func writeSecrets(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-secrets.yaml"), []byte(secretsYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

func TestParseSecretsFileAcceptsBothSecretForms(t *testing.T) {
	dir := writeSecrets(t)
	sf, ok, err := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	if err != nil || !ok {
		t.Fatalf("ParseSecretsFile: ok=%v err=%v", ok, err)
	}
	if len(sf.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(sf.Targets))
	}
	if got := sf.Targets[0].Secrets[0]; got.Name != "NEXUS_USER" || got.As != "nexusUser" {
		t.Errorf("object form parsed as %+v", got)
	}
	// Shorthand form: the alias defaults to the name.
	if got := sf.Targets[1].Secrets[0]; got.Name != "NEXUS_USER" || got.As != "NEXUS_USER" {
		t.Errorf("scalar form parsed as %+v", got)
	}
}

func TestParseSecretsFileMissingIsNotAnError(t *testing.T) {
	_, ok, err := ParseSecretsFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("ParseSecretsFile: %v", err)
	}
	if ok {
		t.Error("ok must be false for a missing file")
	}
}

func TestResolveSecretsTrimsNewlines(t *testing.T) {
	dir := writeSecrets(t)
	sf, _, err := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	if err != nil {
		t.Fatalf("ParseSecretsFile: %v", err)
	}
	host := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		// args is ["-c", "<source command>"]; return a value with a newline.
		if len(args) == 2 && args[1] == "printf ci-user" {
			return []byte("ci-user\n"), nil
		}
		return []byte("ci-pass\n"), nil
	}
	got, err := ResolveSecrets(context.Background(), host, sf)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if got["NEXUS_USER"] != "ci-user" || got["NEXUS_PASS"] != "ci-pass" {
		t.Errorf("ResolveSecrets = %v, want newline-trimmed values", got)
	}
}

func TestResolveSecretsReportsTheFailingSecret(t *testing.T) {
	dir := writeSecrets(t)
	sf, _, _ := ParseSecretsFile(filepath.Join(dir, ".sandbox-secrets.yaml"))
	host := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("boom")
	}
	_, err := ResolveSecrets(context.Background(), host, sf)
	if err == nil {
		t.Fatal("expected an error when a source command fails")
	}
	if !contains(err.Error(), "NEXUS_") {
		t.Errorf("error must name the failing secret, got %q", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (func() bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
})() }

func TestDenyRulesCoverReadAndShellPaths(t *testing.T) {
	rules := DenyRules([]Target{{Dest: "/home/devuser/.netrc"}})
	want := []string{
		"Bash(cat /home/devuser/.netrc*)",
		"Bash(grep * /home/devuser/.netrc*)",
		"Bash(head * /home/devuser/.netrc*)",
		"Bash(python * /home/devuser/.netrc*)",
		"Bash(python3 * /home/devuser/.netrc*)",
		"Bash(tail * /home/devuser/.netrc*)",
		"Read(/home/devuser/.netrc)",
	}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("DenyRules =\n%v\nwant (sorted, de-duplicated)\n%v", rules, want)
	}
}

func TestBuildPayloadShape(t *testing.T) {
	body, err := BuildPayload("/home/st/projects/repo",
		map[string]string{"A": "1"},
		[]Target{{Template: "dotenv", Dest: "/tmp/x", Secrets: []SecretRef{{Name: "A", As: "alias"}}}})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got struct {
		Workspace string            `json:"workspace"`
		Secrets   map[string]string `json:"secrets"`
		Targets   []struct {
			Template string `json:"template"`
			Dest     string `json:"dest"`
			Secrets  []struct {
				Name string `json:"name"`
				As   string `json:"as"`
			} `json:"secrets"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if got.Workspace != "/home/st/projects/repo" {
		t.Errorf("workspace = %q; the guest needs it to resolve custom templates", got.Workspace)
	}
	if got.Secrets["A"] != "1" || len(got.Targets) != 1 || got.Targets[0].Secrets[0].As != "alias" {
		t.Errorf("unexpected payload: %s", body)
	}
}

func TestBuildPayloadOmitsEmptySecretsList(t *testing.T) {
	body, err := BuildPayload("/ws", map[string]string{"A": "1"}, []Target{{Template: "dotenv", Dest: "/tmp/x"}})
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	targets := got["targets"].([]any)
	target := targets[0].(map[string]any)
	if v, present := target["secrets"]; present && v != nil {
		t.Errorf("an empty secrets list must be omitted so the guest renders all secrets, got %v", v)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `undefined: ParseSecretsFile` and friends.

- [ ] **Step 4: Implement `internal/session/credentials.go`**

```go
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	secretsDir     = "/run/sandbox-secrets"
	payloadPath    = secretsDir + "/payload.json"
	denyRulesPath  = secretsDir + "/deny-rules.json"
	secretsFileRel = ".sandbox-secrets.yaml"
)

// SecretRef names a secret and the identifier a template sees.
type SecretRef struct {
	Name string `json:"name"`
	As   string `json:"as"`
}

// UnmarshalYAML accepts the shorthand scalar form ("NAME") as well as the
// aliased mapping form ({name: NAME, as: alias}).
func (r *SecretRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Name, r.As = value.Value, value.Value
		return nil
	}
	var aux struct {
		Name string `yaml:"name"`
		As   string `yaml:"as"`
	}
	if err := value.Decode(&aux); err != nil {
		return err
	}
	if aux.Name == "" {
		return errors.New("secret entry requires a name")
	}
	r.Name = aux.Name
	r.As = aux.As
	if r.As == "" {
		r.As = aux.Name
	}
	return nil
}

// Target is one rendered credential file.
type Target struct {
	Template string      `yaml:"template" json:"template"`
	Dest     string      `yaml:"dest" json:"dest"`
	Secrets  []SecretRef `yaml:"secrets,omitempty" json:"secrets,omitempty"`
}

// SecretsFile is the parsed .sandbox-secrets.yaml.
type SecretsFile struct {
	Secrets map[string]struct {
		Source string `yaml:"source"`
	} `yaml:"secrets"`
	Targets []Target `yaml:"targets"`
}

// ParseSecretsFile reads the credential config. The bool reports existence;
// a missing file is the normal case for most projects.
func ParseSecretsFile(path string) (SecretsFile, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return SecretsFile{}, false, nil
	}
	if err != nil {
		return SecretsFile{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var sf SecretsFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return SecretsFile{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return sf, true, nil
}

// ResolveSecrets runs each source command on the host, where the credential
// tooling (gopass, sops) is configured. Values are newline-trimmed; multi-line
// values such as PEM keys are not supported.
func ResolveSecrets(ctx context.Context, host HostRunner, sf SecretsFile) (map[string]string, error) {
	if host == nil {
		host = ExecHost
	}
	out := make(map[string]string, len(sf.Secrets))
	names := make([]string, 0, len(sf.Secrets))
	for n := range sf.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		src := sf.Secrets[name].Source
		if strings.TrimSpace(src) == "" {
			return nil, fmt.Errorf("secret %s has no source command", name)
		}
		val, err := host(ctx, "bash", "-c", src)
		if err != nil {
			return nil, fmt.Errorf("credential source for %s failed: %w", name, err)
		}
		out[name] = strings.ReplaceAll(strings.TrimSpace(string(val)), "\n", "")
	}
	return out, nil
}

// DenyRules generates the Claude Code deny patterns that stop the agent
// reading the rendered credential files, whether via the Read tool or a shell.
func DenyRules(targets []Target) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		if t.Dest == "" {
			continue
		}
		for _, r := range []string{
			fmt.Sprintf("Read(%s)", t.Dest),
			fmt.Sprintf("Bash(cat %s*)", t.Dest),
			fmt.Sprintf("Bash(grep * %s*)", t.Dest),
			fmt.Sprintf("Bash(head * %s*)", t.Dest),
			fmt.Sprintf("Bash(tail * %s*)", t.Dest),
			fmt.Sprintf("Bash(python * %s*)", t.Dest),
			fmt.Sprintf("Bash(python3 * %s*)", t.Dest),
		} {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

// BuildPayload renders the JSON the guest renderer consumes. workspace is
// included so custom template paths resolve against the project directory.
func BuildPayload(workspace string, secrets map[string]string, targets []Target) ([]byte, error) {
	body, err := json.Marshal(struct {
		Workspace string            `json:"workspace"`
		Secrets   map[string]string `json:"secrets"`
		Targets   []Target          `json:"targets"`
	}{workspace, secrets, targets})
	if err != nil {
		return nil, fmt.Errorf("marshal credential payload: %w", err)
	}
	return body, nil
}

// ApplyCredentials resolves the workspace's credentials on the host and has
// the guest render them.
//
// Ordering: the deny rules must be in place before lock-settings.sh runs, and
// lock-settings.sh must run before the files are rendered — the same order the
// container sandbox's entrypoint uses.
func ApplyCredentials(ctx context.Context, d Deps) error {
	sf, ok, err := ParseSecretsFile(filepath.Join(d.Workspace, secretsFileRel))
	if err != nil {
		return err
	}
	if !ok || len(sf.Targets) == 0 {
		return nil
	}
	secrets, err := ResolveSecrets(ctx, d.Host, sf)
	if err != nil {
		return err
	}
	payload, err := BuildPayload(d.Workspace, secrets, sf.Targets)
	if err != nil {
		return err
	}
	deny, err := json.Marshal(DenyRules(sf.Targets))
	if err != nil {
		return fmt.Errorf("marshal deny rules: %w", err)
	}

	// tmpfs so secret material never touches the guest disk.
	if err := d.Client.Admin(ctx, []string{"sh", "-c",
		fmt.Sprintf("install -d -m 0700 %s && (mount | grep -q ' %s ' || mount -t tmpfs -o mode=0700,nosuid,nodev,size=1m tmpfs %s)",
			secretsDir, secretsDir, secretsDir)}); err != nil {
		return err
	}

	for _, f := range []struct {
		body []byte
		dst  string
	}{{payload, payloadPath}, {deny, denyRulesPath}} {
		tmp, err := os.CreateTemp("", "code-vm-cred-*")
		if err != nil {
			return fmt.Errorf("create temp credential file: %w", err)
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return fmt.Errorf("chmod temp credential file: %w", err)
		}
		_, werr := tmp.Write(f.body)
		tmp.Close()
		if werr != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("write temp credential file: %w", werr)
		}
		staged := "/tmp/" + filepath.Base(tmp.Name())
		cerr := d.Client.Copy(ctx, tmp.Name(), staged)
		os.Remove(tmp.Name())
		if cerr != nil {
			return cerr
		}
		if err := d.Client.Admin(ctx, []string{"install", "-m", "0400", "-o", "root", "-g", "root", staged, f.dst}); err != nil {
			return err
		}
		if err := d.Client.Admin(ctx, []string{"rm", "-f", staged}); err != nil {
			return err
		}
	}

	if err := d.Client.Admin(ctx, []string{"/usr/local/lib/sandbox/lock-settings.sh"}); err != nil {
		return err
	}
	return d.Client.Admin(ctx, []string{"/usr/local/lib/sandbox/render-credentials.sh"})
}
```

Add the step to `internal/session/session.go`:

```go
func Setup(ctx context.Context, d Deps) error {
	if err := ApplyAllowlist(ctx, d); err != nil {
		return err
	}
	if err := ApplyGitIdentity(ctx, d); err != nil {
		return err
	}
	return ApplyCredentials(ctx, d)
}
```

- [ ] **Step 5: Write `render-credentials.sh`**

Create `internal/guest/files/scripts/render-credentials.sh`. This is the container sandbox's script with the hardcoded `/workspace` replaced by the payload's `workspace` field and the agent user parameterised. Copy `/workspace/render-credentials.sh` and apply these edits:

```bash
# Header block: document the new payload field.
#   "workspace": "/absolute/path/to/the/project"   # template lookup root
```

```bash
# After sourcing provision.env and reading the payload, derive the lookup root:
# shellcheck source=/dev/null
. /etc/sandbox/provision.env

PAYLOAD=/run/sandbox-secrets/payload.json
BUILTIN_TEMPLATES=/usr/local/share/sandbox-templates

payload_json=$(cat "$PAYLOAD")
WORKSPACE=$(printf '%s' "$payload_json" | jq -r '.workspace')
if [ -z "$WORKSPACE" ] || [ "$WORKSPACE" = "null" ]; then
    echo "[render-credentials] ERROR: payload is missing the workspace field"
    exit 1
fi
WORKSPACE_TEMPLATES="$WORKSPACE/.sandbox-templates"
```

```bash
# In the template resolution branch, replace /workspace with $WORKSPACE:
        template_path="$WORKSPACE/$template_name"
```

```bash
# In the lock step, use the agent user from provision.env:
    chown "root:${AGENT_USER}" "$dest"
    chmod 0444 "$dest"
```

Keep everything else — the `trap` that wipes secret material on any exit, the two-tier template lookup, the absolute-path validation on `dest`, and the final wipe — unchanged.

- [ ] **Step 6: Add VM assertions**

Append to `test-vm-sandbox.sh`:

```bash
echo ""
echo "── Credential injection ──────────────────────────────────────────"

CRED_DIR="$PROJECTS_ROOT/.code-vm-cred-test"
mkdir -p "$CRED_DIR"
CRED_DEST="/home/$AGENT_USER/.code-vm-test.properties"
cat > "$CRED_DIR/.sandbox-secrets.yaml" <<YAML
secrets:
  TEST_USER:
    source: printf sandbox-user
targets:
  - template: gradle-properties
    dest: $CRED_DEST
    secrets:
      - name: TEST_USER
        as: testUser
YAML

(cd "$CRED_DIR" && "$CODE_VM" -- true > /dev/null 2>&1)

if [ "$(adm stat -c '%U:%G %a' "$CRED_DEST" 2>/dev/null)" = "root:$AGENT_USER 444" ]; then
    pass "rendered credential is root-owned and read-only"
else
    fail "rendered credential is root-owned and read-only (got $(adm stat -c '%U:%G %a' "$CRED_DEST" 2>/dev/null))"
fi

if adm grep -q 'testUser=sandbox-user' "$CRED_DEST"; then
    pass "credential rendered through the gradle-properties template"
else
    fail "credential rendered through the gradle-properties template"
fi

assert_fails "agent cannot overwrite the rendered credential" \
    agent bash -c "echo x > $CRED_DEST"

if adm jq -e --arg d "$CRED_DEST" '.permissions.deny | index("Read(" + $d + ")")' \
        "/home/$AGENT_USER/.claude/settings.json" > /dev/null; then
    pass "credential deny rule merged into settings.json"
else
    fail "credential deny rule merged into settings.json"
fi

assert_fails "secret payload is wiped from the guest" \
    adm test -f /run/sandbox-secrets/payload.json

adm rm -f "$CRED_DEST"
rm -rf "$CRED_DIR"
```

- [ ] **Step 7: Run the tests and commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check && mise run test:vm`
Expected: PASS.

```bash
cd /workspace/vm-sandbox
git add internal/session internal/guest test-vm-sandbox.sh
git commit -m "feat: inject workspace credentials into the guest

Secrets resolve on the host, where gopass and sops are configured, and
travel through a tmpfs-backed payload that the renderer wipes. The
payload carries the workspace path because custom templates resolve
against the project directory, which is no longer a fixed /workspace."
```

---

### Task 9: Lifecycle subcommands — `mount`, `status`, `stop`, `recreate`, `proxy-log`

**Files:**
- Create: `internal/cli/mount.go`, `internal/cli/status.go`, `internal/cli/stop.go`, `internal/cli/recreate.go`, `internal/cli/proxylog.go`
- Create: `internal/guest/files/scripts/proxy-log.sh`
- Modify: `internal/cli/root.go` (register the commands)
- Test: `internal/cli/mount_test.go`, `internal/cli/proxylog_test.go`

**Interfaces:**
- Consumes: `config.Config`, `config.Save`, `config.CoveringMount`, `lima.Client`, `cli.newClient`, `cli.renderInstanceFile`.
- Produces:
  - `cli.addMount(c config.Config, path string) (config.Config, bool, error)` — returns the updated config and whether it changed; errors when the path does not exist or is not a directory
  - `cli.proxyLogArgs(mode string) ([]string, error)`
  - `code-vm mount`, `status`, `stop`, `recreate`, `proxy-log`

- [ ] **Step 1: Write the failing tests**

`internal/cli/mount_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestAddMountAppendsNewDirectory(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	c := config.Default()
	c.ProjectsRoot = root

	got, changed, err := addMount(c, other)
	if err != nil {
		t.Fatalf("addMount: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true for a new directory")
	}
	if len(got.ExtraMounts) != 1 || got.ExtraMounts[0] != other {
		t.Errorf("ExtraMounts = %v, want [%s]", got.ExtraMounts, other)
	}
}

func TestAddMountIsNoOpForAlreadyCoveredPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "repo")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	c := config.Default()
	c.ProjectsRoot = root

	got, changed, err := addMount(c, sub)
	if err != nil {
		t.Fatalf("addMount: %v", err)
	}
	if changed {
		t.Error("a path already under the projects root needs no new mount")
	}
	if len(got.ExtraMounts) != 0 {
		t.Errorf("ExtraMounts = %v, want empty", got.ExtraMounts)
	}
}

func TestAddMountRejectsMissingOrNonDirectory(t *testing.T) {
	root := t.TempDir()
	c := config.Default()
	c.ProjectsRoot = root

	if _, _, err := addMount(c, filepath.Join(root, "absent")); err == nil {
		t.Error("expected an error for a path that does not exist")
	}

	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := addMount(c, file); err == nil {
		t.Error("expected an error for a path that is not a directory")
	}
}
```

`internal/cli/proxylog_test.go`:

```go
package cli

import (
	"reflect"
	"testing"
)

func TestProxyLogArgs(t *testing.T) {
	for _, mode := range []string{"all", "denied", "allowed", "follow"} {
		got, err := proxyLogArgs(mode)
		if err != nil {
			t.Fatalf("proxyLogArgs(%q): %v", mode, err)
		}
		want := []string{"/usr/local/lib/sandbox/proxy-log.sh", mode}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("proxyLogArgs(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestProxyLogArgsRejectsUnknownMode(t *testing.T) {
	if _, err := proxyLogArgs("everything"); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `mise run test:unit`
Expected: FAIL — `undefined: addMount`, `undefined: proxyLogArgs`.

- [ ] **Step 3: Implement `internal/cli/mount.go`**

```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
)

// addMount adds path to the shared directories unless an existing mount
// already covers it. Lima declares mounts in the instance config, so this
// requires a VM restart to take effect.
func addMount(c config.Config, path string) (config.Config, bool, error) {
	p, err := config.ExpandPath(path)
	if err != nil {
		return c, false, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return c, false, fmt.Errorf("cannot share %s: %w", p, err)
	}
	if !fi.IsDir() {
		return c, false, fmt.Errorf("cannot share %s: not a directory", p)
	}
	if m, ok := config.CoveringMount(c.Mounts(), p); ok {
		_ = m
		return c, false, nil
	}
	c.ExtraMounts = append(c.ExtraMounts, p)
	return c, true, nil
}

func newMountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mount <directory>",
		Short: "Share an additional host directory with the sandbox VM",
		Long: "Share an additional host directory with the sandbox VM.\n\n" +
			"Lima declares mounts in the instance configuration, so the VM is\n" +
			"restarted to apply the change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, path, err := loadConfig()
			if err != nil {
				return err
			}
			updated, changed, err := addMount(c, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !changed {
				fmt.Fprintf(out, "%s is already shared; nothing to do.\n", args[0])
				return nil
			}
			if err := updated.Save(path); err != nil {
				return err
			}
			fmt.Fprintf(out, "Added %s to %s.\n", args[0], path)

			cl := newClient()
			status, err := cl.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status != "Running" {
				fmt.Fprintln(out, "VM is not running; the new mount applies on next start.")
				return nil
			}
			fmt.Fprintln(out, "Restarting the VM to apply the new mount...")
			if err := cl.Stop(cmd.Context()); err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), cl, updated)
		},
	}
}
```

- [ ] **Step 4: Implement the remaining commands**

`internal/cli/stop.go`:

```go
package cli

import "github.com/spf13/cobra"

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the sandbox VM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return newClient().Stop(cmd.Context())
		},
	}
}
```

`internal/cli/recreate.go`:

```go
package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newRecreateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "recreate",
		Short: "Delete and rebuild the sandbox VM from scratch",
		Long: "Delete and rebuild the sandbox VM.\n\n" +
			"This destroys the guest disk, which holds Claude authentication,\n" +
			"installed plugins and the Docker image cache. Workspace files live\n" +
			"on the host and are unaffected.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			if !yes {
				fmt.Fprint(cmd.OutOrStdout(),
					"This deletes the guest disk, including Claude authentication and the\n"+
						"Docker image cache. Workspace files are not affected. Continue? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(line)) != "y" {
					return fmt.Errorf("aborted")
				}
			}
			cl := newClient()
			if err := cl.Delete(cmd.Context()); err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), cl, c)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
```

`internal/cli/status.go`:

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the sandbox VM's state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, path, err := loadConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			cl := newClient()
			status, err := cl.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status == "" {
				status = "not created"
			}
			fmt.Fprintf(out, "instance:      %s (%s)\n", lima.InstanceName, status)
			fmt.Fprintf(out, "config:        %s\n", path)
			fmt.Fprintf(out, "cpus/memory:   %d / %s\n", c.CPUs, c.Memory)
			fmt.Fprintln(out, "shared paths:")
			for _, m := range c.Mounts() {
				fmt.Fprintf(out, "  %s\n", m)
			}
			if status != "Running" {
				return nil
			}
			fmt.Fprintln(out, "firewall:")
			verify, err := cl.AdminOutput(cmd.Context(), []string{"cat", "/run/firewall-verify"})
			if err != nil {
				fmt.Fprintln(out, "  unavailable")
				return nil
			}
			fmt.Fprintf(out, "%s", indentLines(string(verify), "  "))
			return nil
		},
	}
}

// indentLines prefixes every non-empty line, for readable nested output.
func indentLines(s, prefix string) string {
	var b []byte
	for _, line := range splitLines(s) {
		if line == "" {
			continue
		}
		b = append(b, prefix...)
		b = append(b, line...)
		b = append(b, '\n')
	}
	return string(b)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
```

`internal/cli/proxylog.go`:

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// proxyLogArgs validates the mode and builds the guest command.
func proxyLogArgs(mode string) ([]string, error) {
	switch mode {
	case "all", "denied", "allowed", "follow":
		return []string{"/usr/local/lib/sandbox/proxy-log.sh", mode}, nil
	default:
		return nil, fmt.Errorf("unknown mode %q; want all, denied, allowed or follow", mode)
	}
}

func newProxyLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "proxy-log [all|denied|allowed|follow]",
		Short:     "Read the Squid access log from the sandbox VM",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"all", "denied", "allowed", "follow"},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := "all"
			if len(args) == 1 {
				mode = args[0]
			}
			guestCmd, err := proxyLogArgs(mode)
			if err != nil {
				return err
			}
			return newClient().Admin(cmd.Context(), guestCmd)
		},
	}
}
```

`internal/guest/files/scripts/proxy-log.sh`:

```bash
#!/bin/bash
###############################################################################
# proxy-log.sh — read the Squid access log inside the sandbox VM
###############################################################################
set -uo pipefail

LOG=/var/log/squid/access.log
mode=${1:-all}

if [ ! -f "$LOG" ]; then
    echo "proxy-log: $LOG does not exist yet" >&2
    exit 1
fi

case "$mode" in
    all) cat "$LOG" ;;
    denied) grep -E 'DENIED' "$LOG" || true ;;
    allowed) grep -vE 'DENIED' "$LOG" || true ;;
    follow) tail -f "$LOG" ;;
    *)
        echo "usage: proxy-log.sh [all|denied|allowed|follow]" >&2
        exit 2
        ;;
esac
```

Register everything in `NewRootCmd`:

```go
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newStartCmd())
	root.AddCommand(newStopCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newMountCmd())
	root.AddCommand(newRecreateCmd())
	root.AddCommand(newProxyLogCmd())
```

- [ ] **Step 5: Add VM assertions**

Append to `test-vm-sandbox.sh`:

```bash
echo ""
echo "── Lifecycle commands ────────────────────────────────────────────"

assert_ok "status reports the running instance" \
    bash -c "\"$CODE_VM\" status | grep -q 'code-sandbox (Running)'"
assert_ok "status shows the firewall verification" \
    bash -c "\"$CODE_VM\" status | grep -q 'OUTPUT_POLICY=DROP'"
assert_ok "proxy-log denied mode runs" "$CODE_VM" proxy-log denied
assert_fails "proxy-log rejects an unknown mode" "$CODE_VM" proxy-log everything

MOUNT_TEST_DIR=$(mktemp -d)
if "$CODE_VM" mount "$MOUNT_TEST_DIR" | grep -q "Added"; then
    pass "mount adds a new shared directory"
else
    fail "mount adds a new shared directory"
fi
if (cd "$MOUNT_TEST_DIR" && "$CODE_VM" -- pwd | grep -qx "$MOUNT_TEST_DIR"); then
    pass "newly mounted directory is usable as a workspace"
else
    fail "newly mounted directory is usable as a workspace"
fi
if "$CODE_VM" mount "$MOUNT_TEST_DIR" | grep -q "already shared"; then
    pass "mount is idempotent"
else
    fail "mount is idempotent"
fi
```

Note that `code-vm mount` rewrites `~/.config/code-vm/config.yaml`. The suite leaves the temp mount in the config; add a cleanup step that removes it with `yq -i 'del(.extraMounts[] | select(. == "'"$MOUNT_TEST_DIR"'"))' ~/.config/code-vm/config.yaml` and `rmdir "$MOUNT_TEST_DIR"`, then note in the script comment that the VM keeps the stale mount until the next restart.

- [ ] **Step 6: Run the tests and commit**

Run: `mise run test:unit && mise run lint && mise run fmt-check && mise run test:vm`
Expected: PASS.

```bash
cd /workspace/vm-sandbox
git add internal/cli internal/guest test-vm-sandbox.sh
git commit -m "feat: add mount, status, stop, recreate and proxy-log

mount restarts the VM because Lima declares mounts in the instance
config; recreate confirms first, since the guest disk holds Claude
authentication and the image cache."
```

---

### Task 10: Suite completion, README, and the CI split

**Files:**
- Modify: `test-vm-sandbox.sh` (remaining assertions)
- Create: `README.md`
- Modify: `.github/workflows/ci.yml` (document the split explicitly)

**Interfaces:**
- Consumes: everything from Tasks 1-9. Produces no new Go API.

- [ ] **Step 1: Add the virtiofs ownership assertions**

Append to `test-vm-sandbox.sh`, before the lifecycle section:

```bash
echo ""
echo "── Workspace file ownership ──────────────────────────────────────"

OWN_DIR="$PROJECTS_ROOT/.code-vm-own-test"
mkdir -p "$OWN_DIR"
echo host > "$OWN_DIR/from-host"

if [ "$(adm stat -c '%u' "$OWN_DIR/from-host")" = "$(id -u)" ]; then
    pass "host-created file is owned by the agent UID in the guest"
else
    fail "host-created file is owned by the agent UID in the guest"
fi

(cd "$OWN_DIR" && agent bash -c 'echo guest > from-guest')
if [ -f "$OWN_DIR/from-guest" ] && [ "$(stat -c '%u' "$OWN_DIR/from-guest")" = "$(id -u)" ]; then
    pass "guest-created file is owned by the host user on the host"
else
    fail "guest-created file is owned by the host user on the host"
fi

rm -rf "$OWN_DIR"
```

- [ ] **Step 2: Add the Docker API primitives that testcontainers depends on**

The suite verifies the primitives rather than driving a framework: a full JVM
testcontainers run needs a project toolchain and belongs with the projects that
use it. What is asserted here is exactly what testcontainers requires from the
daemon — a reachable API socket, socket bind-mounting, Ryuk, and published
ports.

```bash
echo ""
echo "── Testcontainers primitives ─────────────────────────────────────"

AGENT_SOCK="/run/user/$(id -u)/docker.sock"

assert_ok "Docker API responds over DOCKER_HOST" \
    agent docker version --format '{{.Server.Version}}'

assert_ok "the daemon socket can be bind-mounted into a container" \
    agent docker run --rm -v "$AGENT_SOCK:/var/run/docker.sock" docker:cli docker version

if agent docker run -d --rm --name code-vm-ryuk \
        -v "$AGENT_SOCK:/var/run/docker.sock" \
        -e RYUK_PORT=8080 testcontainers/ryuk:0.11.0 > /dev/null 2>&1; then
    pass "Ryuk starts (it is incompatible with rootless Podman, which is why this exists)"
    agent docker rm -f code-vm-ryuk > /dev/null 2>&1
else
    fail "Ryuk starts"
fi

agent docker run -d --rm --name code-vm-ports -p 18080:80 nginx:alpine > /dev/null 2>&1
sleep 3
if agent curl -fsS -o /dev/null --max-time 10 --noproxy 127.0.0.1 http://127.0.0.1:18080; then
    pass "published container ports are reachable inside the guest"
else
    fail "published container ports are reachable inside the guest"
fi
agent docker rm -f code-vm-ports > /dev/null 2>&1
```

Pin the Ryuk tag to the current release at implementation time (check
`https://hub.docker.com/r/testcontainers/ryuk/tags`); `0.11.0` is a placeholder
for whatever is current and must be replaced with the version you verify.

- [ ] **Step 3: Add the restart assertions last**

Append at the very end of `test-vm-sandbox.sh`, before the summary block:

```bash
echo ""
echo "── Restart hygiene ───────────────────────────────────────────────"
# Runs last: it verifies the tmpfs allowlist is cleared, which undoes the
# widened domain the session-setup section installed.

"$CODE_VM" stop > /dev/null 2>&1
"$CODE_VM" start > /dev/null 2>&1

if [ "$(adm sh -c 'ls /run/sandbox/squid-allow.d | tr "\n" " "')" = "00-base.conf " ]; then
    pass "allowlist fragments are cleared by a VM restart"
else
    fail "allowlist fragments are cleared by a VM restart"
fi

assert_fails "the previously widened domain is blocked again after restart" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

assert_ok "settings stay locked after restart" \
    bash -c "[ \"\$(limactl shell $INSTANCE sudo stat -c '%U:%G %a' /home/$AGENT_USER/.claude/settings.json)\" = 'root:$AGENT_USER 444' ]"
```

- [ ] **Step 4: Run the full suite**

Run: `mise run test:unit && mise run lint && mise run fmt-check`
Expected: PASS.

```bash
cd /workspace/vm-sandbox
limactl delete --force code-sandbox
mise run test:vm
```
Expected: every assertion PASS from a clean guest. Record the wall-clock time of the first boot for the README.

- [ ] **Step 5: Write the README**

Create `README.md`:

```markdown
# code-vm

Run Claude Code in a hardened VM with real Docker.

`code-vm` is the VM-based sibling of the container sandbox. The container
version emulates a container runtime with rootless Podman, which breaks
Docker/Podman compatibility, bridge DNS, and anything needing privileges.
A VM has its own kernel, so it runs a real Docker daemon instead.

## What you get

- **Real Docker.** Rootless `dockerd`, the real Docker CLI and API. `docker
  compose` service discovery, buildx, and testcontainers behave as they do on
  a developer machine.
- **Egress allowlist.** Squid domain allowlist plus iptables default-deny,
  enforced inside the guest where the agent has no sudo.
- **Non-root agent.** The agent runs as `devuser`, whose UID and GID mirror
  yours so workspace files stay host-owned. `limaadmin` holds sudo and is used
  only by `code-vm` for privileged setup.
- **Locked permissions.** `~/.claude/settings.json` is root-owned and
  read-only in the guest; `settings.local.json` is pre-claimed.
- **No host credentials.** Only the directories you configure are shared. Host
  `$HOME`, `~/.ssh` and `~/.aws` are not visible in the guest.

## Prerequisites

- Linux x86_64 with KVM (`/dev/kvm` readable and writable by your user)
- [Lima](https://lima-vm.io) 2.2.0 or newer, and `virtiofsd`
- [mise](https://mise.jdx.dev) for the build toolchain

Run `code-vm doctor` to check all of the above.

macOS is expected to work (Lima supports `vz`) but is not tested.

## Quick start

```bash
mise run build
sudo install -m 0755 dist/code-vm /usr/local/bin/code-vm

# Configure which directories are shared
mkdir -p ~/.config/code-vm
cat > ~/.config/code-vm/config.yaml <<'YAML'
projectsRoot: ~/projects
cpus: 4
memory: 12GiB
disk: 100GiB
YAML

code-vm doctor
code-vm start        # first boot provisions the VM; expect several minutes

cd ~/projects/my-repo
code-vm -- claude login          # once; persists on the guest disk
code-vm -- claude -p "fix the failing test" --max-turns 20
code-vm                          # interactive shell
```

## Commands

| Command | Purpose |
|---|---|
| `code-vm` | Interactive shell in the guest, at the current directory |
| `code-vm -- <cmd>` | Run a command as the agent, at the current directory |
| `code-vm start` / `stop` | Bring the VM up (idempotent) or shut it down |
| `code-vm status` | Instance state, shared paths, firewall verification |
| `code-vm mount <dir>` | Share another host directory (restarts the VM) |
| `code-vm recreate` | Delete and rebuild the guest from scratch |
| `code-vm proxy-log [all\|denied\|allowed\|follow]` | Read the Squid access log |
| `code-vm doctor` | Check host prerequisites |

## Configuration

`~/.config/code-vm/config.yaml`:

```yaml
projectsRoot: ~/projects      # the one directory always shared
extraMounts:                  # added by `code-vm mount`
  - ~/work/other-repo
cpus: 4
memory: 12GiB
disk: 100GiB
extraDomains:                 # added to the Squid allowlist
  - registry.mycompany.com
containerProxy: false         # see below
```

Per-project extras live in the project directory:

- `.sandbox-domains` — extra allowed domains for that project. Compiled into a
  Squid fragment in tmpfs, so it is forgotten when the VM restarts.
- `.sandbox-secrets.yaml` — credentials to inject. Secrets resolve on the
  **host** (where `gopass`/`sops` live), render in the guest, and the payload is
  wiped. Rendered files are `root:devuser 0444` and deny rules are merged into
  `settings.json` so the agent cannot read them.

### `containerProxy`

Off by default. When on, `docker run` and `docker build` containers get
`http_proxy` pointed at the guest's Squid. That is useful when image builds need
to fetch packages, but it also injects the proxy into `docker compose` service
containers — where a bare service name like `db` matches no `noProxy` entry and
would be routed to Squid, breaking service-to-service traffic. Enable it per
project only when you need it.

## Security model

The perimeter is the VM boundary. Inside it, the agent is separated from guest
root: `devuser` has no sudo and the rootful Docker daemon is masked, so the
agent's own rootless `dockerd` cannot be used to become guest root.

### Known limitations

These are consequences of the design, not oversights:

- **No `--privileged` containers**, no arbitrary `sysctl`s, no host networking.
  That is the cost of rootless Docker, accepted in exchange for real separation
  between the agent and guest root.
- **No cross-project isolation.** One agent user with all workspaces mounted
  means one project's agent can read another's tree and injected credentials.
- **Union allowlist.** The allowlist is the union of every project used during
  the VM's current lifetime.
- **Mounts need a VM restart**, because Lima declares them in the instance
  config.
- **Guest root is reachable from the host** by anyone who can run `limactl` —
  you, never the agent.

## Testing

```bash
mise run test:unit   # Go tests: config, template rendering, argv construction
mise run lint        # golangci-lint + shellcheck
mise run test:vm     # full VM suite; requires KVM
```

CI runs everything except `test:vm`, which needs nested KVM that GitHub-hosted
runners do not reliably provide. Run it locally, or on a KVM-capable runner.

The VM suite asserts the primitives testcontainers depends on — API socket,
socket bind-mounting, Ryuk, published ports — rather than driving a JVM
testcontainers run, which belongs with the projects that use it.
```

- [ ] **Step 6: Document the CI split in the workflow**

Add a comment block at the top of `.github/workflows/ci.yml`, under `name: ci`:

```yaml
# The VM suite (mise run test:vm) is deliberately absent: it needs nested KVM,
# which GitHub-hosted runners do not reliably provide. Run it locally or add a
# KVM-capable self-hosted runner and a second job that calls it.
```

- [ ] **Step 7: Final verification and commit**

Run: `mise run fmt-check && mise run lint && mise run test:unit && mise run build`
Expected: all PASS.

Run: `mise run test:vm`
Expected: all PASS. Report the actual pass/fail counts — do not claim the suite
passes without the output in front of you.

```bash
cd /workspace/vm-sandbox
git add README.md test-vm-sandbox.sh .github
git commit -m "docs: add README and complete the VM test suite

The suite asserts the Docker primitives testcontainers needs rather
than driving a JVM run, and the known limitations are documented as
decisions rather than left to be discovered."
```

---

## Self-Review

**Spec coverage.** Every spec section maps to a task:

| Spec section | Task |
|---|---|
| Users (`limaadmin` 60000, `devuser` host UID) | 2 (template), 4 (creation) |
| VM boundary, mount scope, `mounts: []` override | 2 (template + invariant tests), 4 (VM assertion) |
| virtiofs, no `reverse-sshfs` | 2 (invariant test) |
| Egress firewall, agent-UID REJECT, no host gateway | 6 |
| Permission settings lock, `enabledPlugins`, `settings.local.json` | 6 |
| What the VM removes (seccomp, `/proc` reharden, Podman, netavark) | Nothing to port; absence asserted by the compose DNS test in 6 |
| Inner-container egress | 4 (`containerProxy`), 6 (iptables rule) |
| `sandbox-boot.service` ordering | 4 (unit), 6 (real sequence) |
| First-boot provisioning | 4 |
| Resource limits (`TasksMax`, `MemoryMax`) | 4 (drop-in), 6 (assertion) |
| Persistence (guest disk replaces the volume) | 9 (`recreate` is the reset) |
| Host CLI, Cobra, `go:embed`, `doctor` | 1, 2, 3 |
| Commands and invocation flow | 3, 5, 9 |
| Host config | 1 |
| Squid allowlist fragments, `00-base.conf`, tmpfs | 6 (dir), 7 (fragments) |
| Credential injection | 8 |
| Project layout | 1, 2 |
| Testing (ported + VM-specific assertions) | 4, 5, 6, 7, 8, 9, 10 |
| CI split | 1 (workflow), 10 (documented) |
| Known limitations | 10 (README) |

**Two deliberate deviations from the spec**, both discovered while planning:

1. **Layout.** `go:embed` cannot traverse upward, so the spec's top-level
   `guest/` and `lima/` directories live under `internal/guest/files/`. Same
   contents, reachable by the embed directive.
2. **`containerProxy` is opt-in.** The spec sets Docker's `proxies.default`
   unconditionally. That injects `http_proxy` into `docker compose` service
   containers, where a bare service name matches no `noProxy` entry and would be
   routed to Squid — breaking exactly the service-to-service traffic this design
   exists to fix. Squid stays reachable from containers; the env injection is
   now a config flag, default `false`.

**Placeholder scan.** No `TBD`/`TODO` markers. Three places name a value to
verify rather than fixing it, each with the command to resolve it: the mise tool
versions (Task 1 Step 1), the Claude Code and OpenCode installer URLs (Task 6
Step 4), and the Ryuk image tag (Task 10 Step 2). These are external facts that
will have moved by implementation time; the plan says how to resolve each.

**Type consistency.** `Deps`, `HostRunner`, `Target`, `SecretRef`,
`RenderParams`, `DataFile`, `Client`, `Runner` are defined once and used with the
same field names throughout. `AdminArgs`/`AgentArgs` naming is consistent between
Tasks 3, 7 and 8. One inline correction is flagged where it occurs: Task 7 Step 4
notes that `install -o user:group` is invalid and gives the corrected `-o`/`-g`
form.

