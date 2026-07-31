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
