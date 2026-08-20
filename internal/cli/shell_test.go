package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

// A bare invocation must pass NO command: naming a shell host-side would
// override sandbox-exec's login-shell fallback, which is the only place the
// agent's chsh'd shell (e.g. a profile setting fish) is known.
func TestAgentCommandLeavesShellChoiceToTheGuest(t *testing.T) {
	if got := agentCommand(nil); len(got) != 0 {
		t.Errorf("agentCommand(nil) = %v, want empty", got)
	}
	if got := agentCommand([]string{}); len(got) != 0 {
		t.Errorf("agentCommand([]) = %v, want empty", got)
	}
}

func TestAgentCommandPassesArgsThrough(t *testing.T) {
	in := []string{"claude", "-p", "fix the bug"}
	got := agentCommand(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("agentCommand(%v) = %v, want unchanged", in, got)
	}
}

func TestResolveWorkdirAcceptsCoveredPath(t *testing.T) {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	got, err := resolveWorkdir(c, "/home/st/projects/repo/sub")
	if err != nil {
		t.Fatalf("resolveWorkdir: %v", err)
	}
	if got != "/home/st/projects/repo/sub" {
		t.Errorf("resolveWorkdir = %q", got)
	}
}

// setupShellFixture writes a scratch config with a profile "p" that declares
// an unmapped secret used by a template, activates it, and chdirs into a
// directory the config shares with the guest (resolveWorkdir requires this).
// No secrets.yaml is written, so "tok" stays unmapped throughout.
func setupShellFixture(t *testing.T) {
	t.Helper()
	dir := withScratchConfig(t)
	c, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(c.ProjectsRoot, "repo")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workdir)

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
}

// Resolution needs nothing from the guest, so an unmapped secret must abort
// the bare `code-vm` invocation before ensureRunning ever boots the VM.
func TestRunDefaultFailsBeforeBootingOnUnmappedSecret(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "") // absent: a bare invocation would boot it
	root.SetArgs([]string{})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("bare invocation = %v, want an unmapped-secret error with the suggest snippet", err)
	}
	if r.started() {
		t.Errorf("VM must not be started before resolution succeeds, calls=%v", r.calls)
	}
	if ranAny(r.calls, "copy") {
		t.Errorf("no file may be staged into the guest before resolution succeeds, calls=%v", r.calls)
	}
}

// The already-running fast path must keep today's behavior exactly: no
// resolution (so no pinentry) and no push, even when a declared secret has no
// mapping — resolution never even looks at secrets.yaml in this path.
func TestRunDefaultRunningFastPathSkipsResolution(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("bare invocation against a running VM = %v, want no error (fast path must not resolve)", err)
	}
	if ranAny(r.calls, ".npmrc") {
		t.Errorf("fast path must not render/push templates, calls=%v", r.calls)
	}
}

// countStatusCalls reports how many times the runner was asked for `limactl
// list ...` (the status query behind cl.Status).
func countStatusCalls(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == "list" {
			n++
		}
	}
	return n
}

// runDefault must decide "is it running" exactly once: the same observation
// both gates resolution and is handed to ensureRunningWithStatus to decide
// whether to boot. A second, independent Status call here would reopen the
// TOCTOU this fix closes — the VM stopping between two separate checks could
// leave resolution skipped (outer check saw Running) while ensureRunning
// boots anyway, silently dropping template rendering for that boot.
func TestRunDefaultQueriesStatusExactlyOnce(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("bare invocation against a running VM = %v", err)
	}
	if n := countStatusCalls(r.calls); n != 1 {
		t.Errorf("expected exactly one status query, got %d, calls=%v", n, r.calls)
	}
}

// A Stopped VM is the boot path: resolution must happen before ensureRunning
// boots it, keyed off the very same status observation used to decide the
// boot itself (single-status-decision — see the comment on ensureRunning).
func TestRunDefaultStoppedResolvesBeforeBoot(t *testing.T) {
	root := NewRootCmd()
	setupShellFixture(t)

	r := installFakeClient(t, "Stopped")
	root.SetArgs([]string{})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("bare invocation against a stopped VM = %v, want an unmapped-secret error with the suggest snippet", err)
	}
	if r.started() {
		t.Errorf("VM must not be started before resolution succeeds, calls=%v", r.calls)
	}
	if n := countStatusCalls(r.calls); n != 1 {
		t.Errorf("expected exactly one status query, got %d, calls=%v", n, r.calls)
	}
}

func TestResolveWorkdirRejectsUncoveredPathWithActionableError(t *testing.T) {
	c := config.Default()
	c.ProjectsRoot = "/home/st/projects"
	_, err := resolveWorkdir(c, "/tmp/elsewhere")
	if err == nil {
		t.Fatal("expected an error for a path outside every mount")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/tmp/elsewhere") {
		t.Errorf("error must name the offending path, got %q", msg)
	}
	if !strings.Contains(msg, "code-vm mount") {
		t.Errorf("error must tell the user how to fix it, got %q", msg)
	}
}
