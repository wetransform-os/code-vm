package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/guest"
	"github.com/wetransform/code-vm/internal/lima"
)

// newClient is a package variable so tests can substitute a fake runner.
var newClient = lima.NewClient

// agentUser is the guest account the agent runs as. Its UID and GID mirror
// the host user's so virtiofs-shared files are owned by it.
const agentUser = "devuser"

// renderParams gathers the host-derived values the Lima template needs.
func renderParams() (lima.RenderParams, error) {
	files, err := guest.DataFiles()
	if err != nil {
		return lima.RenderParams{}, err
	}
	return lima.RenderParams{
		AgentUser: agentUser,
		AgentUID:  os.Getuid(),
		AgentGID:  os.Getgid(),
		DataFiles: files,
	}, nil
}

// renderInstanceFile writes the rendered Lima instance to a temp file and
// returns its path. The caller is responsible for removing it.
func renderInstanceFile(c config.Config) (string, error) {
	p, err := renderParams()
	if err != nil {
		return "", err
	}
	body, err := lima.Render(c, p)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "code-sandbox-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp instance file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod temp instance file: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		return "", fmt.Errorf("write temp instance file: %w", err)
	}
	return f.Name(), nil
}

// ensureRunning brings the sandbox VM up if it is not already running.
// The rendered template is passed on every start, so a code-vm upgrade or a
// config change is picked up without a separate migration step: the
// mode:data guest files carry overwrite: true.
func ensureRunning(ctx context.Context, cl lima.Client, c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	status, err := cl.Status(ctx)
	if err != nil {
		return err
	}
	if status == "Running" {
		return nil
	}
	path, err := renderInstanceFile(c)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	return cl.Start(ctx, path)
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the sandbox VM (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := loadConfig()
			if err != nil {
				return err
			}
			return ensureRunning(cmd.Context(), newClient(), c)
		},
	}
}
