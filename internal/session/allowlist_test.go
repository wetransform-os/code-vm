package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

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
	joined := strings.Join(args, " ")
	for match, content := range f.out {
		if strings.Contains(joined, match) {
			return content, nil
		}
	}
	return nil, nil
}

func (f *fakeRunner) ranAny(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func testDeps(t *testing.T, r lima.Runner) Deps {
	t.Helper()
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	return Deps{Client: lima.Client{R: r}, Config: c, AgentUser: "devuser"}
}

func fragmentPath() string { return fragmentDir + "/" + HostFragmentName }

func TestFragmentContentEmitsOneACLPerDomain(t *testing.T) {
	got := FragmentContent([]string{"a.example", ".b.example"})
	if !strings.Contains(got, "acl allowed_domains dstdomain a.example\n") {
		t.Error("missing ACL line for a.example")
	}
	if !strings.Contains(got, "acl allowed_domains dstdomain .b.example\n") {
		t.Error("missing ACL line for .b.example")
	}
}

// The fragment name is shared with init-firewall.sh, which writes it at boot.
// A rename on one side without the other would leave two fragments live.
func TestHostFragmentNameSortsAfterBase(t *testing.T) {
	if HostFragmentName <= "00-base.conf" {
		t.Errorf("HostFragmentName = %q, must sort after 00-base.conf", HostFragmentName)
	}
	if !strings.HasSuffix(HostFragmentName, ".conf") {
		t.Errorf("HostFragmentName = %q, must match the wildcard include", HostFragmentName)
	}
}

func TestApplyAllowlistInstallsFragmentAndReloadsSquid(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	d.Config.ExtraDomains = []string{"registry.example.com"}
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if !r.ranAny("copy") {
		t.Error("fragment must be copied into the guest")
	}
	if !r.ranAny("install -m 0444 -o root -g root") {
		t.Error("fragment must be installed root-owned and read-only")
	}
	if !r.ranAny("squid -k reconfigure") {
		t.Error("Squid must be reloaded after the allowlist changes")
	}
}

func TestApplyAllowlistSkipsReloadWhenUnchanged(t *testing.T) {
	existing := FragmentContent([]string{"registry.example.com"})
	r := &fakeRunner{out: map[string][]byte{fragmentPath(): []byte(existing)}}
	d := testDeps(t, r)
	d.Config.ExtraDomains = []string{"registry.example.com"}
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if r.ranAny("squid -k reconfigure") {
		t.Error("Squid must not be reloaded when the fragment is unchanged")
	}
}

func TestApplyAllowlistNoDomainsAndNoFragmentIsNoOp(t *testing.T) {
	r := &fakeRunner{}
	d := testDeps(t, r)
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if r.ranAny("copy") || r.ranAny("squid -k reconfigure") {
		t.Errorf("expected only the state read, got %v", r.calls)
	}
}

// Removing the last domain from the config has to take effect, or a revoked
// domain would stay allowed for the rest of the VM's lifetime.
func TestApplyAllowlistRemovesFragmentWhenDomainsCleared(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{
		fragmentPath(): []byte(FragmentContent([]string{"registry.example.com"})),
	}}
	d := testDeps(t, r)
	d.Config.ExtraDomains = nil
	if err := ApplyAllowlist(context.Background(), d); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if !r.ranAny("rm -f " + fragmentPath()) {
		t.Errorf("stale fragment must be removed, got %v", r.calls)
	}
	if !r.ranAny("squid -k reconfigure") {
		t.Error("Squid must be reloaded after removing the fragment")
	}
}

// The workspace is agent-writable, so a domain file there must never influence
// the allowlist. Deps has no workspace field at all, which makes this
// structurally true; the guard here is against a reimplementation that reaches
// for the working directory instead.
func TestApplyAllowlistIgnoresDomainFilesInTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".sandbox-domains", "sandbox-domains", ".code-vm-domains"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("attacker.example\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Chdir(dir)

	r := &fakeRunner{}
	if err := ApplyAllowlist(context.Background(), testDeps(t, r)); err != nil {
		t.Fatalf("ApplyAllowlist: %v", err)
	}
	if r.ranAny("copy") || r.ranAny("attacker.example") {
		t.Errorf("files in the working directory must never reach the allowlist, got %v", r.calls)
	}
}
