package lima

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"

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

// findProvisionContent locates the "content" field of the provision entry
// for path in a generic YAML document, the way a real YAML consumer (Lima
// itself) would see it after parsing.
func findProvisionContent(t *testing.T, doc map[string]interface{}, path string) string {
	t.Helper()
	prov, ok := doc["provision"].([]interface{})
	if !ok {
		t.Fatalf("provision is not a list: %#v", doc["provision"])
	}
	for _, e := range prov {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if m["path"] == path {
			content, ok := m["content"].(string)
			if !ok {
				t.Fatalf("provision entry for %s has no string content: %#v", path, m)
			}
			return content
		}
	}
	t.Fatalf("no provision entry found for path %s", path)
	return ""
}

// A YAML block scalar with no explicit indentation indicator derives its
// indentation from its first non-empty line. A DataFile whose own first line
// starts with a space or a tab (a patch file, some Markdown) would shift that
// detection and corrupt the document, or fail to parse outright for a tab —
// verified by rendering with the old "content: |" header and confirming
// yaml.Unmarshal rejected it before the "content: |2" fix landed. The
// explicit indicator on the block header must make both survive: the whole
// rendered document must remain valid YAML, and the content must come back
// byte-for-byte once the guest's own per-file Go-template pass (what Lima
// runs on every `mode: data` entry) executes it.
func TestRenderHandlesContentWithLeadingWhitespace(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"leading space", " echo indented first line\n#!/bin/bash\necho normal\n"},
		{"leading tab", "\techo tabbed first line\n#!/bin/bash\necho normal\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := []guest.DataFile{{
				Path:        "/usr/local/lib/sandbox/tricky-whitespace.sh",
				Permissions: "0755",
				Content:     tc.content,
			}}
			out, err := Render(testConfig(), testParams(files))
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			var doc map[string]interface{}
			if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("rendered template is not valid YAML: %v\n--- rendered ---\n%s", err, out)
			}

			entryContent := findProvisionContent(t, doc, files[0].Path)
			tpl, err := template.New("roundtrip").Parse(entryContent)
			if err != nil {
				t.Fatalf("guest content must parse as a Go template (what Lima does in the guest), got: %v", err)
			}
			var b strings.Builder
			if err := tpl.Execute(&b, nil); err != nil {
				t.Fatalf("guest content must execute cleanly, got: %v", err)
			}
			if b.String() != tc.content {
				t.Errorf("round trip mismatch:\n--- got ---\n%q\n--- want ---\n%q", b.String(), tc.content)
			}
		})
	}
}

// Trailing-newline fidelity: `indent`'s old TrimRight(s, "\n") plus the
// template's fixed "content: |2" (clip chomping) header forced every
// embedded file through exactly one trailing newline, no matter how many it
// actually had. A file with none gained one; a file with several collapsed
// to one. `profile apply` (a byte-exact staged copy) does not have this
// problem, so the two delivery paths silently diverged. This pins the fix:
// the rendered document must stay valid YAML, and — critically — decoding
// the provision entry's content and then executing it as a Go template (the
// per-file pass Lima itself runs on every mode:data entry, see
// escapeGuestTemplate) must reproduce the original bytes exactly, trailing
// newlines included.
func TestRenderPreservesTrailingNewlineFidelity(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no trailing newline", "x"},
		{"one trailing newline", "x\n"},
		{"three trailing newlines", "x\n\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := []guest.DataFile{{
				Path:        "/usr/local/lib/sandbox/trailing.sh",
				Permissions: "0644",
				Content:     tc.content,
			}}
			out, err := Render(testConfig(), testParams(files))
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			var doc map[string]interface{}
			if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("rendered template is not valid YAML: %v\n--- rendered ---\n%s", err, out)
			}

			entryContent := findProvisionContent(t, doc, files[0].Path)
			tpl, err := template.New("roundtrip").Parse(entryContent)
			if err != nil {
				t.Fatalf("guest content must parse as a Go template, got: %v", err)
			}
			var b strings.Builder
			if err := tpl.Execute(&b, nil); err != nil {
				t.Fatalf("guest content must execute cleanly, got: %v", err)
			}
			if b.String() != tc.content {
				t.Errorf("round trip mismatch:\n--- got ---\n%q\n--- want ---\n%q", b.String(), tc.content)
			}
		})
	}
}

// A DataFile whose content is empty or consists solely of newline characters
// (a literal block scalar with no non-blank line at all) is rejected before
// it ever becomes a DataFile: package profile's isBlank check refuses such
// files and hooks at Load time, with its own tests. That rejection exists
// precisely because this case is a genuine limactl limitation, not just a
// theoretical one: confirmed against limactl 2.2.0, `limactl validate`
// outright fails to parse a rendered document containing such a block
// ("could not find multi-line content" / "mapping value is not allowed in
// this context"), regardless of which chomping indicator is used — even
// though the same document parses and round-trips correctly with
// gopkg.in/yaml.v3, which is what this test (deliberately) still uses. This
// test therefore documents a narrower claim than it might look like: our own
// template layer renders spec-valid YAML for blank content, but that is not
// sufficient for limactl's own parser, which is why profile.Load blocks it
// upstream instead of relying on this layer.
func TestRenderHandlesEmptyContent(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"totally empty", ""},
		{"single newline", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []guest.DataFile{{
				Path:        "/usr/local/lib/sandbox/empty.sh",
				Permissions: "0644",
				Content:     tc.content,
			}}
			out, err := Render(testConfig(), testParams(files))
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var doc map[string]interface{}
			if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("rendered template is not valid YAML: %v\n--- rendered ---\n%s", err, out)
			}
			entryContent := findProvisionContent(t, doc, files[0].Path)
			tpl, err := template.New("roundtrip").Parse(entryContent)
			if err != nil {
				t.Fatalf("guest content must parse as a Go template, got: %v", err)
			}
			var b strings.Builder
			if err := tpl.Execute(&b, nil); err != nil {
				t.Fatalf("guest content must execute cleanly, got: %v", err)
			}
			if b.String() != tc.content {
				t.Errorf("round trip mismatch:\n--- got ---\n%q\n--- want ---\n%q", b.String(), tc.content)
			}
		})
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
	// The whole rendered instance file must be valid YAML, not just the
	// substrings other tests grep for: a broken block scalar header (or any
	// other structural mistake) would otherwise only surface at `limactl
	// start`, against a real VM.
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n--- rendered ---\n%s", err, out)
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
