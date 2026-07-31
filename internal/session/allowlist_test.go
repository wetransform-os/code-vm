package session

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

func TestReadDomains(t *testing.T) {
	dir := t.TempDir()
	body := "# a comment\n\nregistry.example.com\n  .internal.example  \nregistry.example.com\n"
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ReadDomains(dir)
	if err != nil {
		t.Fatalf("ReadDomains: %v", err)
	}
	want := []string{"registry.example.com", ".internal.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadDomains = %v, want %v (comments and blanks dropped, trimmed, de-duplicated in first-seen order)", got, want)
	}
}

func TestReadDomainsMissingFileIsEmpty(t *testing.T) {
	got, err := ReadDomains(t.TempDir())
	if err != nil {
		t.Fatalf("ReadDomains: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadDomains = %v, want empty", got)
	}
}

func TestFragmentNameIsStableAndWorkspaceSpecific(t *testing.T) {
	a := FragmentName("/home/st/projects/one")
	b := FragmentName("/home/st/projects/two")
	if a != FragmentName("/home/st/projects/one") {
		t.Error("FragmentName must be stable for the same workspace")
	}
	if a == b {
		t.Error("different workspaces must get different fragments")
	}
	if !strings.HasPrefix(a, "10-") || !strings.HasSuffix(a, ".conf") {
		t.Errorf("FragmentName = %q, want 10-<hash>.conf", a)
	}
}

func TestFragmentContentEmitsOneACLPerDomain(t *testing.T) {
	got := FragmentContent("/home/st/projects/one", []string{"a.example", ".b.example"})
	if !strings.Contains(got, "acl allowed_domains dstdomain a.example\n") {
		t.Error("missing ACL line for a.example")
	}
	if !strings.Contains(got, "acl allowed_domains dstdomain .b.example\n") {
		t.Error("missing ACL line for .b.example")
	}
	if !strings.Contains(got, "/home/st/projects/one") {
		t.Error("fragment should record which workspace it came from")
	}
}

type fakeRunner struct {
	calls [][]string
	out   map[string][]byte
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out[strings.Join(args, " ")], nil
}

func (f *fakeRunner) ranAny(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func testDeps(t *testing.T, r lima.Runner, ws string) Deps {
	t.Helper()
	c := config.Default()
	c.ProjectsRoot = filepath.Dir(ws)
	return Deps{Client: lima.Client{R: r}, Config: c, Workspace: ws, AgentUser: "devuser"}
}

func TestApplyAllowlistInstallsFragmentAndReloadsSquid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte("registry.example.com\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := &fakeRunner{}
	if err := ApplyAllowlist(context.Background(), testDeps(t, r, dir)); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if !r.ranAny("copy") {
		t.Error("fragment must be copied into the guest")
	}
	if !r.ranAny("install") {
		t.Error("fragment must be installed into the allowlist directory as root")
	}
	if !r.ranAny("squid -k reconfigure") {
		t.Error("Squid must be reloaded after the allowlist changes")
	}
}

func TestApplyAllowlistSkipsReloadWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sandbox-domains"), []byte("registry.example.com\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	d := testDeps(t, nil, dir)
	existing := FragmentContent(dir, []string{"registry.example.com"})
	r := &fakeRunner{out: map[string][]byte{
		strings.Join(lima.Client{}.AdminArgs([]string{"cat", "/run/sandbox/squid-allow.d/" + FragmentName(dir)}), " "): []byte(existing),
	}}
	d.Client = lima.Client{R: r}
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if r.ranAny("squid -k reconfigure") {
		t.Error("Squid must not be reloaded when the fragment is unchanged")
	}
}

func TestApplyAllowlistNoDomainsIsNoOp(t *testing.T) {
	r := &fakeRunner{}
	if err := ApplyAllowlist(context.Background(), testDeps(t, r, t.TempDir())); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no guest calls without a .sandbox-domains file, got %v", r.calls)
	}
}
