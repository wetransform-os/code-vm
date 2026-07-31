package cli

import (
	"reflect"
	"testing"
)

func TestProxyLogArgs(t *testing.T) {
	for _, mode := range []string{"all", "denied", "allowed", "follow"} {
		got, err := proxyLogArgs(mode)
		if err != nil {
			t.Fatalf("proxyLogArgs(%q): %v", mode, err)
		}
		want := []string{"/usr/local/lib/sandbox/proxy-log.sh", mode}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("proxyLogArgs(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestProxyLogArgsRejectsUnknownMode(t *testing.T) {
	if _, err := proxyLogArgs("everything"); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}
