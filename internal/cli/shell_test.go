package cli

import (
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
