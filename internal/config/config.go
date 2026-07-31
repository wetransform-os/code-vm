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
