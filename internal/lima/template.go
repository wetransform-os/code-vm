// Package lima renders the sandbox Lima instance and drives limactl.
package lima

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
)

// InstanceName is the Lima instance code-vm manages.
const InstanceName = "code-sandbox"

// RenderParams carries the host-derived values the template needs.
type RenderParams struct {
	AgentUser string
	AgentUID  int
	AgentGID  int
	// VMType is the resolved Lima driver, not the raw config value: which
	// hypervisor is usable depends on the host OS, which is host-derived and
	// therefore does not belong in Render. See config.ResolveVMType.
	VMType    string
	DataFiles []guest.DataFile
	// AllowDomains is the merged egress allowlist: config extraDomains plus
	// active profile domains. nil means just the config's extraDomains, so a
	// caller without profiles renders identically to before profiles existed.
	AllowDomains []string
}

// provisionEnv is delivered as a data file so the guest scripts read their
// inputs from one well-known place instead of relying on Lima passing env.
func provisionEnv(c config.Config, p RenderParams) guest.DataFile {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by code-vm. Sourced by the guest provisioning scripts.\n")
	fmt.Fprintf(&b, "AGENT_USER=%s\n", p.AgentUser)
	fmt.Fprintf(&b, "AGENT_UID=%d\n", p.AgentUID)
	fmt.Fprintf(&b, "AGENT_GID=%d\n", p.AgentGID)
	domains := p.AllowDomains
	if domains == nil {
		domains = c.ExtraDomains
	}
	fmt.Fprintf(&b, "EXTRA_ALLOWED_DOMAINS=%q\n", strings.Join(domains, " "))
	fmt.Fprintf(&b, "CONTAINER_PROXY=%t\n", c.ContainerProxy)
	return guest.DataFile{Path: "/etc/sandbox/provision.env", Permissions: "0444", Content: b.String()}
}

// escapeGuestTemplate makes file content safe for Lima's data entries. Lima
// evaluates every `mode: data` content as a Go template (for {{.UID}}-style
// guest variables) with no opt-out; raw gomplate templates fail that parse and
// Lima prints a "Couldn't process data content" warning on every invocation
// before falling back to the raw bytes. Escaping each "{{" as {{"{{"}} lets
// Lima's engine parse and execute cleanly, reproducing the original bytes.
func escapeGuestTemplate(s string) string {
	return strings.ReplaceAll(s, "{{", `{{"{{"}}`)
}

// guestBlockContent prepares DataFile content for embedding in a "content:
// |2" block scalar: escapeGuestTemplate's usual guard, plus a defense against
// a limactl-side bug found while testing this template against `limactl
// template copy --embed-all` (the exact call ResolveConfigInto makes, and
// what `limactl start` does internally for our `base:`-based template). That
// merge step mis-renders a literal block scalar whose first line's very first
// byte is a tab: the explicit indentation indicator does not help, and
// neither does indentation depth — confirmed corrupting the document even
// though the same content is fine one line down, or led by a space instead.
// Prefixing a Go template comment (which Lima's own per-file template pass
// evaluates to nothing, restoring the original bytes) moves that byte off
// column one whenever it would land there, sidestepping the bug rather than
// working around it blind.
func guestBlockContent(s string) string {
	escaped := escapeGuestTemplate(s)
	if len(escaped) > 0 && (escaped[0] == ' ' || escaped[0] == '\t') {
		escaped = "{{/* code-vm: content follows */}}" + escaped
	}
	return escaped
}

// trailingNewlines counts s's trailing "\n" bytes.
func trailingNewlines(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\n'; i-- {
		n++
	}
	return n
}

// chomp returns the YAML block scalar chomping indicator that reproduces s's
// trailing newlines exactly: clip (no indicator) keeps exactly one, so it is
// only correct when s ends with exactly one "\n". Content with none needs
// strip ("-"); content with more than one needs keep ("+"), paired with
// indent below reproducing the extra ones as trailing blank lines. Without
// this, every embedded file gained or lost trailing newlines relative to its
// source: profile apply (a byte-exact staged copy) and the boot path (this
// template) would silently diverge on trailing-newline count alone.
//
// Known limitation, confirmed against limactl 2.2.0 rather than assumed:
// `limactl template copy --embed-all` (what ResolveConfigInto and `limactl
// start` both run for our base-inherited template, same as the tab bug
// documented on guestBlockContent) re-serializes the whole document and does
// not preserve the "+" (keep) indicator — it silently rewrites those block
// scalars back to clip, collapsing 2+ trailing newlines down to exactly one.
// Content with 0 or 1 trailing newlines (the overwhelming majority of real
// files) round-trips correctly through that step; content with 2+ does not,
// purely because of that downstream rewrite. This is strictly better than
// before the fix (which forced every file through exactly one trailing
// newline regardless of chomping), and is left as-is per the existing
// guard-only-what-we-can policy: it is limactl's own re-serialization that is
// lossy here, not this template's rendering, which validates and round-trips
// correctly prior to that step.
func chomp(s string) string {
	switch trailingNewlines(s) {
	case 0:
		return "-"
	case 1:
		if s == "\n" {
			// A block scalar with no non-blank line at all has nothing for
			// clip's implicit "keep exactly one final line break" to anchor
			// on: a wholly-blank block collapses to "" under both clip and
			// strip (confirmed against yaml.v3). Only keep reproduces the
			// single blank line's own newline.
			return "+"
		}
		return ""
	default:
		return "+"
	}
}

// indent prefixes every non-empty line with n spaces. YAML block scalars
// reproduce content verbatim, so together with an explicit indentation
// indicator on the block header (see code-sandbox.yaml.tpl's "content: |2")
// and guestBlockContent's tab guard, this is all that is needed to embed
// arbitrary file content safely, including content whose own first line
// starts with a space or tab.
//
// Pairs with chomp: chomp's "keep" indicator ("+") preserves the block's
// final line break plus every trailing blank line that follows it in the
// document, so when s has more than one trailing "\n", the extra ones (all
// but the last, which the template's own line break after this block
// supplies) are reproduced here as blank lines. Blank lines need no
// indentation in a YAML literal block scalar.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	trailing := trailingNewlines(s)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	out := strings.Join(lines, "\n")
	if trailing > 1 {
		out += strings.Repeat("\n", trailing-1)
	}
	return out
}

// Render produces the Lima instance YAML for the given config.
func Render(c config.Config, p RenderParams) (string, error) {
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("invalid config: %w", err)
	}
	// An empty driver would render `vmType: ""` and hand Lima's own default
	// back the choice the mount type depends on, so callers must resolve it.
	if p.VMType == "" {
		return "", fmt.Errorf("unresolved vmType %q: resolve it with config.ResolveVMType before rendering", p.VMType)
	}
	if err := config.ValidateVMType(p.VMType); err != nil {
		return "", fmt.Errorf("invalid vmType %q: %w", p.VMType, err)
	}
	raw, err := guest.LimaTemplate()
	if err != nil {
		return "", err
	}
	tpl, err := template.New("lima").Funcs(template.FuncMap{
		"indent":     indent,
		"escapeData": guestBlockContent,
		"chomp":      chomp,
	}).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse Lima template: %w", err)
	}
	files := append([]guest.DataFile{provisionEnv(c, p)}, p.DataFiles...)
	data := struct {
		Config    config.Config
		AgentUser string
		AgentUID  int
		AgentGID  int
		VMType    string
		DataFiles []guest.DataFile
	}{c, p.AgentUser, p.AgentUID, p.AgentGID, p.VMType, files}

	var out strings.Builder
	if err := tpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render Lima template: %w", err)
	}
	return out.String(), nil
}
