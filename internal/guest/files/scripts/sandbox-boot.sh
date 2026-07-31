#!/bin/bash
###############################################################################
# sandbox-boot.sh — the VM's equivalent of the container's entrypoint.sh
#
# Ordered sequence, run as root after provisioning completes:
#   1. update the agent CLIs      (needs unrestricted egress)
#   2. lock the Claude settings   (Task 6)
#   3. initialise the firewall    (Task 6 — closes egress, so it goes last)
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

echo "[boot] Sandbox boot sequence starting"

# Placeholder until Task 6 lands the firewall. The readiness probe waits on
# this file, so it must exist for `limactl start` to return.
echo "PLACEHOLDER=yes" > /run/firewall-verify
chmod 0444 /run/firewall-verify

echo "[boot] Sandbox boot sequence complete"
