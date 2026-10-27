package cli

import (
	"bytes"
	"testing"
)

// runVersion runs `code-vm --version` through the real command tree. If the
// flag ever fell through to the root RunE, runDefault would try to load the
// config and enter the VM and return an error, so the error check doubles as
// a guard that --version never touches the VM.
func runVersion(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	return out.String()
}

// Unstamped builds must say so instead of claiming a release version.
func TestVersionFlagReportsDevWhenUnstamped(t *testing.T) {
	if out := runVersion(t); out != "code-vm version dev\n" {
		t.Errorf("--version output = %q, want %q", out, "code-vm version dev\n")
	}
}

// The ldflags-stamped value is what --version reports.
func TestVersionFlagReportsStampedVersion(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	if out := runVersion(t); out != "code-vm version 1.2.3\n" {
		t.Errorf("--version output = %q, want %q", out, "code-vm version 1.2.3\n")
	}
}
