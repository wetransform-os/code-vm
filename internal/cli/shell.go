package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/session"
)

// agentCommand returns the command to run in the guest. A bare invocation
// passes no command at all: only the guest knows the agent's login shell
// (profiles may chsh it), and sandbox-exec's fallback launches that shell —
// naming bash here would override it.
func agentCommand(args []string) []string {
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
	c, profiles, cfgPath, err := loadConfigWithProfiles()
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
	cl := clientFor(c)
	started, err := ensureRunning(ctx, cl, c, profiles)
	if err != nil {
		return err
	}
	if err := session.Setup(ctx, agentDeps(cl, c, profiles)); err != nil {
		return fmt.Errorf("session setup: %w", err)
	}
	if started {
		if err := pushRenderedTemplates(ctx, cl, c, profiles, cfgPath, os.Stdout); err != nil {
			return err
		}
	}
	return cl.Agent(ctx, workdir, agentCommand(args))
}
