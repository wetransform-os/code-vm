package profile

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
)

// refRe matches exactly the two placeholder forms templates may use. The name
// charset mirrors ValidateName, so anything else — Maven properties,
// ${env.FOO} — is left untouched by both the scanner and the renderer.
var refRe = regexp.MustCompile(`\$\{(secret|var):([a-zA-Z0-9][a-zA-Z0-9-]{0,62})\}`)

// Ref is one placeholder occurrence kind+name.
type Ref struct {
	Kind string // "secret" or "var"
	Name string
}

// FindRefs returns the distinct placeholder references in content, in first-
// appearance order.
func FindRefs(content []byte) []Ref {
	seen := map[Ref]bool{}
	var out []Ref
	for _, m := range refRe.FindAllSubmatch(content, -1) {
		r := Ref{Kind: string(m[1]), Name: string(m[2])}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// Rendered is one template after substitution, destined for the agent home.
type Rendered struct {
	Rel     string
	Content []byte
}

// RenderTemplates substitutes secret and var values into every active
// profile's templates. Later profiles win Rel collisions, matching the files/
// rule. Values are opaque bytes: no escaping layer, exactly as the spec
// states — a value that breaks the target format is the user's own.
func RenderTemplates(profiles []Profile, secrets, vars map[string]string) []Rendered {
	byRel := map[string][]byte{}
	for _, p := range profiles {
		for _, tpl := range p.Templates {
			byRel[tpl.Rel] = refRe.ReplaceAllFunc(tpl.Content, func(m []byte) []byte {
				sub := refRe.FindSubmatch(m)
				if string(sub[1]) == "secret" {
					return []byte(secrets[string(sub[2])])
				}
				return []byte(vars[string(sub[2])])
			})
		}
	}
	out := make([]Rendered, 0, len(byRel))
	for rel, content := range byRel {
		out = append(out, Rendered{Rel: rel, Content: content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// DeclaredSecret is one secret name unioned across the active profiles.
type DeclaredSecret struct {
	Name        string
	Profiles    []string // declaring profiles, in activation order
	Description string   // first non-empty wins
	Suggest     string   // first non-empty wins; inert display string
}

// DeclaredVar is the var analog.
type DeclaredVar struct {
	Name        string
	Profiles    []string
	Description string
}

// DeclaredSecrets unions secret declarations across profiles, sorted by name.
func DeclaredSecrets(profiles []Profile) []DeclaredSecret {
	byName := map[string]*DeclaredSecret{}
	for _, p := range profiles {
		names := make([]string, 0, len(p.Manifest.Secrets))
		for n := range p.Manifest.Secrets {
			names = append(names, n)
		}
		sort.Strings(names) // map order is random; keep Profiles deterministic
		for _, n := range names {
			spec := p.Manifest.Secrets[n]
			d, ok := byName[n]
			if !ok {
				d = &DeclaredSecret{Name: n}
				byName[n] = d
			}
			d.Profiles = append(d.Profiles, p.Name)
			if d.Description == "" {
				d.Description = spec.Description
			}
			if d.Suggest == "" {
				d.Suggest = spec.Suggest
			}
		}
	}
	out := make([]DeclaredSecret, 0, len(byName))
	for _, d := range byName {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeclaredVars unions var declarations across profiles, sorted by name.
func DeclaredVars(profiles []Profile) []DeclaredVar {
	byName := map[string]*DeclaredVar{}
	for _, p := range profiles {
		names := make([]string, 0, len(p.Manifest.Vars))
		for n := range p.Manifest.Vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			spec := p.Manifest.Vars[n]
			d, ok := byName[n]
			if !ok {
				d = &DeclaredVar{Name: n}
				byName[n] = d
			}
			d.Profiles = append(d.Profiles, p.Name)
			if d.Description == "" {
				d.Description = spec.Description
			}
		}
	}
	out := make([]DeclaredVar, 0, len(byName))
	for _, d := range byName {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CommandRunner executes a user-authored secrets.yaml command on the host and
// returns its combined output. Injectable for tests.
type CommandRunner func(ctx context.Context, command string) ([]byte, error)

// MissingSecretSnippet renders the ready-to-paste secrets.yaml block for an
// unmapped secret. The suggest hint is copied verbatim as the command —
// display only until the user adopts it by saving this snippet themselves.
func MissingSecretSnippet(d DeclaredSecret) string {
	cmd := d.Suggest
	if cmd == "" {
		cmd = "<command printing the value>"
	}
	return fmt.Sprintf("secrets:\n  %s:\n    command: %s\n", d.Name, cmd)
}

// ResolveSecrets resolves every declared secret from the user's sources. Each
// command runs exactly once per resolve pass with one trailing newline
// stripped (the gopass/pass convention). A missing mapping fails with an
// actionable snippet rather than prompting or falling back to hints: hints
// never execute.
func ResolveSecrets(ctx context.Context, declared []DeclaredSecret, sources map[string]config.SecretSource, run CommandRunner) (map[string]string, error) {
	out := make(map[string]string, len(declared))
	for _, d := range declared {
		src, ok := sources[d.Name]
		if !ok {
			desc := d.Description
			if desc == "" {
				desc = "no description"
			}
			return nil, fmt.Errorf(
				"profile %s needs secret %q (%s), but secrets.yaml does not map it.\nAdd to ~/.config/code-vm/secrets.yaml:\n\n%s",
				strings.Join(d.Profiles, ", "), d.Name, desc, MissingSecretSnippet(d))
		}
		if src.Command != "" {
			b, err := run(ctx, src.Command)
			if err != nil {
				return nil, fmt.Errorf("secret %q: command failed: %w: %s", d.Name, err, strings.TrimSpace(string(b)))
			}
			out[d.Name] = strings.TrimSuffix(string(b), "\n")
			continue
		}
		out[d.Name] = src.Value
	}
	return out, nil
}

// ResolveVars resolves declared vars from config.yaml's literal map.
func ResolveVars(declared []DeclaredVar, values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(declared))
	for _, d := range declared {
		v, ok := values[d.Name]
		if !ok {
			desc := d.Description
			if desc == "" {
				desc = "no description"
			}
			return nil, fmt.Errorf(
				"profile %s needs var %q (%s), but config.yaml does not set it.\nAdd to config.yaml:\n\nvars:\n  %s: <value>",
				strings.Join(d.Profiles, ", "), d.Name, desc, d.Name)
		}
		out[d.Name] = v
	}
	return out, nil
}
