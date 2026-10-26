//go:build unix

package agentssh

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive advisory lock on dir/.lock and returns the
// release. Two code-vm commands racing on the first run could otherwise see
// ssh-keygen's private key before its .pub and report a half keypair.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open key lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
