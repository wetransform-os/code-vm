package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the sandbox VM's state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, path, err := loadConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			cl := newClient()
			status, err := cl.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status == "" {
				status = "not created"
			}
			fmt.Fprintf(out, "instance:      %s (%s)\n", lima.InstanceName, status)
			fmt.Fprintf(out, "config:        %s\n", path)
			fmt.Fprintf(out, "cpus/memory:   %d / %s\n", c.CPUs, c.Memory)
			fmt.Fprintln(out, "shared paths:")
			for _, m := range c.Mounts() {
				fmt.Fprintf(out, "  %s\n", m)
			}
			if status != "Running" {
				return nil
			}
			fmt.Fprintln(out, "firewall:")
			verify, err := cl.AdminOutput(cmd.Context(), []string{"cat", "/run/firewall-verify"})
			if err != nil {
				fmt.Fprintln(out, "  unavailable")
				return nil
			}
			fmt.Fprintf(out, "%s", indentLines(string(verify), "  "))
			return nil
		},
	}
}

// indentLines prefixes every non-empty line, for readable nested output.
func indentLines(s, prefix string) string {
	var b []byte
	for _, line := range splitLines(s) {
		if line == "" {
			continue
		}
		b = append(b, prefix...)
		b = append(b, line...)
		b = append(b, '\n')
	}
	return string(b)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
