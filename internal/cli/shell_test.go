package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestAgentCommandDefaultsToLoginShell(t *testing.T) {
	if got := agentCommand(nil); !reflect.DeepEqual(got, []string{"bash", "-l"}) {
		t.Errorf("agentCommand(nil) = %v, want [bash -l]", got)
	}
	if got := agentCommand([]string{}); !reflect.DeepEqual(got, []string{"bash", "-l"}) {
		t.Errorf("agentCommand([]) = %v, want [bash -l]", got)
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
