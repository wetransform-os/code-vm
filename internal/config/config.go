package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// sizeRe matches Lima-style size strings such as "12GiB" or "512MiB".
var sizeRe = regexp.MustCompile(`^[0-9]+(B|KiB|MiB|GiB|TiB)$`)

// domainRe matches an allowlist entry: a hostname, optionally prefixed with a
// dot to mean "this domain and all subdomains". Entries are interpolated into
// Squid `acl allowed_domains dstdomain <entry>` lines, so anything that could
// split the line or start a new directive — whitespace, quotes, semicolons,
// schemes, ports, paths — must be rejected here rather than reaching squid.conf.
var domainRe = regexp.MustCompile(`^\.?[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// DefaultInstance is the Lima instance code-vm manages unless the config names
// another one.
const DefaultInstance = "code-sandbox"

// instanceRe matches names Lima accepts, and that are safe to interpolate into
// the generated YAML and into shell commands in the readiness probe.
var instanceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// Config is the code-vm host configuration. It is the entire knob surface:
// the Lima instance is rendered from it, so the VM shape stays reproducible.
type Config struct {
	// Instance is the Lima instance this config drives. Selecting it here is
	// what lets a throwaway VM exist alongside the one in daily use: the test
	// suite points --config at its own file and never touches the real VM.
	Instance     string   `yaml:"instance"`
	ProjectsRoot string   `yaml:"projectsRoot"`
	ExtraMounts  []string `yaml:"extraMounts,omitempty"`
	// VMType selects the Lima hypervisor driver. Empty (default) means
	// the host's accelerated one, so nothing has to be set on either platform;
	// see ResolveVMType.
	VMType         string   `yaml:"vmType,omitempty"`
	CPUs           int      `yaml:"cpus"`
	Memory         string   `yaml:"memory"`
	Disk           string   `yaml:"disk"`
	ExtraDomains   []string `yaml:"extraDomains,omitempty"`
	ContainerProxy bool     `yaml:"containerProxy"`
	// Profiles names the customization bundles applied to the guest, in
	// order: later profiles win file collisions and the last declared shell
	// wins. Each name must exist under the profiles directory next to this
	// config file; that is checked when profiles are loaded, not here.
	Profiles []string `yaml:"profiles,omitempty"`
}

// Default returns the built-in configuration. Disk is large because Docker
// image layers accumulate inside the guest.
func Default() Config {
	return Config{
		Instance:       DefaultInstance,
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
	// An explicit empty instance means "the default", so a hand-written config
	// that omits the key keeps working.
	if c.Instance == "" {
		c.Instance = DefaultInstance
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
	if !instanceRe.MatchString(c.Instance) {
		return fmt.Errorf("instance must be a Lima instance name like %q, got %q", DefaultInstance, c.Instance)
	}
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
	if err := ValidateVMType(c.VMType); err != nil {
		return err
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
	for i, d := range c.ExtraDomains {
		if err := ValidateDomain(d); err != nil {
			return fmt.Errorf("extraDomains[%d]: %w", i, err)
		}
	}
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
	return nil
}

// ValidateDomain reports whether d is usable as an allowlist entry.
func ValidateDomain(d string) error {
	if !domainRe.MatchString(d) {
		return fmt.Errorf("not a valid domain: %q (expected e.g. %q or %q)", d, "registry.example.com", ".example.com")
	}
	return nil
}

// MountsExclude reports an error if any shared directory would expose path
// inside the guest. The host config is the only trusted input to the egress
// allowlist, which holds only while the agent cannot write it: sharing $HOME
// or ~/.config would hand the agent control of its own firewall.
//
// Both sides are canonicalized before comparison: a lexical prefix check
// would miss a mount, or the config path itself, reached through a symlink —
// an alias directory outside every mount that resolves inside one would
// otherwise sail through untouched.
func (c Config) MountsExclude(path string) error {
	p := CanonicalizeExisting(path)
	mounts := canonicalizeAll(c.Mounts())
	if m, ok := CoveringMount(mounts, p); ok {
		return fmt.Errorf(
			"shared directory %s would expose the code-vm config (%s) to the agent, "+
				"which could then widen its own egress allowlist; narrow projectsRoot or the extra mount",
			m, p)
	}
	return nil
}

// MountsExcludeTree reports an error if any shared directory overlaps dir in
// either direction: a mount above dir exposes it wholesale, and a mount at or
// below it exposes part of it. MountsExclude cannot cover the second case —
// it guards a single file path — and profiles feed the egress allowlist, so
// an agent-writable profile source would be an allowlist the agent controls.
//
// Both sides are canonicalized before comparison; see MountsExclude for why.
func (c Config) MountsExcludeTree(dir string) error {
	d := CanonicalizeExisting(dir)
	mounts := canonicalizeAll(c.Mounts())
	if m, ok := CoveringMount(mounts, d); ok {
		return fmt.Errorf(
			"shared directory %s would expose the code-vm profiles (%s) to the agent, "+
				"which could then widen its own egress allowlist; narrow projectsRoot or the extra mount",
			m, d)
	}
	for _, m := range mounts {
		// m == d is already covered above: CoveringMount treats equality as
		// covering, so a mount exactly at d never reaches this loop.
		if strings.HasPrefix(m, d+string(filepath.Separator)) {
			return fmt.Errorf(
				"shared directory %s lies inside the code-vm profiles directory (%s); "+
					"the agent must not be able to edit profile sources", m, d)
		}
	}
	return nil
}

// canonicalizeAll applies CanonicalizeExisting to every mount for comparison
// purposes. The configured spellings in Mounts() itself are left untouched —
// rendering the Lima template must keep what the user wrote.
func canonicalizeAll(mounts []string) []string {
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = CanonicalizeExisting(m)
	}
	return out
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
