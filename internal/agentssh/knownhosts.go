package agentssh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// publicKeyLine is one public key as ssh-keygen writes it: a key type, the
// base64 blob, and an optional comment. The type's shape ("ssh-", "ecdsa-",
// "sk-" and the like) is what tells it from a known_hosts entry, whose first
// field is a marker ("@cert-authority"), a pattern ("*") or a host.
var publicKeyLine = regexp.MustCompile(`^([a-z][a-z0-9]*-[a-z0-9.@-]+) ([A-Za-z0-9+/]+={0,2})(?: [^\n]*)?$`)

// hostKeyTypes are the host key types a current OpenSSH server offers. Only
// these are pinned; a well-formed key of any other type (DSA from an old
// image, a type newer than this list) is skipped, so one odd key never
// leaves every client on a stale pin.
var hostKeyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// ParseHostKeys reduces the concatenated contents of the guest's
// /etc/ssh/ssh_host_*_key.pub files to "<type> <key>" entries, dropping the
// comments. Every non-blank line must be a bare public key: the result is
// written into a known_hosts file, where a stray "@cert-authority" marker or
// a wildcard pattern would widen what the client trusts. Keys of a type not
// in hostKeyTypes are skipped, not copied; at least one must remain.
func ParseHostKeys(raw string) ([]string, error) {
	var keys []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := publicKeyLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("guest host key line is not a bare public key: %q", line)
		}
		if hostKeyTypes[m[1]] {
			keys = append(keys, m[1]+" "+m[2])
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("the guest returned no SSH host keys of a supported type")
	}
	return keys, nil
}

// KnownHostsContent renders known_hosts entries pinning keys for the guest's
// sshd as the host reaches it: 127.0.0.1 on each of ports. Zero and repeated
// ports are skipped, so callers can pass "the configured port and the live
// one" without caring whether they coincide or are known.
func KnownHostsContent(ports []int, keys []string) string {
	var b strings.Builder
	seen := map[int]bool{}
	for _, p := range ports {
		if p == 0 || seen[p] {
			continue
		}
		seen[p] = true
		for _, k := range keys {
			fmt.Fprintf(&b, "[127.0.0.1]:%d %s\n", p, k)
		}
	}
	return b.String()
}

// WriteKnownHosts replaces the file at path with content, atomically and
// 0600 like the private key beside it. changed reports that the file held
// keys and they differ from keys: the one case the caller should tell the
// user about, because every client that pinned the old key is about to be
// refused (or, after this write, accepted again). A first write, a file
// without keys, or the same keys under other hosts (a port Lima picked
// afresh, an entry a client hashed) is not a change of trust.
func WriteKnownHosts(path, content string, keys []string) (changed bool, err error) {
	// A symlink, live or dangling, is refused before anything is read or
	// written, as with the key files: the pin must be a real file here.
	present, err := exists(path)
	if err != nil {
		return false, err
	}
	if present {
		old, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", path, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", path, err)
		}
		// Current content in a private file: nothing to do. A permissive
		// file is replaced even so, to end up 0600 as documented.
		if string(old) == content && fi.Mode().Perm() == 0o600 {
			return false, nil
		}
		if had := knownHostsKeys(string(old)); len(had) > 0 && !sameKeys(had, keys) {
			changed = true
		}
	}
	// CreateTemp makes the file 0600, so no chmod is needed.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*")
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}
	return changed, nil
}

// knownHostsKeys returns the "<type> <key>" pairs a known_hosts file holds,
// whatever host, marker or hashing each line carries: the key fields are the
// first that look like a public key.
func knownHostsKeys(content string) []string {
	var keys []string
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		for i := 0; i+1 < len(f); i++ {
			if publicKeyLine.MatchString(f[i] + " " + f[i+1]) {
				keys = append(keys, f[i]+" "+f[i+1])
				break
			}
		}
	}
	return keys
}

// sameKeys reports whether a and b hold the same set of keys.
func sameKeys(a, b []string) bool {
	set := func(keys []string) map[string]bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}
