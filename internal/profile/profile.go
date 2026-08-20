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

// isBlank reports whether content is empty or consists solely of newline
// characters: rendered as a Lima "content: |" literal block scalar, such
// content has no non-blank line at all. That is valid per the YAML spec and
// parses fine with gopkg.in/yaml.v3, but was confirmed against limactl
// 2.2.0's own parser (`limactl validate`) to fail outright regardless of
// chomping indicator — "could not find multi-line content" / "mapping value
// is not allowed in this context" — which would only surface at `code-vm
// start`, against a real VM. Rejected here instead, at load time.
func isBlank(content []byte) bool {
	return len(bytes.Trim(content, "\n")) == 0
}

// forbiddenFiles are agent-home paths a profile may never ship: the
// security-critical files lock-settings.sh owns and locks.
var forbiddenFiles = map[string]bool{
	".claude/settings.json":       true,
	".claude/settings.local.json": true,
}

// SecretSpec declares a secret the profile requires.
type SecretSpec struct {
	Description string `yaml:"description"`
	Suggest     string `yaml:"suggest"`
}

// VarSpec declares a variable the profile requires.
type VarSpec struct {
	Description string `yaml:"description"`
}

// Manifest is the profile.yaml schema. Every key is optional, but a profile
// that declares nothing at all is rejected.
type Manifest struct {
	Description string                `yaml:"description"`
	Packages    []string              `yaml:"packages"`
	Shell       string                `yaml:"shell"`
	Domains     []string              `yaml:"domains"`
	Hook        string                `yaml:"hook"`
	Secrets     map[string]SecretSpec `yaml:"secrets"`
	Vars        map[string]VarSpec    `yaml:"vars"`
}

// File is one file a profile ships into the agent home.
type File struct {
	Rel        string // cleaned forward-slash path relative to the agent home
	Content    []byte
	Executable bool
}

// Profile is a loaded, validated bundle.
type Profile struct {
	Name      string
	Dir       string
	Manifest  Manifest
	Files     []File // sorted by Rel
	Templates []File // sorted by Rel
	Hook      []byte // nil when the manifest declares no hook
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
	// Lstat, not Stat: a symlinked profile directory could point anywhere on
	// the host (including back into an agent-writable mounted workspace),
	// letting a sandboxed agent rewrite what the next host invocation treats
	// as trusted profile input. No symlinks are permitted anywhere in a
	// bundle; this is the directory-level half of that rule.
	if fi, err := os.Lstat(dir); err != nil {
		return Profile{}, fmt.Errorf("profile %s: read manifest: %w", name, err)
	} else if !fi.IsDir() {
		return Profile{}, fmt.Errorf("profile %s: profile directory must be a real directory (symlinks are rejected)", name)
	}
	manifestPath := filepath.Join(dir, "profile.yaml")
	// Lstat before ReadFile for the same reason: a symlinked manifest could
	// be swapped between install-time validation and a later load, feeding
	// agent-authored domains, packages, or shell settings back into the
	// trusted host path.
	if fi, err := os.Lstat(manifestPath); err != nil {
		return Profile{}, fmt.Errorf("profile %s: read manifest: %w", name, err)
	} else if !fi.Mode().IsRegular() {
		return Profile{}, fmt.Errorf("profile %s: profile.yaml must be a regular file (symlinks are rejected)", name)
	}
	data, err := os.ReadFile(manifestPath)
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
	if p.Templates, err = loadTemplates(dir); err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", name, err)
	}
	// Check for collisions between files/ and templates/.
	fileRels := map[string]bool{}
	for _, f := range p.Files {
		fileRels[f.Rel] = true
	}
	for _, tpl := range p.Templates {
		if fileRels[tpl.Rel] {
			return Profile{}, fmt.Errorf("profile %s: %s is shipped by both files/ and templates/; pick one", name, tpl.Rel)
		}
	}
	if m.Hook != "" {
		hookPath := filepath.Join(dir, m.Hook)
		// Lstat, not Stat: a hostile bundle could symlink its hook at a file
		// the installing user can read but the profile author cannot (or at
		// content that changes between validation and delivery). Following
		// it would deliver that file world-readable (0555) into the guest.
		fi, err := os.Lstat(hookPath)
		if err != nil {
			return Profile{}, fmt.Errorf("profile %s: read hook: %w", name, err)
		}
		if !fi.Mode().IsRegular() {
			return Profile{}, fmt.Errorf("profile %s: hook must be a regular file (symlinks are rejected)", name)
		}
		b, err := os.ReadFile(hookPath)
		if err != nil {
			return Profile{}, fmt.Errorf("profile %s: read hook: %w", name, err)
		}
		if isBlank(b) {
			return Profile{}, fmt.Errorf("profile %s: hook must not be blank (empty or newline-only content cannot be embedded)", name)
		}
		p.Hook = b
	}
	if len(p.Files) == 0 && len(m.Packages) == 0 && m.Shell == "" && len(m.Domains) == 0 && m.Hook == "" && len(p.Templates) == 0 && len(m.Secrets) == 0 && len(m.Vars) == 0 {
		return Profile{}, fmt.Errorf("profile %s: declares nothing: no files, packages, shell, domains, hook, templates, secrets or vars", name)
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
	return nil
}

// loadTree reads a tree from dir/subdir. Only regular files are accepted: a symlink
// could escape the tree on the host, or change content between validation and
// delivery. subdir is used as the prefix in error messages and the subdirectory name.
func loadTree(dir, subdir string) ([]File, error) {
	root := filepath.Join(dir, subdir)
	// Lstat, not Stat: a symlinked root would let WalkDir walk
	// wherever it points, including a directory an agent controls.
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsDir() {
		// Folds in the former "subdir is a regular file" case: a non-directory
		// subdir makes WalkDir yield a single entry with rel ".", which
		// passes every check below (it is a regular file, it matches
		// relPathRe, it is not forbidden) and would otherwise be silently
		// installed as-is. A symlinked subdir (to a directory or otherwise)
		// is rejected the same way, by the same message.
		return nil, fmt.Errorf("%s must be a real directory (symlinks are rejected)", subdir)
	}
	var out []File
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
			return fmt.Errorf("%s/%s: only regular files may be shipped (symlinks are rejected)", subdir, rel)
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
			return fmt.Errorf("%s/%s: profiles may not ship the locked Claude settings", subdir, rel)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if isBlank(b) {
			return fmt.Errorf("%s/%s: must not be blank (empty or newline-only content cannot be embedded)", subdir, rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, File{Rel: rel, Content: b, Executable: info.Mode()&0o111 != 0})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}

// loadFiles reads the files/ tree. Only regular files are accepted: a symlink
// could escape the tree on the host, or change content between validation and
// delivery.
func loadFiles(dir string) ([]File, error) {
	return loadTree(dir, "files")
}

// loadTemplates reads the templates/ tree. Only regular files are accepted: a symlink
// could escape the tree on the host, or change content between validation and
// delivery.
func loadTemplates(dir string) ([]File, error) {
	return loadTree(dir, "templates")
}
