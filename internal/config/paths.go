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

// ProfilesDirFor returns the profile bundle directory belonging to a config
// file: a "profiles" directory next to it. Deriving it from the config path —
// rather than a fixed location — keeps a --config test setup fully isolated.
func ProfilesDirFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "profiles")
}
