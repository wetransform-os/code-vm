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

func TestSSHTargetUsesPinnedPortAndInstallsMissingKey(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	appendConfig(t, "sshPort: 60122\n")
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Running") // ReadFile of authorized_keys returns "Running" too: not the key, so a push follows
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Port:          60122\n") {
		t.Errorf("pinned port not shown:\n%s", out.String())
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") || !strings.Contains(out.String(), "Installed the agent's SSH key into the running VM.") {
		t.Errorf("a running VM without the key must get it installed; calls=%q out=%s", r.calls, out.String())
	}
	if ranAny(r.calls, "{{.SSHConfigFile}}") {
		t.Error("a pinned port needs no look at Lima's ssh config")
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
