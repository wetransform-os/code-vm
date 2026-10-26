//go:build !unix

package agentssh

// lockDir is a no-op where flock is unavailable; code-vm only runs on Linux
// and macOS hosts, this keeps the package compiling elsewhere.
func lockDir(string) (func(), error) { return func() {}, nil }
