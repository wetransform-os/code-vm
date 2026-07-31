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
	DataFiles []guest.DataFile
}

// provisionEnv is delivered as a data file so the guest scripts read their
// inputs from one well-known place instead of relying on Lima passing env.
func provisionEnv(c config.Config, p RenderParams) guest.DataFile {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by code-vm. Sourced by the guest provisioning scripts.\n")
	fmt.Fprintf(&b, "AGENT_USER=%s\n", p.AgentUser)
	fmt.Fprintf(&b, "AGENT_UID=%d\n", p.AgentUID)
	fmt.Fprintf(&b, "AGENT_GID=%d\n", p.AgentGID)
	fmt.Fprintf(&b, "EXTRA_ALLOWED_DOMAINS=%q\n", strings.Join(c.ExtraDomains, " "))
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

// indent prefixes every non-empty line with n spaces. YAML block scalars
// reproduce content verbatim, so this is all that is needed to embed
// arbitrary script bodies safely.
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// Render produces the Lima instance YAML for the given config.
func Render(c config.Config, p RenderParams) (string, error) {
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("invalid config: %w", err)
	}
	raw, err := guest.LimaTemplate()
	if err != nil {
		return "", err
	}
	tpl, err := template.New("lima").Funcs(template.FuncMap{
		"indent":     indent,
		"escapeData": escapeGuestTemplate,
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
		DataFiles []guest.DataFile
	}{c, p.AgentUser, p.AgentUID, p.AgentGID, files}

	var out strings.Builder
	if err := tpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render Lima template: %w", err)
	}
	return out.String(), nil
}
