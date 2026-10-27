#!/bin/bash
###############################################################################
# agent-login-gate.sh — refuse agent logins while the early egress guard is
# not in place
#
# Wired into /etc/pam.d/sshd as a pam_exec account check by provisioning.
# sandbox-egress-early.service is what keeps agent-controlled code off the
# network between early boot and the firewall; `Before=` only orders units,
# so a failed guard would otherwise leave sshd accepting the agent's key
# with nothing in front of it. A guard that failed, was stopped, or never
# ran (an unprovisioned guest) refuses the agent; every other account is
# untouched, so limaadmin stays reachable to diagnose it.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

[ "${PAM_USER:-}" = "$AGENT_USER" ] || exit 0
if systemctl -q is-active sandbox-egress-early.service; then
    exit 0
fi
echo "agent-login-gate: refusing $PAM_USER, sandbox-egress-early.service is not active" >&2
exit 1
