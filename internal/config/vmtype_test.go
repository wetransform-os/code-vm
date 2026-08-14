package config

import "testing"

func TestValidateVMType(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{VMTypeQEMU, false},
		{VMTypeVZ, false},
		// "hvf" is the QEMU accelerator name, not a Lima driver: on macOS the
		// accelerated driver is vz. Rejecting it here turns a plausible guess
		// into a message naming the values that work.
		{"hvf", true},
		{"kvm", true},
		{"QEMU", true},
		{"vmware", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if err := ValidateVMType(tc.in); (err != nil) != tc.wantErr {
				t.Errorf("ValidateVMType(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// An unset vmType must select the driver that is actually accelerated on the
// host, so neither platform needs a config entry to work.
func TestResolveVMTypeDefaultsPerHost(t *testing.T) {
	tests := []struct {
		goos string
		want string
	}{
		{"linux", VMTypeQEMU},
		{"darwin", VMTypeVZ},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			got, err := ResolveVMType("", tc.goos)
			if err != nil {
				t.Fatalf("ResolveVMType(\"\", %q): %v", tc.goos, err)
			}
			if got != tc.want {
				t.Errorf("ResolveVMType(\"\", %q) = %q, want %q", tc.goos, got, tc.want)
			}
		})
	}
}

// Validation is deliberately host-independent — a config file has to survive
// being carried between a Linux and a macOS machine — so the host check lives
// here, and both directions must fail rather than degrade the mount type.
func TestResolveVMTypeRejectsDriverNotAvailableOnHost(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		goos       string
		wantErr    bool
	}{
		{"vz on macOS", VMTypeVZ, "darwin", false},
		{"qemu on Linux", VMTypeQEMU, "linux", false},
		{"vz on Linux", VMTypeVZ, "linux", true},
		{"qemu on macOS", VMTypeQEMU, "darwin", true},
		{"unknown driver", "hvf", "darwin", true},
		{"unsupported host", "", "windows", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveVMType(tc.configured, tc.goos)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ResolveVMType(%q, %q) = %q, error = %v, wantErr %v",
					tc.configured, tc.goos, got, err, tc.wantErr)
			}
			if err == nil && got != tc.configured {
				t.Errorf("ResolveVMType(%q, %q) = %q, want the configured driver back",
					tc.configured, tc.goos, got)
			}
		})
	}
}

// A config naming the other platform's driver must load and validate, so that
// carrying one machine's config to the other fails at doctor with an
// explanation rather than at parse time.
func TestValidateAcceptsEitherHostsDriver(t *testing.T) {
	for _, vmType := range []string{"", VMTypeQEMU, VMTypeVZ} {
		c := Default()
		c.ProjectsRoot = "/home/st/projects"
		c.VMType = vmType
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with vmType %q: %v", vmType, err)
		}
	}
}

func TestValidateRejectsUnknownVMType(t *testing.T) {
	c := Default()
	c.ProjectsRoot = "/home/st/projects"
	c.VMType = "hvf"
	if err := c.Validate(); err == nil {
		t.Error("Validate with vmType \"hvf\" = nil error, want a failure")
	}
}
