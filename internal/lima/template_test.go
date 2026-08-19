package lima

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/profile"
)

func testConfig() config.Config {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	c.ExtraMounts = []string{"/home/st/work/other"}
	c.ExtraDomains = []string{"registry.example.com"}
	return c
}

// The driver is pinned rather than taken from the host, so rendering
// is identical on a Linux and a macOS checkout.
func testParams(files []guest.DataFile) RenderParams {
	return RenderParams{
		AgentUser: "devuser", AgentUID: 1000, AgentGID: 1000,
		VMType: config.VMTypeQEMU, DataFiles: files,
	}
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

// Both supported drivers must render, because the same template serves a Linux
// host on QEMU/KVM and a macOS host on vz/HVF. Only virtiofs pairs with either,
// so the mount type must not vary with the driver.
func TestRenderPinsTheResolvedVMType(t *testing.T) {
	for _, vmType := range []string{config.VMTypeQEMU, config.VMTypeVZ} {
		t.Run(vmType, func(t *testing.T) {
			p := testParams(nil)
			p.VMType = vmType
			out, err := Render(testConfig(), p)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if want := `vmType: "` + vmType + `"`; !strings.Contains(out, want) {
				t.Errorf("rendered template missing %q", want)
			}
			if !strings.Contains(out, "mountType: virtiofs") {
				t.Error("virtiofs is what preserves host UIDs and is available under both drivers; it must not vary")
			}
		})
	}
}

// Leaving the driver to Lima's default would silently pick one the mount type
// may not pair with, so an unresolved value is a render-time error rather than
// an empty field in the instance file.
func TestRenderRejectsUnresolvedVMType(t *testing.T) {
	for _, vmType := range []string{"", "hvf", "vmware"} {
		p := testParams(nil)
		p.VMType = vmType
		if _, err := Render(testConfig(), p); err == nil {
			t.Errorf("Render with VMType %q = nil error, want a failure", vmType)
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

// Lima evaluates mode:data content as a Go template with no opt-out. Content
// that is not a valid Go template fails that parse and triggers a "Couldn't
// process data content" warning on every limactl invocation, so every "{{" must
// be escaped as {{"{{"}} — which Lima's engine renders back to the original.
//
// No shipped asset contains "{{" today; this keeps the guarantee for the next
// one that does, and pins the round trip so the escaping stays lossless.
func TestRenderEscapesGuestTemplateSyntax(t *testing.T) {
	files := []guest.DataFile{{
		Path:        "/usr/local/lib/sandbox/example.sh",
		Permissions: "0755",
		Content:     "#!/bin/bash\n{{- range $k, $v := .Values -}}\n{{$k}}={{$v}}\n{{end -}}\n",
	}}
	out, err := Render(testConfig(), testParams(files))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `{{"{{"}}- range $k, $v := .Values -}}`) {
		t.Error("action openers must be escaped for Lima's template engine")
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

func testProfiles() []profile.Profile {
	return []profile.Profile{{
		Name: "fixture",
		Manifest: profile.Manifest{
			Description: "golden fixture",
			Packages:    []string{"fish"},
			Shell:       "/usr/bin/fish",
			Domains:     []string{"raw.githubusercontent.com"},
			Hook:        "hook.sh",
		},
		Files: []profile.File{{Rel: ".claude/CLAUDE.md", Content: []byte("# rules\n")}},
		Hook:  []byte("#!/bin/bash\necho hook\n"),
	}}
}

func TestRenderMatchesGolden(t *testing.T) {
	profs := testProfiles()
	files := append(profile.GuestFiles(profs), guest.DataFile{
		Path: "/usr/local/lib/sandbox/example.sh", Permissions: "0755", Content: "#!/bin/bash\necho hi\n",
	})
	p := testParams(files)
	p.AllowDomains = profile.AllowDomains(testConfig().ExtraDomains, profs)
	out, err := Render(testConfig(), p)
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

// provision.env carries the merged allowlist (config extraDomains plus
// profile domains) when the caller resolves one; a nil slice preserves the
// pre-profile behavior of using the config's extraDomains directly.
func TestRenderAllowDomains(t *testing.T) {
	p := testParams(nil)
	p.AllowDomains = []string{"registry.example.com", "raw.githubusercontent.com"}
	out, err := Render(testConfig(), p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `EXTRA_ALLOWED_DOMAINS="registry.example.com raw.githubusercontent.com"`
	if !strings.Contains(out, want) {
		t.Errorf("rendered template missing %q", want)
	}

	out, err = Render(testConfig(), testParams(nil))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, `EXTRA_ALLOWED_DOMAINS="registry.example.com"`) {
		t.Error("nil AllowDomains must fall back to the config's extraDomains")
	}
}
