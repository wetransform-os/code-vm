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

// A throwaway test VM must never act on the instance in daily use, so every
// argv builder has to honour Client.Instance rather than the package default.
func TestClientTargetsItsOwnInstance(t *testing.T) {
	c := Client{R: &fakeRunner{}, Instance: "code-sandbox-test"}
	argvs := [][]string{
		c.AgentArgs("/w", []string{"true"}),
		c.AdminArgs([]string{"true"}),
	}
	for _, argv := range argvs {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "code-sandbox-test") {
			t.Errorf("argv does not target the configured instance: %v", argv)
		}
		if strings.Contains(joined, InstanceName+" ") {
			t.Errorf("argv leaked the default instance %q: %v", InstanceName, argv)
		}
	}

	f := &fakeRunner{}
	c = Client{R: f, Instance: "code-sandbox-test"}
	_ = c.Stop(context.Background())
	_ = c.Delete(context.Background())
	_, _ = c.Status(context.Background())
	_ = c.Copy(context.Background(), "/l", "/g")
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, "code-sandbox-test") {
			t.Errorf("lifecycle call did not target the configured instance: %v", call)
		}
	}
}

// An unset Instance keeps targeting the default, so a zero Client and older
// callers behave as before.
func TestClientDefaultsToTheStandardInstance(t *testing.T) {
	got := Client{R: &fakeRunner{}}.AdminArgs([]string{"true"})
	if !strings.Contains(strings.Join(got, " "), InstanceName) {
		t.Errorf("a zero Client must target %q, got %v", InstanceName, got)
	}
}

// Probing guest state must not print cat's "No such file or directory" into the
// user's terminal: AdminOutput streams stderr, so the redirect has to happen in
// the guest. The path must travel as an argument, not inside the command string.
func TestReadFileSuppressesStderrAndPassesPathSafely(t *testing.T) {
	f := &fakeRunner{}
	Client{R: f}.ReadFile(context.Background(), "/run/sandbox/squid-allow.d/10-host-config.conf")
	if len(f.calls) != 1 {
		t.Fatalf("expected one call, got %v", f.calls)
	}
	argv := f.calls[0]
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "2>/dev/null") {
		t.Errorf("read must discard stderr in the guest: %v", argv)
	}
	if argv[len(argv)-1] != "/run/sandbox/squid-allow.d/10-host-config.conf" {
		t.Errorf("path must be the last argument, not interpolated: %v", argv)
	}
	for _, a := range argv[:len(argv)-1] {
		if strings.Contains(a, "10-host-config.conf") {
			t.Errorf("path must not appear inside the command string: %v", argv)
		}
	}
}
