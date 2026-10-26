package lima

import (
	"errors"
	"regexp"
	"strconv"
)

var sshPortLine = regexp.MustCompile(`(?m)^\s*Port\s+(\d+)\s*$`)

// ParseSSHPort returns the port of the first "Port N" line in a Lima-generated
// ssh config. Lima writes one Host block per instance, so the first match is
// the instance's forwarded sshd.
func ParseSSHPort(sshConfig string) (int, error) {
	m := sshPortLine.FindStringSubmatch(sshConfig)
	if m == nil {
		return 0, errors.New("no Port line in Lima's ssh config")
	}
	return strconv.Atoi(m[1])
}
