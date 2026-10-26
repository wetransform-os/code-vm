package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/session"
)

// sshTarget is what an SSH client needs to reach the agent user in the guest.
type sshTarget struct {
	Instance     string
	Port         int  // the port to connect to; 0 when unknown
	Pinned       bool // from config.sshPort
	Running      bool
	LivePort     int // what Lima's ssh config says the running VM uses; 0 when unknown
	User         string
	IdentityFile string
	KnownHosts   string
}

// renderSSHTarget formats the Orca form values and an ssh_config alias. An
// unknown port omits the alias: a snippet with a guessed port would fail
// confusingly instead of saying why.
func renderSSHTarget(t sshTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Orca / SSH target for instance %s\n", t.Instance)
	b.WriteString("  Host:          127.0.0.1\n")
	switch {
	case t.Port == 0 && t.Running:
		b.WriteString("  Port:          unknown (could not read Lima's ssh config; set sshPort in config.yaml to pin one)\n")
	case t.Port == 0:
		b.WriteString("  Port:          unknown (VM not running; set sshPort in config.yaml to pin one)\n")
	case t.Pinned:
		// A pinned port only takes effect at the next boot, so a running VM
		// may still listen elsewhere; a snippet with the wrong port would
		// fail confusingly.
		fmt.Fprintf(&b, "  Port:          %d\n", t.Port)
		switch {
		case !t.Running:
		case t.LivePort == 0:
			b.WriteString("  (warning: could not read Lima's ssh config to confirm the live port)\n")
		case t.LivePort != t.Port:
			fmt.Fprintf(&b, "  (warning: sshPort %d is not applied yet; the running VM still listens on %d — restart it with code-vm stop && code-vm start)\n", t.Port, t.LivePort)
		}
	default:
		fmt.Fprintf(&b, "  Port:          %d\n", t.Port)
		b.WriteString("  (warning: sshPort is not set in config.yaml, so this port changes on every boot)\n")
	}
	fmt.Fprintf(&b, "  User:          %s\n", t.User)
	fmt.Fprintf(&b, "  Identity file: %s\n", t.IdentityFile)
	if t.Port == 0 {
		return b.String()
	}
	b.WriteString("\nOpenSSH config snippet (add to ~/.ssh/config or an included file):\n")
	fmt.Fprintf(&b, "Host code-vm-%s\n", t.Instance)
	b.WriteString("  HostName 127.0.0.1\n")
	fmt.Fprintf(&b, "  Port %d\n", t.Port)
	fmt.Fprintf(&b, "  User %s\n", t.User)
	fmt.Fprintf(&b, "  IdentityFile %s\n", t.IdentityFile)
	b.WriteString("  IdentitiesOnly yes\n")
	fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", t.KnownHosts)
	return b.String()
}

func newSSHTargetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-target",
		Short: "Print the SSH target Orca or an ssh_config alias needs to reach the agent user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, cfgPath, err := loadConfig()
			if err != nil {
				return err
			}
			cl := clientFor(c)
			// A failed status query reads as "not running": this command only
			// reports, so it must not fail because the VM is unreachable.
			status, _ := cl.Status(ctx)
			running := status == "Running"

			// Unlike at boot, a missing ssh-keygen is fatal: every line of the
			// output depends on the key.
			key, err := agentKey(ctx, c, cfgPath)
			if err != nil {
				return err
			}

			// Reading the live port is best effort: the key install below
			// matters more, so an unreadable ssh config only degrades the output.
			live := 0
			if running {
				live = liveSSHPort(ctx, cl)
			}
			port := c.SSHPort
			if port == 0 {
				port = live
			}

			out := cmd.OutOrStdout()
			if running {
				d := agentDeps(cl, c, nil)
				if !session.AuthorizedKeyInstalled(ctx, d, key.AuthorizedLine) {
					if err := session.PushAuthorizedKey(ctx, d, key.AuthorizedLine); err != nil {
						return err
					}
					fmt.Fprintln(out, "Installed the agent's SSH key into the running VM.")
				}
			}
			fmt.Fprint(out, renderSSHTarget(sshTarget{
				Instance:     c.Instance,
				Port:         port,
				Pinned:       c.SSHPort != 0,
				Running:      running,
				LivePort:     live,
				User:         agentUser,
				IdentityFile: key.PrivatePath,
				KnownHosts:   key.KnownHostsPath,
			}))
			return nil
		},
	}
}

// liveSSHPort returns the port the running VM's sshd is forwarded to, or 0
// when Lima's ssh config cannot be read or parsed.
func liveSSHPort(ctx context.Context, cl lima.Client) int {
	file, err := cl.SSHConfigFile(ctx)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	port, err := lima.ParseSSHPort(string(data))
	if err != nil {
		return 0
	}
	return port
}
