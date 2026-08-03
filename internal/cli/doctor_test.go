package cli

import (
	"os"
	"os/exec"
	"testing"
)

func TestParseLimaVersion(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"standard output", "limactl version 2.2.0\n", "2.2.0", false},
		{"with v prefix", "limactl version v2.3.1\n", "2.3.1", false},
		{"unparseable", "some other tool\n", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimaVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLimaVersion(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLimaVersion(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseLimaVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAtLeastLimaVersion(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"2.2.0", false},
		{"2.2.1", false},
		{"2.3.0", false},
		{"3.0.0", false},
		{"2.1.9", true},
		{"1.9.9", true},
		{"garbage", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			err := atLeastLimaVersion(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("atLeastLimaVersion(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// virtiofsd is a helper binary that distributions install outside PATH, so a
// PATH-only check reported it missing on hosts where Lima was using it happily.
func TestCheckVirtiofsdAcceptsLibexecLocations(t *testing.T) {
	if err := checkVirtiofsd(); err != nil {
		t.Skipf("virtiofsd not installed on this host: %v", err)
	}
	// Present somewhere: either on PATH or at one of the known paths.
	onPath := false
	if _, err := exec.LookPath("virtiofsd"); err == nil {
		onPath = true
	}
	found := onPath
	for _, p := range virtiofsdPaths {
		if _, err := os.Stat(p); err == nil {
			found = true
		}
	}
	if !found {
		t.Error("checkVirtiofsd passed but the binary is at none of the locations it checks")
	}
}

func TestVirtiofsdPathsCoverDebianAndFedoraLayouts(t *testing.T) {
	want := map[string]bool{"/usr/lib/virtiofsd": false, "/usr/libexec/virtiofsd": false}
	for _, p := range virtiofsdPaths {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, ok := range want {
		if !ok {
			t.Errorf("virtiofsdPaths must include %s", p)
		}
	}
}
