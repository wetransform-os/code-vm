#!/bin/bash
###############################################################################
# update-agent-clis.sh — install or update Claude Code and OpenCode
#
# Runs as root from sandbox-boot.sh AFTER the firewall is up, so the installers
# go through Squid like everything else: their hosts are allowlisted, and a
# compromised installer is confined and logged rather than free. Failures are
# warnings, not errors — an offline boot, or a vendor moving a download host,
# must not brick the VM. `code-vm proxy-log denied` names a blocked host and
# `code-vm allow` fixes it.
#
# Nothing the agent can write may influence this step. It runs as the agent but
# is driven by root, so an injection here would execute with root's intent, and
# for most of this project's life it also ran before the firewall existed. Three
# things keep the agent out, and all three are load-bearing:
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
    # The proxy is set explicitly rather than inherited: sandbox-boot.sh runs
    # from systemd, which never reads /etc/environment, so without these the
    # installers would try to connect directly and be rejected by iptables.
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env -u BASH_ENV -u ENV \
        HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        http_proxy=http://localhost:3128 \
        https_proxy=http://localhost:3128 \
        no_proxy=localhost,127.0.0.1 \
        bash -c "$1"
}

echo "[boot] Updating agent CLIs"
run_as_agent 'curl -fsSL https://claude.ai/install.sh | bash' ||
    echo "[boot] WARNING: Claude Code install/update failed"
run_as_agent 'curl -fsSL https://opencode.ai/install | bash' ||
    echo "[boot] WARNING: OpenCode install/update failed"
