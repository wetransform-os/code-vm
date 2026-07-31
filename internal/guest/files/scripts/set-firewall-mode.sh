#!/bin/bash
###############################################################################
# set-firewall-mode.sh — switch the egress firewall mode and reapply it
#
# Runs as root, invoked by code-vm through limaadmin. The agent has no sudo and
# therefore cannot call this.
#
# The mode file lives in /run (tmpfs), so a VM restart always reverts to
# allowlist.
###############################################################################
set -euo pipefail

MODE=${1:-}
case "$MODE" in
    allowlist | audit | open) ;;
    *)
        echo "usage: set-firewall-mode.sh [allowlist|audit|open]" >&2
        exit 2
        ;;
esac

install -d -m 0755 /run/sandbox
printf '%s\n' "$MODE" > /run/sandbox/firewall-mode
chmod 0444 /run/sandbox/firewall-mode

# init-firewall.sh regenerates squid.conf and every iptables rule from scratch,
# so reapplying it is the whole mode switch.
exec /usr/local/lib/sandbox/init-firewall.sh
