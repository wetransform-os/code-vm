// Command code-vm runs coding agents in a hardened Lima VM.
package main

import (
	"fmt"
	"os"

	"github.com/wetransform/code-vm/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "code-vm: %v\n", err)
		os.Exit(1)
	}
}
