#!/bin/bash
###############################################################################
# early-egress.sh — fail-closed agent egress from early boot
#
# Run by sandbox-egress-early.service before the network, sshd and the agent's
# lingering user manager come up. init-firewall.sh builds the real ruleset
# much later (after cloud-final); until it flushes OUTPUT, this single rule is
# what keeps agent-controlled code off the network.
#
# Loopback is not exempt. Squid is enabled at boot and starts with whatever
# squid.conf the previous mode left on disk — `allow all` after audit or open
# mode — and its own egress runs as the proxy user, so an agent reaching
# localhost:3128 in this window would have had the proxy's unrestricted
# egress. Nothing the agent runs during boot needs IP loopback.
#
# Only the agent's UIDs are touched: root, _apt and Lima's own ssh must keep
# unrestricted egress for provisioning, exactly as init-firewall.sh grants.
# "The agent's UIDs" includes its subordinate ranges from /etc/subuid: a
# process in one of the agent's user namespaces running as a mapped UID owns
# sockets with that kernel UID, which a rule on the primary UID alone misses.
# Both address families: the guest has IPv6 loopback and a site-local
# address, and ip6tables starts out wide open.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

OWNERS=("$AGENT_UID")
while IFS=: read -r user start count; do
    [ "$user" = "$AGENT_USER" ] || continue
    OWNERS+=("$start-$((start + count - 1))")
done < /etc/subuid

for tables in iptables ip6tables; do
    reject=icmp-port-unreachable
    [ "$tables" = ip6tables ] && reject=icmp6-port-unreachable
    for owner in "${OWNERS[@]}"; do
        rule=(OUTPUT -m owner --uid-owner "$owner" -j REJECT --reject-with "$reject")
        if ! "$tables" -C "${rule[@]}" 2> /dev/null; then
            "$tables" -I "${rule[@]}"
        fi
    done
done
echo "[early-egress] agent uids ${OWNERS[*]} have no network until the firewall is built"
