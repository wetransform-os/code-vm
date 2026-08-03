#!/bin/bash
###############################################################################
# sandbox-boot.sh — the VM's equivalent of the container's entrypoint.sh
#
# Ordered sequence, run as root once provisioning has finished:
#   1. lock the Claude config  — must precede any agent process
#   2. initialise the firewall — nothing below it runs unfiltered
#   3. check the agent's egress path actually works
#   4. update the agent CLIs   — through the proxy, like everything else
#
# The container sandbox updates the CLIs first, because it assumed they needed
# unrestricted egress. They do not: every host the installers use is
# allowlisted, so they work through the proxy. Running them last means no step
# in this VM's life has unfiltered egress, and a compromised installer is
# confined to the allowlist and recorded in the proxy log like any other
# traffic. The cost is that a vendor moving a download host turns into a failed
# update rather than an invisible one — loud, and fixable with `code-vm allow`.
###############################################################################
set -euo pipefail

# Needed for AGENT_UID/AGENT_GID/AGENT_USER in the connectivity check below.
# Under `set -u` an unset one would abort the boot and trip the failure marker.
# shellcheck source=/dev/null
. /etc/sandbox/provision.env

FAIL_MARKER=/run/sandbox-boot-failed
DONE_MARKER=/run/sandbox-boot-complete
rm -f "$FAIL_MARKER" "$DONE_MARKER"

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

# Last, and through the proxy. Placed after the connectivity check so that when
# the installers fail, the warning above has already said whether egress works
# at all.
/usr/local/lib/sandbox/update-agent-clis.sh

# The readiness probe waits for this, not for the firewall's own verify file.
# Those were the same thing while the CLI update ran first; now that it runs
# last, gating on the firewall would let `limactl start` return while the
# installers were still downloading — the agent could be asked to run a CLI that
# was not there yet, and a prompt `code-vm stop` killed the install mid-flight.
: > "$DONE_MARKER"
chmod 0444 "$DONE_MARKER"

echo "[boot] Sandbox boot sequence complete"
