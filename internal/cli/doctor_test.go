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

// macOS reports two-component versions ("13.5") as often as three, so the
// comparison must not require all three to be present.
func TestAtLeastVersionPadsMissingComponents(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"13.5", false},
		{"13.5.2", false},
		{"14", false},
		{"26.5.2", false},
		{"13.4", true},
		{"13", true},
		{"12.9.9", true},
		{"", true},
		{"sequoia", true},
		{"1.2.3.4", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if err := atLeastVersion(tc.in, minMacOS); (err != nil) != tc.wantErr {
				t.Errorf("atLeastVersion(%q, %v) error = %v, wantErr %v", tc.in, minMacOS, err, tc.wantErr)
			}
		})
	}
}

// The virtualisation prerequisites are not the same list on both platforms:
// macOS has no /dev/kvm and needs no virtiofsd package, because
// Virtualization.framework provides both the hypervisor and the virtio-fs
// device. Reporting the Linux list on a Mac would fail every run for reasons
// the user cannot act on.
func TestHypervisorChecksAreHostAppropriate(t *testing.T) {
	tests := []struct {
		goos    string
		want    []string
		notWant []string
	}{
		{
			goos:    "darwin",
			want:    []string{"macOS supports Virtualization.framework", "hardware virtualisation (HVF) available"},
			notWant: []string{"KVM accessible", "virtiofsd installed"},
		},
		{
			goos:    "linux",
			want:    []string{"virtiofsd installed", "KVM accessible"},
			notWant: []string{"hardware virtualisation (HVF) available"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			names := map[string]bool{}
			for _, c := range hypervisorChecks(tc.goos) {
				names[c.name] = true
			}
			for _, w := range tc.want {
				if !names[w] {
					t.Errorf("hypervisorChecks(%q) missing check %q, got %v", tc.goos, w, names)
				}
			}
			for _, w := range tc.notWant {
				if names[w] {
					t.Errorf("hypervisorChecks(%q) must not run check %q", tc.goos, w)
				}
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
