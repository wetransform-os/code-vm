package cli

import (
	"context"
	"os"
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
			if err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t)); err != nil {
				t.Fatalf("ensureRunning: %v", err)
			}
			if !r.started() {
				t.Errorf("expected a start call for status %q, calls=%v", status, r.calls)
			}
		})
	}
}

// An existing instance cannot be started with a template argument (limactl
// refuses with "already exists"), so the stopped path must replace the stored
// config via `template copy --embed-all` and then start by name.
func TestEnsureRunningUpdatesStoredConfigWhenStopped(t *testing.T) {
	r := &recordingRunner{statusOut: "Stopped"}
	if err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t)); err != nil {
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
	if err := ensureRunning(context.Background(), lima.Client{R: r}, testCfg(t)); err != nil {
		t.Fatalf("ensureRunning: %v", err)
	}
	if r.started() {
		t.Errorf("must not start an already-running instance, calls=%v", r.calls)
	}
}

func TestRenderInstanceFileIsPrivateAndComplete(t *testing.T) {
	c := testCfg(t)
	path, err := renderInstanceFile(c)
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
}

func TestRenderParamsUsesHostIdentity(t *testing.T) {
	p, err := renderParams()
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
