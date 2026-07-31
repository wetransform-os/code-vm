package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
)

// agentCommand returns the command to run in the guest, defaulting to an
// interactive login shell.
func agentCommand(args []string) []string {
	if len(args) == 0 {
		return []string{"bash", "-l"}
	}
	return args
}

// resolveWorkdir checks that cwd is inside a declared mount. Lima declares
// mounts in the instance config, so a directory that is not shared cannot be
// reached from the guest at all.
func resolveWorkdir(c config.Config, cwd string) (string, error) {
	mounts := c.Mounts()
	if _, ok := config.CoveringMount(mounts, cwd); ok {
		return cwd, nil
	}
	return "", fmt.Errorf(
		"%s is not shared with the sandbox VM.\nShared directories:\n  %s\nAdd it with:  code-vm mount %s",
		cwd, strings.Join(mounts, "\n  "), cwd)
}

// runDefault is the root command's action: bring the VM up, verify the current
// directory is shared, then run the command as the agent user at that path.
func runDefault(ctx context.Context, args []string) error {
	c, _, err := loadConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	workdir, err := resolveWorkdir(c, cwd)
	if err != nil {
		return err
	}
	cl := newClient()
	if err := ensureRunning(ctx, cl, c); err != nil {
		return err
	}
	return cl.Agent(ctx, workdir, agentCommand(args))
}
