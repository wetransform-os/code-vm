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

# Needed for AGENT_UID/AGENT_GID/AGENT_USER in the connectivity check below.
# Under `set -u` an unset one would abort the boot and trip the failure marker.
# shellcheck source=/dev/null
. /etc/sandbox/provision.env

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

# Rule verification proves the firewall has the shape it intends, not that
# anything can actually get out through it — DNS resolution or Squid itself can
# be broken while every rule is present and correct. This reaches the API the
# way the agent does, as the agent, through the proxy, so the common breakages
# announce themselves at boot instead of surfacing later as an agent that hangs.
#
# Deliberately not fatal, and deliberately inside an `if` so the ERR trap above
# does not treat it as a failed boot: an offline or air-gapped start should warn,
# not withhold the VM. No -f, because the API root answers 404 — any completed
# exchange proves the path works.
echo "[boot] Verifying the agent can reach the API through the proxy"
if setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
    env -u BASH_ENV -u ENV \
    HOME="/home/${AGENT_USER}" \
    PATH=/usr/local/bin:/usr/bin:/bin \
    https_proxy=http://localhost:3128 \
    curl -sS -o /dev/null --max-time 20 https://api.anthropic.com > /dev/null 2>&1; then
    echo "[boot] API reachable through the proxy"
else
    echo "[boot] WARNING: cannot reach api.anthropic.com through the proxy." >&2
    echo "[boot]          The firewall rules verified, so this is DNS, Squid or" >&2
    echo "[boot]          upstream connectivity. Check: code-vm proxy-log denied" >&2
fi

echo "[boot] Sandbox boot sequence complete"
