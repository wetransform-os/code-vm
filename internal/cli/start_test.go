package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
)

type recordingRunner struct {
	statusOut string
	calls     [][]string
}

func (r *recordingRunner) Run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, args)
	return nil
}

func (r *recordingRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return []byte(r.statusOut), nil
}

func (r *recordingRunner) started() bool {
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "start" {
			return true
		}
	}
	return false
}

func testCfg(t *testing.T) config.Config {
	t.Helper()
	c := config.Default()
	c.ProjectsRoot = t.TempDir()
	return c
}

func TestEnsureRunningStartsWhenAbsentOrStopped(t *testing.T) {
	for _, status := range []string{"", "Stopped", "Broken"} {
		t.Run("status="+status, func(t *testing.T) {
			r := &recordingRunner{statusOut: status}
			started, err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t), nil)
			if err != nil {
				t.Fatalf("ensureRunning: %v", err)
			}
			if !r.started() {
				t.Errorf("expected a start call for status %q, calls=%v", status, r.calls)
			}
			if !started {
				t.Errorf("started = false for status %q, want true", status)
			}
		})
	}
}

// An existing instance cannot be started with a template argument (limactl
// refuses with "already exists"), so the stopped path must replace the stored
// config via `template copy --embed-all` and then start by name.
func TestEnsureRunningUpdatesStoredConfigWhenStopped(t *testing.T) {
	r := &recordingRunner{statusOut: "Stopped"}
	if _, err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t), nil); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	var sawResolve, sawPlainStart bool
	for _, call := range r.calls {
		joined := strings.Join(call, " ")
		if strings.HasPrefix(joined, "template copy --embed-all ") && strings.HasSuffix(joined, "/lima.yaml") {
			sawResolve = true
		}
		if joined == "start --tty=false code-sandbox" {
			sawPlainStart = true
		}
		if strings.HasPrefix(joined, "start --tty=false --name") {
			t.Errorf("must not pass a template to an existing instance, got %v", call)
		}
	}
	if !sawResolve {
		t.Errorf("expected the stored config to be replaced via template copy, calls=%v", r.calls)
	}
	if !sawPlainStart {
		t.Errorf("expected a plain start by instance name, calls=%v", r.calls)
	}
}

func TestEnsureRunningIsNoOpWhenRunning(t *testing.T) {
	r := &recordingRunner{statusOut: "Running"}
	started, err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t), nil)
	if err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if r.started() {
		t.Errorf("must not start an already-running instance, calls=%v", r.calls)
	}
	if started {
		t.Errorf("started = true for a Running instance, want false")
	}
}

func TestRenderInstanceFileIsPrivateAndComplete(t *testing.T) {
	c := testCfg(t)
	path, err := renderInstanceFile(c, nil)
	if err != nil {
		t.Fatalf("renderInstanceFile: %v", err)
	}
	defer os.Remove(path)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("rendered instance file mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "/etc/sandbox/provision.env") {
		t.Error("rendered instance must include the provision.env data file")
	}
	if !strings.Contains(s, "/usr/local/lib/sandbox/provision-system.sh") {
		t.Error("rendered instance must deliver and invoke provision-system.sh")
	}
	if !strings.Contains(s, c.ProjectsRoot) {
		t.Error("rendered instance must mount the projects root")
	}
	if !strings.Contains(s, "/usr/local/share/sandbox-profiles/manifest.env") {
		t.Error("rendered instance must always deliver manifest.env, even with no profiles")
	}
}

func TestRenderParamsUsesHostIdentity(t *testing.T) {
	p, err := renderParams(testCfg(t), nil)
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	if p.AgentUser != "devuser" {
		t.Errorf("AgentUser = %q, want \"devuser\"", p.AgentUser)
	}
	if p.AgentUID != os.Getuid() || p.AgentGID != os.Getgid() {
		t.Errorf("agent identity = %d:%d, want host %d:%d", p.AgentUID, p.AgentGID, os.Getuid(), os.Getgid())
	}
	if len(p.DataFiles) == 0 {
		t.Error("DataFiles must be populated from the embedded assets")
	}
}

// The config leaves the driver unset by default, so the accelerated one for
// this host has to be filled in before rendering — on Linux QEMU, which KVM
// accelerates, and on macOS vz, which runs on Hypervisor.framework.
func TestRenderParamsResolvesTheHostHypervisor(t *testing.T) {
	want, err := config.ResolveVMType("", runtime.GOOS)
	if err != nil {
		t.Skipf("code-vm does not support %s hosts: %v", runtime.GOOS, err)
	}
	p, err := renderParams(testCfg(t), nil)
	if err != nil {
		t.Fatalf("renderParams: %v", err)
	}
	if p.VMType != want {
		t.Errorf("VMType = %q, want %q on %s", p.VMType, want, runtime.GOOS)
	}
}

// Asking for the driver the other platform uses must fail before a VM is
// created, rather than booting one whose mounts silently rewrite ownership.
func TestRenderParamsRejectsTheOtherHostsHypervisor(t *testing.T) {
	c := testCfg(t)
	c.VMType = config.VMTypeVZ
	if runtime.GOOS == "darwin" {
		c.VMType = config.VMTypeQEMU
	}
	if _, err := renderParams(c, nil); err == nil {
		t.Errorf("renderParams with vmType %q on %s = nil error, want a failure", c.VMType, runtime.GOOS)
	}
}

// Resolution needs nothing from the guest, so an unmapped secret must abort
// `code-vm start` before ensureRunning ever boots the VM — otherwise a VM
// with a stale/half-rendered template set is left running.
func TestStartFailsBeforeBootingOnUnmappedSecret(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.yaml"),
		[]byte("secrets:\n  tok:\n    suggest: gopass show -o t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "templates", ".npmrc"), []byte("${secret:tok}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, "profiles:\n  - p\n")
	// No secrets.yaml: "tok" is unmapped.

	r := installFakeClient(t, "") // absent: start would create+boot the VM
	root.SetArgs([]string{"start"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("start = %v, want an unmapped-secret error with the suggest snippet", err)
	}
	if r.started() {
		t.Errorf("VM must not be started before resolution succeeds, calls=%v", r.calls)
	}
	if ranAny(r.calls, "copy") {
		t.Errorf("no file may be staged into the guest before resolution succeeds, calls=%v", r.calls)
	}
}
