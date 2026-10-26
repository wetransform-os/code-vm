package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSSHTargetPinned(t *testing.T) {
	got := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 60022, Pinned: true, User: "devuser",
		IdentityFile: "/h/.config/code-vm/ssh/code-sandbox/agent_ed25519", KnownHosts: "/h/.config/code-vm/ssh/code-sandbox/known_hosts"})
	want := `Orca / SSH target for instance code-sandbox
  Host:          127.0.0.1
  Port:          60022
  User:          devuser
  Identity file: /h/.config/code-vm/ssh/code-sandbox/agent_ed25519

OpenSSH config snippet (add to ~/.ssh/config or an included file):
Host code-vm-code-sandbox
  HostName 127.0.0.1
  Port 60022
  User devuser
  IdentityFile /h/.config/code-vm/ssh/code-sandbox/agent_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile /h/.config/code-vm/ssh/code-sandbox/known_hosts
`
	if got != want {
		t.Errorf("renderSSHTarget pinned:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderSSHTargetUnpinned(t *testing.T) {
	running := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 38421, User: "devuser", IdentityFile: "/k", KnownHosts: "/kh"})
	if !strings.Contains(running, "  Port:          38421\n") || !strings.Contains(running, "warning: sshPort is not set in config.yaml, so this port changes on every boot") {
		t.Errorf("unpinned running output:\n%s", running)
	}
	stopped := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 0, User: "devuser", IdentityFile: "/k", KnownHosts: "/kh"})
	if !strings.Contains(stopped, "  Port:          unknown (VM not running; set sshPort in config.yaml to pin one)") || strings.Contains(stopped, "Host code-vm-") {
		t.Errorf("unpinned stopped output must say unknown and omit the snippet:\n%s", stopped)
	}
}

func TestSSHTargetNeverBoots(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Stopped")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ssh-target on a stopped VM must succeed: %v", err)
	}
	if r.started() {
		t.Error("ssh-target must never boot the VM")
	}
	if !strings.Contains(out.String(), "User:          devuser") {
		t.Errorf("output:\n%s", out.String())
	}
}

func writeSSHConfig(t *testing.T, port string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh.config")
	if err := os.WriteFile(p, []byte("Host lima-code-sandbox-clitest\n  Port "+port+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderSSHTargetLiveness(t *testing.T) {
	base := sshTarget{Instance: "i", Pinned: true, Port: 60022, Running: true, User: "u", IdentityFile: "/k", KnownHosts: "/kh"}
	mismatch := base
	mismatch.LivePort = 38421
	got := renderSSHTarget(mismatch)
	if !strings.Contains(got, "  Port:          60022\n  (warning: sshPort 60022 is not applied yet; the running VM still listens on 38421 — restart it with code-vm stop && code-vm start)\n  User:") {
		t.Errorf("mismatch output:\n%s", got)
	}
	unknown := base
	got = renderSSHTarget(unknown)
	if !strings.Contains(got, "  Port:          60022\n  (warning: could not read Lima's ssh config to confirm the live port)\n") {
		t.Errorf("unknown live port output:\n%s", got)
	}
	same := base
	same.LivePort = 60022
	stopped := base
	stopped.Running = false
	if a, b := renderSSHTarget(same), renderSSHTarget(stopped); a != b || strings.Contains(a, "warning") {
		t.Errorf("equal live port / stopped must not warn:\n%s\n%s", a, b)
	}
	un := sshTarget{Instance: "i", Running: true, User: "u", IdentityFile: "/k", KnownHosts: "/kh"}
	got = renderSSHTarget(un)
	if !strings.Contains(got, "  Port:          unknown (could not read Lima's ssh config; set sshPort in config.yaml to pin one)\n") || strings.Contains(got, "Host code-vm-") {
		t.Errorf("unpinned running unknown:\n%s", got)
	}
}

func runSSHTarget(t *testing.T, sshCfg string) (string, *recordingRunner, error) {
	t.Helper()
	root := NewRootCmd()
	withScratchConfig(t)
	appendConfig(t, "sshPort: 60122\n")
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Running") // ReadFile of authorized_keys returns "Running" too: not the key, so a push follows
	r.outputs = map[string]string{"{{.SSHConfigFile}}": sshCfg}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	err := root.Execute()
	return out.String(), r, err
}

func TestSSHTargetUsesPinnedPortAndInstallsMissingKey(t *testing.T) {
	out, r, err := runSSHTarget(t, writeSSHConfig(t, "60122"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  Port:          60122\n") || strings.Contains(out, "warning") {
		t.Errorf("pinned port matching the live one must show no warning:\n%s", out)
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") || !strings.Contains(out, "Installed the agent's SSH key into the running VM.") {
		t.Errorf("a running VM without the key must get it installed; calls=%q out=%s", r.calls, out)
	}
}

func TestSSHTargetWarnsWhenPinnedPortNotApplied(t *testing.T) {
	out, r, err := runSSHTarget(t, writeSSHConfig(t, "38421"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: sshPort 60122 is not applied yet; the running VM still listens on 38421") {
		t.Errorf("output:\n%s", out)
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") {
		t.Error("key must still be installed")
	}
}

func TestSSHTargetUnreadableSSHConfigOnlyWarns(t *testing.T) {
	out, r, err := runSSHTarget(t, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("an unreadable ssh config must not fail the command: %v", err)
	}
	if !strings.Contains(out, "warning: could not read Lima's ssh config to confirm the live port") {
		t.Errorf("output:\n%s", out)
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") {
		t.Error("key must still be installed")
	}
}

func TestSSHTargetReadsLimaPortWhenUnpinned(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	fakeAgentKeygen(t)
	sshCfg := filepath.Join(t.TempDir(), "ssh.config")
	if err := os.WriteFile(sshCfg, []byte("Host lima-code-sandbox-clitest\n  Port 38421\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := installFakeClient(t, "Running")
	r.outputs = map[string]string{"{{.SSHConfigFile}}": sshCfg}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Port:          38421\n") || !strings.Contains(out.String(), "warning: sshPort is not set") {
		t.Errorf("output:\n%s", out.String())
	}
}
