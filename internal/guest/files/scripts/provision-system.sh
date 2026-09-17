#!/bin/bash
###############################################################################
# provision-system.sh — root provisioning for the code-vm sandbox VM
#
# Runs on every boot via Lima `provision: mode: system`, after the mode:data
# files have been written. Every step is idempotent.
#
# Inputs come from /etc/sandbox/provision.env, a mode:data file rendered by
# code-vm: AGENT_USER, AGENT_UID, AGENT_GID, EXTRA_ALLOWED_DOMAINS,
# CONTAINER_PROXY, SWAP_SIZE.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

log() { echo "[provision] $*"; }

export DEBIAN_FRONTEND=noninteractive

# ── Agent user ───────────────────────────────────────────────────────────────
# UID/GID mirror the host user so virtiofs-shared workspace files are genuinely
# owned by the agent, and stay host-owned when viewed from the host.
# The agent's primary group must carry the host's GID so virtiofs ownership
# lines up. Its *name* is whatever the guest already calls that GID: stock
# groups occupy low GIDs (users=100 on many Linux distros, 20 for macOS hosts),
# and a second group with a duplicate GID would leave `stat -c %G` reporting the
# pre-existing name anyway. Nothing may assume this group is called
# "$AGENT_USER" — every consumer chowns by numeric GID instead.
if getent group "$AGENT_GID" > /dev/null; then
    log "Group for gid $AGENT_GID already exists: $(getent group "$AGENT_GID" | cut -d: -f1)"
else
    groupadd -g "$AGENT_GID" "$AGENT_USER"
    log "Created group $AGENT_USER (gid=$AGENT_GID)"
fi

# A different account already on the host's UID would make the agent share an
# identity with a guest user. Nothing in the stock image occupies it, so this is
# a loud failure rather than a silent reuse.
# `|| true` is required, not defensive noise: getent exits 2 when nothing matches,
# and under `set -euo pipefail` that status propagates out of the pipeline and
# aborts provisioning on the very first boot, when the agent does not exist yet.
EXISTING_UID_USER=$(getent passwd "$AGENT_UID" | cut -d: -f1 || true)
if [ -n "$EXISTING_UID_USER" ] && [ "$EXISTING_UID_USER" != "$AGENT_USER" ]; then
    echo "[provision] ERROR: uid $AGENT_UID already belongs to '$EXISTING_UID_USER';" >&2
    echo "[provision]        cannot create $AGENT_USER with the host user's UID." >&2
    exit 1
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

# ── Profile packages ─────────────────────────────────────────────────────────
# Declared by active profiles; manifest.env is delivered as mode:data before
# provisioning runs. Installed here, pre-firewall, like the base packages.
# apply-profiles.sh repeats a missing-only install for the `profile apply`
# path on a running VM.
PROFILE_MANIFEST=/usr/local/share/sandbox-profiles/manifest.env
if [ -f "$PROFILE_MANIFEST" ]; then
    # shellcheck source=/dev/null
    . "$PROFILE_MANIFEST"
    PROFILE_MISSING=()
    for p in ${PROFILE_PACKAGES:-}; do
        dpkg -s "$p" > /dev/null 2>&1 || PROFILE_MISSING+=("$p")
    done
    if [ ${#PROFILE_MISSING[@]} -gt 0 ]; then
        log "Installing profile packages: ${PROFILE_MISSING[*]}"
        apt-get update -qq
        apt-get install -y -qq "${PROFILE_MISSING[@]}"
    fi
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

# ── /tmp on disk ─────────────────────────────────────────────────────────────
# The stock image mounts /tmp as a tmpfs sized at half of RAM (tmp.mount, a
# static unit). Agent scratch written there — Claude Code's own scratchpad
# among it — then competes with the workload for memory: 3.7 GB of scratch on
# a 12 GB guest was what tipped a Gradle-plus-Testcontainers run into the OOM
# killer. On the root filesystem the same scratch is just disk, which
# systemd-tmpfiles still ages out (stock tmp.conf: 10 days).
#
# The mask is what persists across boots. Unmounting here as well makes it
# effective on this very boot: provisioning runs before anything
# sandbox-specific has touched /tmp, so the tmpfs is empty and unused. If it
# is busy, it is left alone (a lazy unmount would let its holders keep
# writing into an orphaned tmpfs) and takes effect on the next boot; the
# marker tells the integration suite which of the two happened.
#
# The mask's result is checked rather than assumed: the unmount below would
# make this boot look right even if the mask silently failed, and the tmpfs
# would be back on the next one.
systemctl mask tmp.mount > /dev/null 2>&1 || true
if [ "$(systemctl is-enabled tmp.mount 2> /dev/null || true)" != "masked" ]; then
    log "WARNING: could not mask tmp.mount; /tmp returns to tmpfs on the next boot"
fi
install -d -m 0755 /run/sandbox
rm -f /run/sandbox/tmp-unmount-deferred
if [ "$(findmnt -no FSTYPE /tmp 2> /dev/null || true)" = "tmpfs" ]; then
    if umount /tmp 2> /dev/null; then
        chmod 1777 /tmp
        log "/tmp: tmpfs unmounted; now on the root filesystem"
    else
        touch /run/sandbox/tmp-unmount-deferred
        log "/tmp: tmpfs is busy, stays until the next boot (tmp.mount is masked)"
    fi
fi

# ── Swap ─────────────────────────────────────────────────────────────────────
# The image ships without swap, so a memory burst goes straight to the OOM
# killer. That picks dockerd (highest oom_score_adj), the workload restarts
# it, and the loop leaves the guest unresponsive. A swapfile on the guest disk
# turns the burst into slowness instead. It also changes what the agent
# slice's MemoryMax above does: with swap present, the slice hitting its
# limit swaps (MemorySwapMax is unlimited) instead of being OOM-killed.
#
# SWAP_SIZE is a Lima-style size ("4GiB") validated on the host; "0B" means
# no swap. A wrong-sized swapfile is recreated so a config change takes
# effect on the next start. Swap is a mitigation, not a prerequisite, so
# failing to set it up is logged and provisioning continues.
SWAPFILE=/swapfile
SWAP_HAVE=0
[ -f "$SWAPFILE" ] && SWAP_HAVE=$(stat -c %s "$SWAPFILE")
swap_active() { swapon --show=NAME --noheadings | grep -qx "$SWAPFILE"; }
# The host bounds the value, so this only fails on a hand-edited provision.env.
# An unparseable size is not a request for no swap: keep whatever exists
# rather than delete it, and skip the reconcile below by wanting what we have.
if ! SWAP_WANT=$(numfmt --from=iec-i "${SWAP_SIZE%B}" 2> /dev/null); then
    log "WARNING: cannot parse SWAP_SIZE=${SWAP_SIZE}; leaving swap as it is"
    SWAP_WANT=$SWAP_HAVE
fi
if [ "$SWAP_WANT" -eq 0 ] || [ "$SWAP_HAVE" -ne "$SWAP_WANT" ]; then
    if [ -f "$SWAPFILE" ]; then
        if swap_active && ! swapoff "$SWAPFILE"; then
            log "WARNING: swapoff $SWAPFILE failed; keeping the existing $(numfmt --to=iec-i "$SWAP_HAVE")B swapfile"
        else
            rm -f "$SWAPFILE"
            SWAP_HAVE=0
        fi
    fi
fi
if [ "$SWAP_WANT" -ne 0 ] && [ "$SWAP_HAVE" -eq 0 ]; then
    log "Creating ${SWAP_SIZE} swapfile at $SWAPFILE"
    # fallocate is fine for ext4, which the image's root filesystem is.
    if fallocate -l "$SWAP_WANT" "$SWAPFILE" && chmod 0600 "$SWAPFILE" && mkswap -q "$SWAPFILE"; then
        SWAP_HAVE=$SWAP_WANT
    else
        log "WARNING: could not create the swapfile; the guest runs without swap"
        rm -f "$SWAPFILE"
    fi
fi
# Activate first, persist second: fstab mirrors what is actually working,
# not what was attempted. An entry for a swapfile that fails to activate
# would give every later boot a failing swap unit, and since the file would
# already be the right size, provisioning would never rebuild it either. So
# on activation failure the file goes too, and the next boot starts clean.
if [ "$SWAP_HAVE" -ne 0 ] && ! swap_active && ! swapon "$SWAPFILE"; then
    log "WARNING: swapon $SWAPFILE failed; removing it, the guest runs without swap"
    rm -f "$SWAPFILE"
    SWAP_HAVE=0
fi
if [ "$SWAP_HAVE" -eq 0 ]; then
    sed -i "\|^${SWAPFILE} |d" /etc/fstab
else
    grep -q "^${SWAPFILE} " /etc/fstab || echo "${SWAPFILE} none swap sw 0 0" >> /etc/fstab
fi

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
