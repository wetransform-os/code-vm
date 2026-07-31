#!/bin/bash
###############################################################################
# update-agent-clis.sh — install or update Claude Code and OpenCode
#
# Runs as root from sandbox-boot.sh BEFORE the firewall closes, because the
# installers fetch from the network. Failures are warnings, not errors: an
# offline boot must not brick the VM.
###############################################################################
set -uo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

run_as_agent() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH="/home/${AGENT_USER}/.local/bin:/usr/local/bin:/usr/bin:/bin" \
        bash -lc "$1"
}

echo "[boot] Updating agent CLIs"
run_as_agent 'curl -fsSL https://claude.ai/install.sh | bash' ||
    echo "[boot] WARNING: Claude Code install/update failed"
run_as_agent 'curl -fsSL https://opencode.ai/install | bash' ||
    echo "[boot] WARNING: OpenCode install/update failed"
