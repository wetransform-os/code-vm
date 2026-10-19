package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseForward(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want portForward
	}{
		{"8080", portForward{Host: 8080, Guest: 8080}},
		{"3000:8080", portForward{Host: 3000, Guest: 8080}},
		{"1:65535", portForward{Host: 1, Guest: 65535}},
	} {
		got, err := parseForward(tc.spec)
		if err != nil {
			t.Errorf("parseForward(%q): %v", tc.spec, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseForward(%q) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

// A bind address in the spec is refused rather than passed through: the host
// side is always loopback, so the guest server is never reachable from the
// network the host sits on.
func TestParseForwardRejectsInvalidSpecs(t *testing.T) {
	for _, spec := range []string{
		"", "0", "65536", "-1", "http", "80:", ":80", "1:2:3",
		"0.0.0.0:8080:8080", "localhost:8080", " 8080",
	} {
		if got, err := parseForward(spec); err == nil {
			t.Errorf("parseForward(%q) = %+v, want an error", spec, got)
		}
	}
}

// Lima's ssh config multiplexes over a persistent master; a forward opened
// through it would outlive code-vm, so the tunnel must not use it.
func TestForwardSSHArgsBindHostLoopbackOnly(t *testing.T) {
	got := forwardSSHArgs("/lima/code-sandbox/ssh.config", "lima-code-sandbox", []portForward{
		{Host: 8080, Guest: 8080},
		{Host: 3000, Guest: 5173},
	})
	want := []string{
		"-F", "/lima/code-sandbox/ssh.config",
		"-N",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ExitOnForwardFailure=yes",
		"-L", "127.0.0.1:8080:127.0.0.1:8080",
		"-L", "127.0.0.1:3000:127.0.0.1:5173",
		"lima-code-sandbox",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("forwardSSHArgs =\n  %q\nwant\n  %q", got, want)
	}
}

// installFakeSSH replaces the ssh launcher and records its argv.
func installFakeSSH(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	old := runSSH
	runSSH = func(_ context.Context, args []string) error {
		calls = append(calls, args)
		return nil
	}
	t.Cleanup(func() { runSSH = old })
	return &calls
}

func TestForwardTunnelsThroughTheConfiguredInstance(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	r := installFakeClient(t, "Running")
	ssh := installFakeSSH(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"forward", "8080", "3000:5173"})
	if err := root.Execute(); err != nil {
		t.Fatalf("forward: %v\n%s", err, out.String())
	}

	if !ranAny(r.calls, "list code-sandbox-clitest --format {{.SSHConfigFile}}") {
		t.Errorf("expected the ssh config to be looked up for the configured instance, calls=%v", r.calls)
	}
	if len(*ssh) != 1 {
		t.Fatalf("ssh ran %d times, want 1", len(*ssh))
	}
	args := strings.Join((*ssh)[0], " ")
	for _, want := range []string{
		"-L 127.0.0.1:8080:127.0.0.1:8080",
		"-L 127.0.0.1:3000:127.0.0.1:5173",
		"lima-code-sandbox-clitest",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args %q lack %q", args, want)
		}
	}
	for _, want := range []string{"http://localhost:8080", "http://localhost:3000"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A forward is for reaching a server that is already running in the guest, so
// a stopped VM is an error rather than a reason to boot it.
func TestForwardRefusesWhenNotRunning(t *testing.T) {
	for _, status := range []string{"", "Stopped"} {
		t.Run("status="+status, func(t *testing.T) {
			root := NewRootCmd()
			withScratchConfig(t)
			r := installFakeClient(t, status)
			ssh := installFakeSSH(t)

			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"forward", "8080"})
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "not running") {
				t.Errorf("err = %v, want a not-running refusal", err)
			}
			if r.started() {
				t.Error("forward must not boot the VM")
			}
			if len(*ssh) != 0 {
				t.Errorf("ssh ran despite the VM not running: %v", *ssh)
			}
		})
	}
}

func TestForwardRejectsBadArgsBeforeTouchingTheVM(t *testing.T) {
	for _, args := range [][]string{
		{"forward"},
		{"forward", "web"},
		{"forward", "8080", "8080"},
		{"forward", "8080", "8080:9090"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := NewRootCmd()
			withScratchConfig(t)
			r := installFakeClient(t, "Running")
			ssh := installFakeSSH(t)

			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Error("expected an error")
			}
			if len(r.calls) != 0 || len(*ssh) != 0 {
				t.Errorf("expected no VM or ssh interaction, limactl=%v ssh=%v", r.calls, *ssh)
			}
		})
	}
}

// A signal sent to code-vm alone, as a script stopping a background forward
// does, never reaches ssh through a terminal process group. It must be relayed,
// or code-vm waits forever on a tunnel that keeps the host port open.
func TestRunSSHRelaysTerminationAndStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "started")
	script := "#!/bin/sh\ntouch \"$CODE_VM_TEST_MARK\"\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODE_VM_TEST_MARK", mark)

	done := make(chan error, 1)
	go func() { done <- runSSH(context.Background(), nil) }()

	// The mark exists only once ssh started, which is after runSSH subscribed
	// to the signal — so the signal below cannot kill the test binary.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake ssh never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runSSH = %v, want nil for a signalled stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runSSH did not return: the signal was not relayed to ssh")
	}
}
