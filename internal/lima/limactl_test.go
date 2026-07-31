package lima

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls  [][]string
	out    []byte
	outErr error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return f.out, f.outErr
}

// The workdir travels to sandbox-exec, not to `limactl shell --workdir`:
// the latter would cd as limaadmin before sudo, and a 0700 directory owned
// by the agent is unreachable for limaadmin.
func TestAgentArgsRunsThroughSandboxExecAsDevuser(t *testing.T) {
	c := Client{R: &fakeRunner{}}
	got := c.AgentArgs("/home/st/projects/repo", []string{"claude", "-p", "fix the bug"})
	want := []string{
		"shell", InstanceName,
		"sudo", "/usr/local/bin/sandbox-exec", "--workdir", "/home/st/projects/repo",
		"claude", "-p", "fix the bug",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentArgs() = %v, want %v", got, want)
	}
}

func TestAdminArgsHasNoWorkdirAndNoSandboxExec(t *testing.T) {
	c := Client{R: &fakeRunner{}}
	got := c.AdminArgs([]string{"squid", "-k", "reconfigure"})
	want := []string{"shell", InstanceName, "sudo", "squid", "-k", "reconfigure"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AdminArgs() = %v, want %v", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "sandbox-exec") {
			t.Error("admin commands must not go through sandbox-exec: they run as limaadmin, not the agent")
		}
	}
}

func TestStatusParsesListOutput(t *testing.T) {
	f := &fakeRunner{out: []byte("Running\n")}
	got, err := Client{R: f}.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != "Running" {
		t.Errorf("Status() = %q, want \"Running\"", got)
	}
	want := []string{"list", InstanceName, "--format", "{{.Status}}"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestStatusEmptyWhenInstanceAbsent(t *testing.T) {
	got, err := Client{R: &fakeRunner{out: []byte("\n")}}.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != "" {
		t.Errorf("Status() = %q, want \"\"", got)
	}
}

func TestStartIsNonInteractive(t *testing.T) {
	f := &fakeRunner{}
	if err := (Client{R: f}).Start(context.Background(), "/tmp/code-sandbox.yaml"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"start", "--tty=false", "--name", InstanceName, "/tmp/code-sandbox.yaml"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestCopyUsesInstancePrefixedDestination(t *testing.T) {
	f := &fakeRunner{}
	if err := (Client{R: f}).Copy(context.Background(), "/tmp/local.json", "/tmp/guest.json"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	want := []string{"copy", "/tmp/local.json", InstanceName + ":/tmp/guest.json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv = %v, want %v", f.calls[0], want)
	}
}
