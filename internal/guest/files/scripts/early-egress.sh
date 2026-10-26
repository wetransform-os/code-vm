#!/bin/bash
###############################################################################
# early-egress.sh — fail-closed agent egress from early boot
#
# Run by sandbox-egress-early.service before the network, sshd and the agent's
# lingering user manager come up. init-firewall.sh builds the real ruleset
# much later (after cloud-final); until it flushes OUTPUT, this single rule is
# what keeps agent-controlled code off the network. Loopback stays open so the
# agent's own IPC keeps working; nothing leaves the guest over lo.
#
# Only the agent UID is touched: root, _apt and Lima's own ssh must keep
# unrestricted egress for provisioning, exactly as init-firewall.sh grants.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

RULE=(OUTPUT ! -o lo -m owner --uid-owner "$AGENT_UID" -j REJECT --reject-with icmp-port-unreachable)
if ! iptables -C "${RULE[@]}" 2> /dev/null; then
    iptables -I "${RULE[@]}"
fi
echo "[early-egress] agent uid $AGENT_UID has no network beyond loopback until the firewall is built"
