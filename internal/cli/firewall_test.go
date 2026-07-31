package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateFirewallMode(t *testing.T) {
	for _, mode := range firewallModes {
		if err := validateFirewallMode(mode); err != nil {
			t.Errorf("validateFirewallMode(%q) = %v, want nil", mode, err)
		}
	}
	for _, mode := range []string{"", "off", "disabled", "ALLOWLIST"} {
		if err := validateFirewallMode(mode); err == nil {
			t.Errorf("validateFirewallMode(%q) = nil, want an error", mode)
		}
	}
}

func TestValidateFirewallModeErrorListsValidModes(t *testing.T) {
	err := validateFirewallMode("off")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, mode := range firewallModes {
		if !strings.Contains(err.Error(), mode) {
			t.Errorf("error should list %q as a valid mode, got %q", mode, err)
		}
	}
}

func TestSetFirewallModeArgs(t *testing.T) {
	got, err := setFirewallModeArgs("audit")
	if err != nil {
		t.Fatalf("setFirewallModeArgs: %v", err)
	}
	want := []string{"/usr/local/lib/sandbox/set-firewall-mode.sh", "audit"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("setFirewallModeArgs = %v, want %v", got, want)
	}
	if _, err := setFirewallModeArgs("off"); err == nil {
		t.Error("expected an error for an invalid mode")
	}
}
