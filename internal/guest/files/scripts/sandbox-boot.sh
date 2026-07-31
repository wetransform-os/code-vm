#!/bin/bash
###############################################################################
# sandbox-boot.sh — the VM's equivalent of the container's entrypoint.sh
#
# Ordered sequence, run as root once provisioning has finished:
#   1. update the agent CLIs   — needs unrestricted egress
#   2. lock the Claude config  — must precede any agent process
#   3. initialise the firewall — closes egress, so it goes last
#
# The order is load-bearing. It is the same order entrypoint.sh uses in the
# container sandbox, for the same reason.
###############################################################################
set -euo pipefail

echo "[boot] Sandbox boot sequence starting"

/usr/local/lib/sandbox/update-agent-clis.sh
/usr/local/lib/sandbox/lock-settings.sh
/usr/local/lib/sandbox/init-firewall.sh

echo "[boot] Sandbox boot sequence complete"
