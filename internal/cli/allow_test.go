package cli

import (
	"reflect"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
)

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"example.com", ".example.com"},
		{".example.com", ".example.com"},
		{"https://example.com", ".example.com"},
		{"http://example.com", ".example.com"},
		{"example.com:443", ".example.com"},
		{"pastebin.com:443", ".pastebin.com"},
		{"example.com/some/path", ".example.com"},
		{"https://example.com:8443/x?y=1", ".example.com"},
		{"  example.com  ", ".example.com"},
		{"EXAMPLE.com", ".example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeDomain(tc.in); got != tc.want {
				t.Errorf("normalizeDomain(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeDomainRejectsDegenerate(t *testing.T) {
	for _, in := range []string{
		"", "   ", "https://", "/", ":443", ".",
		// Not hostname-shaped: these must not reach a Squid ACL line, where
		// whitespace would split the directive and quotes could extend it.
		"not a domain", `a"b`, "x;y", "under_score.com", "emoji🙂.com",
	} {
		if got := normalizeDomain(in); got != "" {
			t.Errorf("normalizeDomain(%q) = %q, want empty", in, got)
		}
	}
}

func TestIsIPAddress(t *testing.T) {
	for _, in := range []string{"1.2.3.4", ".1.2.3.4", "192.168.0.1", "::1", "2001:db8::1"} {
		if !isIPAddress(in) {
			t.Errorf("isIPAddress(%q) = false, want true", in)
		}
	}
	for _, in := range []string{".example.com", "example.com", "1.2.3.4.example.com", "v4.example"} {
		if isIPAddress(in) {
			t.Errorf("isIPAddress(%q) = true, want false", in)
		}
	}
}

// Field 7 of a Squid access.log line is the CONNECT target for HTTPS and the
// full URL for plain HTTP; both must yield a usable domain.
func TestParseDeniedDomains(t *testing.T) {
	log := `1785500000.123    0 192.168.5.15 TCP_DENIED/403 4021 CONNECT pastebin.com:443 - HIER_NONE/- text/html
1785500001.456    0 192.168.5.15 TCP_DENIED/403 4021 GET http://evil.example/beacon - HIER_NONE/- text/html
1785500002.789   12 192.168.5.15 TCP_TUNNEL/200 5678 CONNECT api.anthropic.com:443 - HIER_DIRECT/1.2.3.4 -
1785500003.111    0 192.168.5.15 TCP_DENIED/403 4021 CONNECT pastebin.com:443 - HIER_NONE/- text/html
1785500004.222    0 192.168.5.15 TCP_DENIED/403 4021 CONNECT 10.0.0.5:443 - HIER_NONE/- text/html
`
	got := parseDeniedDomains(log)
	want := []string{".evil.example", ".pastebin.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseDeniedDomains() = %v, want %v (denied only, de-duplicated, sorted, IPs dropped)", got, want)
	}
}

func TestParseDeniedDomainsEmptyLog(t *testing.T) {
	if got := parseDeniedDomains(""); len(got) != 0 {
		t.Errorf("parseDeniedDomains(\"\") = %v, want empty", got)
	}
}

// A domain already covered by a parent entry must not be added again: the
// allowlist would grow with redundant lines on every invocation.
func TestAlreadyCovered(t *testing.T) {
	existing := []string{".example.com", "exact.org"}
	tests := []struct {
		domain string
		want   bool
	}{
		{".example.com", true},
		{".sub.example.com", true},
		{"exact.org", true},
		{".exact.org", false},
		{".other.com", false},
		{".notexample.com", false},
	}
	for _, tc := range tests {
		t.Run(tc.domain, func(t *testing.T) {
			if got := alreadyCovered(existing, tc.domain); got != tc.want {
				t.Errorf("alreadyCovered(%v, %q) = %v, want %v", existing, tc.domain, got, tc.want)
			}
		})
	}
}

func TestMergeDomainsSortsAndDeduplicates(t *testing.T) {
	got := mergeDomains([]string{".b.com", ".a.com"}, []string{".c.com", ".a.com"})
	want := []string{".a.com", ".b.com", ".c.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeDomains() = %v, want %v", got, want)
	}
}

// The parsed set feeds straight into config.Validate, so normalization must
// only ever emit entries that validate.
func TestNormalizeDomainOutputPassesConfigValidation(t *testing.T) {
	for _, in := range []string{"https://Example.COM:8443/x", "registry.mycompany.com", "a-b.example.co.uk"} {
		d := normalizeDomain(in)
		if d == "" {
			t.Fatalf("normalizeDomain(%q) produced nothing", in)
		}
		if err := config.ValidateDomain(d); err != nil {
			t.Errorf("normalizeDomain(%q) = %q, which fails validation: %v", in, d, err)
		}
	}
}
