package config

import "fmt"

// The Lima virtual machine drivers code-vm supports. QEMU is the only driver
// Lima offers on Linux, where it is accelerated by KVM. "vz" is Apple's
// Virtualization.framework, which is built on Hypervisor.framework (HVF) and is
// the accelerated path on macOS — there is no /dev/kvm there.
const (
	VMTypeQEMU = "qemu"
	VMTypeVZ   = "vz"
)

// ValidateVMType checks the configured driver name on its own. Whether it is
// usable on a given host is a separate question — see ResolveVMType — so a
// config file written on one machine still loads on another.
func ValidateVMType(t string) error {
	switch t {
	case "", VMTypeQEMU, VMTypeVZ:
		return nil
	}
	return fmt.Errorf("vmType must be %q or %q, or empty for the host default, got %q",
		VMTypeQEMU, VMTypeVZ, t)
}

// ResolveVMType returns the Lima driver to render for the configured value on
// goos. An empty value selects the host's accelerated driver: QEMU/KVM on
// Linux, vz on macOS.
//
// The pairing is not a free choice. The sandbox shares workspaces with virtiofs
// because it preserves the host user's UID, which is what keeps workspace files
// host-owned and agent-owned at the same time. Lima only offers virtiofs with
// QEMU on Linux and with vz on macOS — its QEMU driver accepts nothing but
// reverse-sshfs and 9p on darwin, neither of which preserves ownership. The
// combination is caught here, with a reason, rather than at `limactl start`
// as a mountType validation error.
func ResolveVMType(configured, goos string) (string, error) {
	if err := ValidateVMType(configured); err != nil {
		return "", err
	}
	switch goos {
	case "linux":
		if configured == VMTypeVZ {
			return "", fmt.Errorf(
				"vmType %q is macOS-only (it is Apple's Virtualization.framework); on Linux use %q, which KVM accelerates",
				VMTypeVZ, VMTypeQEMU)
		}
		return VMTypeQEMU, nil
	case "darwin":
		if configured == VMTypeQEMU {
			return "", fmt.Errorf(
				"vmType %q on macOS supports only reverse-sshfs and 9p mounts, neither of which keeps the host user's UID on shared workspaces; use %q, which runs on Hypervisor.framework (HVF) and provides virtiofs",
				VMTypeQEMU, VMTypeVZ)
		}
		return VMTypeVZ, nil
	}
	return "", fmt.Errorf("unsupported host OS %q: code-vm runs on Linux (KVM) and macOS (HVF)", goos)
}
