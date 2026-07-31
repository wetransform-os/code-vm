#!/bin/bash
###############################################################################
# update-agent-clis.sh — install or update Claude Code and OpenCode
#
# Runs as root from sandbox-boot.sh BEFORE the firewall closes, because the
# installers fetch from the network. Failures are warnings, not errors: an
# offline boot must not brick the VM.
#
# Nothing the agent can write may influence this step. It is the one moment in
# the VM's life with unrestricted egress, so anything the agent could inject
# here would run before the allowlist exists and exfiltrate without leaving a
# proxy-log entry. Three things keep it out, and all three are load-bearing:
#
#   * `bash -c`, never `-lc`. A login shell sources the agent's ~/.profile and
#     ~/.bashrc, which it owns and can append to during any session.
#   * a PATH of system directories only. The agent's ~/.local/bin used to come
#     first, so a planted ~/.local/bin/curl shadowed the real one and ran here.
#     The installers write into $HOME regardless of what is on PATH.
#   * BASH_ENV and ENV cleared. They are the one file a non-interactive shell
#     still sources, and inheriting either from an unexpected place would undo
#     the first point.
###############################################################################
set -uo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

run_as_agent() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env -u BASH_ENV -u ENV \
        HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        bash -c "$1"
}

echo "[boot] Updating agent CLIs"
run_as_agent 'curl -fsSL https://claude.ai/install.sh | bash' ||
    echo "[boot] WARNING: Claude Code install/update failed"
run_as_agent 'curl -fsSL https://opencode.ai/install | bash' ||
    echo "[boot] WARNING: OpenCode install/update failed"
