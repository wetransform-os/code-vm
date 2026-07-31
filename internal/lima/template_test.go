package lima

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
)

func testConfig() config.Config {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	c.ExtraMounts = []string{"/home/st/work/other"}
	c.ExtraDomains = []string{"registry.example.com"}
	return c
}

func testParams(files []guest.DataFile) RenderParams {
	return RenderParams{AgentUser: "devuser", AgentUID: 1000, AgentGID: 1000, DataFiles: files}
}

func TestRenderSecurityInvariants(t *testing.T) {
	out, err := Render(testConfig(), testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "_default/mounts") {
		t.Error("template must not inherit _default/mounts: it exposes host $HOME")
	}
	if strings.Contains(out, "reverse-sshfs") {
		t.Error("reverse-sshfs breaks UID-matched ownership and must not appear")
	}
	if strings.Contains(out, "host.docker.internal") {
		t.Error("host.docker.internal must not be defined: the agent has no reason to reach host services")
	}
	if strings.Contains(out, "docker.sock") {
		t.Error("the Docker socket must not be forwarded to the host")
	}
	for _, want := range []string{
		"minimumLimaVersion: 2.2.0",
		"mountType: virtiofs",
		"name: limaadmin",
		"uid: 60000",
		"loadDotSSHPubKeys: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered template missing %q", want)
		}
	}
}

func TestRenderMountsAndSizing(t *testing.T) {
	c := testConfig()
	c.CPUs = 8
	c.Memory = "16GiB"
	out, err := Render(c, testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"cpus: 8",
		`memory: "16GiB"`,
		`location: "/home/st/projects"`,
		`mountPoint: "/home/st/projects"`,
		`location: "/home/st/work/other"`,
		"writable: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered template missing %q", want)
		}
	}
}

// Script bodies are embedded as YAML block scalars. Content with quotes,
// colons and backslashes must survive indentation without corrupting the
// document, so this asserts the indent helper is applied.
func TestRenderIndentsDataFileContent(t *testing.T) {
	files := []guest.DataFile{{
		Path:        "/usr/local/lib/sandbox/tricky.sh",
		Permissions: "0755",
		Content:     "#!/bin/bash\nkey: \"value\"\nprintf '%s\\n' \"a: b\"\n",
	}}
	out, err := Render(testConfig(), testParams(files))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "\n    #!/bin/bash\n") {
		t.Error("data file content must be indented under the content block scalar")
	}
	if !strings.Contains(out, "\n    printf '%s\\n' \"a: b\"\n") {
		t.Error("data file content must be reproduced verbatim inside the block scalar")
	}
}

// Lima evaluates mode:data content as a Go template with no opt-out. Raw
// gomplate syntax fails its parse and triggers a "Couldn't process data
// content" warning on every limactl invocation, so every "{{" must be
// escaped as {{"{{"}} — which Lima's engine renders back to the original.
func TestRenderEscapesGuestTemplateSyntax(t *testing.T) {
	files := []guest.DataFile{{
		Path:        "/usr/local/share/sandbox-templates/example.tpl",
		Permissions: "0444",
		Content:     "{{- range $k, $v := (ds \"ctx\").secrets -}}\n{{$k}}={{$v}}\n{{end -}}\n",
	}}
	out, err := Render(testConfig(), testParams(files))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `{{"{{"}}- range $k, $v := (ds "ctx").secrets -}}`) {
		t.Error("gomplate action openers must be escaped for Lima's template engine")
	}
	if strings.Contains(out, "\n    {{- range") {
		t.Error("raw {{ must not survive into a data entry: Lima warns on every invocation")
	}

	// Round-trip: executing the escaped content as a Go template (what Lima
	// does in the guest) must reproduce the original bytes.
	escaped := escapeGuestTemplate(files[0].Content)
	tpl, err := template.New("roundtrip").Parse(escaped)
	if err != nil {
		t.Fatalf("escaped content must parse as a Go template, got: %v", err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, nil); err != nil {
		t.Fatalf("escaped content must execute cleanly, got: %v", err)
	}
	if b.String() != files[0].Content {
		t.Errorf("round trip mismatch:\n--- got ---\n%s\n--- want ---\n%s", b.String(), files[0].Content)
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	out, err := Render(testConfig(), testParams([]guest.DataFile{{
		Path: "/usr/local/lib/sandbox/example.sh", Permissions: "0755", Content: "#!/bin/bash\necho hi\n",
	}}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	golden := filepath.Join("testdata", "golden-template.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if out != string(want) {
		t.Errorf("rendered template differs from golden file.\nRegenerate with UPDATE_GOLDEN=1 go test ./internal/lima/ after reviewing the diff.\n--- got ---\n%s", out)
	}
}
