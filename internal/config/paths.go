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

// CanonicalizeExisting resolves symlinks in the longest existing prefix of
// path and rejoins the remainder, so guards compare real filesystem
// locations rather than lexical spellings. A symlinked ancestor — a
// profiles directory whose parent is itself an alias, a config path reached
// through a symlinked directory — would otherwise defeat a plain prefix
// comparison against the mount list: the alias never shares a lexical
// prefix with the mount even though it resolves inside it.
//
// A path that does not exist yet (a profiles dir before the first
// `profile add`, a default config that was never written) still
// canonicalizes through its existing ancestors instead of erroring: only
// the missing tail is left untouched. This is a pure function with no error
// return; worst case, nothing on the path exists and it returns the cleaned
// input.
func CanonicalizeExisting(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	dir := cleaned
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the root without finding an existing prefix; the root
			// always resolves, so this is unreachable in practice, but stop
			// rather than loop forever.
			return cleaned
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
	}
}
