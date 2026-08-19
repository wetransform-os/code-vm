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

// ValidateName reports whether name is usable as a profile name. Exported
// because the cli joins names into filesystem paths (and one RemoveAll)
// before Load ever runs.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("profile name must look like %q, got %q", "fish-shell", name)
	}
	return nil
}

// Load reads and validates the profile at profilesDir/name.
func Load(profilesDir, name string) (Profile, error) {
	if err := ValidateName(name); err != nil {
		return Profile{}, err
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
