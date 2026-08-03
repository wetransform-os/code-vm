package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wetransform/code-vm/internal/lima"
)

// minLima is the lowest Lima version code-vm supports. 2.2.0 is the version
// the template and mode:data provisioning were developed against.
var minLima = [3]int{2, 2, 0}

var limaVersionRe = regexp.MustCompile(`version\s+v?([0-9]+\.[0-9]+\.[0-9]+)`)

// parseLimaVersion extracts the semantic version from `limactl --version`.
func parseLimaVersion(out string) (string, error) {
	m := limaVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("cannot parse limactl version from %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// atLeastLimaVersion reports whether got satisfies the minimum.
func atLeastLimaVersion(got string) error {
	parts := strings.Split(got, ".")
	if len(parts) != 3 {
		return fmt.Errorf("unrecognised version %q", got)
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("unrecognised version %q", got)
		}
		v[i] = n
	}
	for i := range v {
		if v[i] > minLima[i] {
			return nil
		}
		if v[i] < minLima[i] {
			return fmt.Errorf("installed version %s is too old; need %d.%d.%d or newer",
				got, minLima[0], minLima[1], minLima[2])
		}
	}
	return nil
}

type check struct {
	name string
	err  error
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			checks := []check{
				{"limactl on PATH", checkBinary("limactl")},
				{"Lima version", checkLimaVersion(ctx)},
				{"virtiofsd installed", checkVirtiofsd()},
				{"KVM accessible", checkKVM()},
				{"config valid", checkConfig()},
			}
			failed := 0
			for _, c := range checks {
				if c.err == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "  OK   %s\n", c.name)
					continue
				}
				failed++
				fmt.Fprintf(cmd.OutOrStdout(), "  FAIL %s: %v\n", c.name, c.err)
			}
			if failed > 0 {
				return fmt.Errorf("%d prerequisite check(s) failed", failed)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nAll prerequisites satisfied.")
			return nil
		},
	}
}

func checkBinary(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("not found on PATH (install it, e.g. via your package manager)")
	}
	return nil
}

// virtiofsdPaths are the locations distributions install virtiofsd to. It is a
// helper binary rather than a user-facing command, so packages routinely place
// it outside PATH — Debian and Ubuntu use /usr/lib, Fedora /usr/libexec — and
// Lima finds it there. Checking only PATH reported it missing on hosts where it
// was installed and working.
var virtiofsdPaths = []string{
	"/usr/lib/virtiofsd",
	"/usr/libexec/virtiofsd",
	"/usr/lib/qemu/virtiofsd",
	"/usr/libexec/qemu/virtiofsd",
	"/usr/local/lib/virtiofsd",
	"/usr/local/libexec/virtiofsd",
}

func checkVirtiofsd() error {
	if _, err := exec.LookPath("virtiofsd"); err == nil {
		return nil
	}
	for _, p := range virtiofsdPaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return nil
		}
	}
	return fmt.Errorf("not found on PATH or in %v (install the virtiofsd package)", virtiofsdPaths)
}

func checkLimaVersion(ctx context.Context) error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return errors.New("limactl not found; skipping version check")
	}
	out, err := lima.NewClient().Version(ctx)
	if err != nil {
		return err
	}
	v, err := parseLimaVersion(out)
	if err != nil {
		return err
	}
	return atLeastLimaVersion(v)
}

func checkKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm not usable (%v); add your user to the kvm group or enable virtualisation in firmware", err)
	}
	return f.Close()
}

func checkConfig() error {
	c, path, err := loadConfig()
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if fi, err := os.Stat(c.ProjectsRoot); err != nil {
		return fmt.Errorf("projectsRoot %s: %w", c.ProjectsRoot, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("projectsRoot %s is not a directory", c.ProjectsRoot)
	}
	return nil
}
