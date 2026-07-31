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

FAIL_MARKER=/run/sandbox-boot-failed
rm -f "$FAIL_MARKER"

# init-firewall.sh withholds /run/firewall-verify when its own checks fail, and
# the Lima readiness probe waits for that file — so a failure already blocks the
# VM from being reported ready. Without this marker the probe would only find
# out by timing out, several minutes later, with nothing to point at. Touching
# it lets the probe fail immediately and name the step that broke.
on_failure() {
    echo "[boot] FAILED at line $1 — the sandbox is not safe to use" >&2
    : > "$FAIL_MARKER"
}
trap 'on_failure $LINENO' ERR

echo "[boot] Sandbox boot sequence starting"

/usr/local/lib/sandbox/update-agent-clis.sh
/usr/local/lib/sandbox/lock-settings.sh
/usr/local/lib/sandbox/init-firewall.sh

echo "[boot] Sandbox boot sequence complete"
