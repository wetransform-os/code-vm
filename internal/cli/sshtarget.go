package cli

import (
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
	Port         int  // 0 when unknown
	Pinned       bool // from config.sshPort
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
	case t.Port == 0:
		b.WriteString("  Port:          unknown (VM not running; set sshPort in config.yaml to pin one)\n")
	case t.Pinned:
		fmt.Fprintf(&b, "  Port:          %d\n", t.Port)
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

			port := c.SSHPort
			if port == 0 && running {
				file, err := cl.SSHConfigFile(ctx)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				if port, err = lima.ParseSSHPort(string(data)); err != nil {
					return err
				}
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
				User:         agentUser,
				IdentityFile: key.PrivatePath,
				KnownHosts:   key.KnownHostsPath,
			}))
			return nil
		},
	}
}
