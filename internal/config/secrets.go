package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
