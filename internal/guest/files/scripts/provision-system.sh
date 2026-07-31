#!/bin/bash
###############################################################################
# provision-system.sh — root provisioning for the code-vm sandbox VM
#
# Runs on every boot via Lima `provision: mode: system`, after the mode:data
# files have been written. Every step is idempotent.
#
# Inputs come from /etc/sandbox/provision.env, a mode:data file rendered by
# code-vm: AGENT_USER, AGENT_UID, AGENT_GID, EXTRA_ALLOWED_DOMAINS,
# CONTAINER_PROXY.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

log() { echo "[provision] $*"; }

export DEBIAN_FRONTEND=noninteractive

# ── Agent user ───────────────────────────────────────────────────────────────
# UID/GID mirror the host user so virtiofs-shared workspace files are genuinely
# owned by the agent, and stay host-owned when viewed from the host.
if ! getent group "$AGENT_GID" > /dev/null; then
    groupadd -g "$AGENT_GID" "$AGENT_USER"
fi
if ! id -u "$AGENT_USER" > /dev/null 2>&1; then
    useradd -m -u "$AGENT_UID" -g "$AGENT_GID" -s /bin/bash "$AGENT_USER"
    log "Created $AGENT_USER (uid=$AGENT_UID gid=$AGENT_GID)"
fi

# The agent must never hold sudo. Re-asserted on every boot, not just creation.
deluser "$AGENT_USER" sudo > /dev/null 2>&1 || true
rm -f "/etc/sudoers.d/${AGENT_USER}" "/etc/sudoers.d/99-${AGENT_USER}"

# Subordinate ID ranges for rootless Docker's user namespaces.
grep -q "^${AGENT_USER}:" /etc/subuid || echo "${AGENT_USER}:100000:65536" >> /etc/subuid
grep -q "^${AGENT_USER}:" /etc/subgid || echo "${AGENT_USER}:100000:65536" >> /etc/subgid

# Keep the agent's systemd user instance alive without a login session, so
# rootless dockerd survives between code-vm invocations.
loginctl enable-linger "$AGENT_USER"

# ── Packages ─────────────────────────────────────────────────────────────────
NEEDED=(uidmap dbus-user-session iptables squid util-linux git jq curl ca-certificates)
MISSING=()
for p in "${NEEDED[@]}"; do
    dpkg -s "$p" > /dev/null 2>&1 || MISSING+=("$p")
done
if [ ${#MISSING[@]} -gt 0 ]; then
    log "Installing packages: ${MISSING[*]}"
    apt-get update -qq
    apt-get install -y -qq "${MISSING[@]}"
fi

# Rootless Docker manages iptables inside its own network namespace.
modprobe ip_tables > /dev/null 2>&1 || true
modprobe iptable_nat > /dev/null 2>&1 || true
modprobe ip6_tables > /dev/null 2>&1 || true

# ── mise ─────────────────────────────────────────────────────────────────────
# Available to the agent for project toolchains. yq and gomplate used to be
# installed here for credential rendering; that mechanism was removed, and
# nothing in the guest uses them now.
if [ ! -x /usr/local/bin/mise ]; then
    log "Installing mise"
    curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh
fi

# ── Docker ───────────────────────────────────────────────────────────────────
if ! command -v docker > /dev/null 2>&1; then
    log "Installing Docker"
    curl -fsSL https://get.docker.com | sh
fi
# The rootful daemon is never used: the agent runs its own rootless dockerd,
# which is what keeps guest root separated from the agent.
systemctl disable --now docker.service docker.socket containerd.service containerd.socket > /dev/null 2>&1 || true
systemctl mask docker.service docker.socket containerd.service containerd.socket > /dev/null 2>&1 || true

# ── Sandbox-managed environment ──────────────────────────────────────────────
# Single source of truth for proxy and Docker env. sandbox-exec sources this
# because `limactl shell` launches a non-login shell that would never read it.
cat > /etc/environment << EOF
http_proxy=http://localhost:3128
https_proxy=http://localhost:3128
HTTP_PROXY=http://localhost:3128
HTTPS_PROXY=http://localhost:3128
no_proxy=localhost,127.0.0.1
NO_PROXY=localhost,127.0.0.1
JAVA_TOOL_OPTIONS="-Dhttp.proxyHost=localhost -Dhttp.proxyPort=3128 -Dhttps.proxyHost=localhost -Dhttps.proxyPort=3128 -Dhttp.nonProxyHosts=localhost|127.0.0.1"
DOCKER_HOST=unix:///run/user/${AGENT_UID}/docker.sock
EOF

# ── Resource limits ──────────────────────────────────────────────────────────
# Replaces the container sandbox's --pids-limit. MemoryMax leaves headroom for
# the guest OS and Squid.
TOTAL_KB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)
MEM_MAX_MB=$((TOTAL_KB / 1024 - 2048))
[ "$MEM_MAX_MB" -lt 1024 ] && MEM_MAX_MB=1024
install -d "/etc/systemd/system/user-${AGENT_UID}.slice.d"
cat > "/etc/systemd/system/user-${AGENT_UID}.slice.d/50-sandbox.conf" << EOF
[Slice]
TasksMax=2048
MemoryMax=${MEM_MAX_MB}M
EOF
systemctl daemon-reload

# ── Rootless Docker for the agent ────────────────────────────────────────────
# Lima's `mode: user` scripts run as limaadmin, so the agent's rootless setup is
# driven from here into the agent's own systemd user session.
if [ ! -S "/run/user/${AGENT_UID}/docker.sock" ]; then
    log "Setting up rootless Docker for $AGENT_USER"
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups --reset-env \
        env HOME="/home/${AGENT_USER}" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin \
        CONTAINER_PROXY="$CONTAINER_PROXY" \
        bash /usr/local/lib/sandbox/provision-user-docker.sh
fi

# ── Boot sequence ────────────────────────────────────────────────────────────
# sandbox-boot.service is ordered after cloud-final.service so on later boots it
# runs only once provisioning has finished — provisioning needs unrestricted
# egress for apt and get.docker.com, and the firewall closes at the end of the
# boot sequence. Enabling a unit mid-boot does not queue it for this boot, so
# start it explicitly here too.
systemctl enable sandbox-boot.service > /dev/null 2>&1 || true
systemctl start --no-block sandbox-boot.service

log "Provisioning complete"
