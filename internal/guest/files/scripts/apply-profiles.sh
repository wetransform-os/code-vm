#!/bin/bash
###############################################################################
# apply-profiles.sh — apply the active customization profiles
#
# Runs as root. Two callers, one implementation:
#   * sandbox-boot.sh at boot, after the firewall is up
#   * `code-vm profile apply` on a running VM (SANDBOX_PROFILES_STRICT=1)
#
# Inputs live under /usr/local/share/sandbox-profiles, delivered root-owned by
# code-vm (mode:data at boot, staged push on apply):
#   manifest.env       ordered profile list, package union, shell, hook list
#   <name>/files/...   file tree destined for the agent home
#   <name>/files.list  the paths this profile version ships — the applier
#                      installs exactly these, so stale tree content left by
#                      an earlier version is inert
#   <name>/hook        hook script, run as the agent user
#
# File and shell failures abort (set -e): they are local and deterministic, so
# failure means a broken profile — at boot the ERR trap in sandbox-boot.sh
# turns that into a failed boot. Hook failures only warn at boot (a flaky
# download must not brick an otherwise safe VM) and are fatal in strict mode.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

PROFILE_ROOT=/usr/local/share/sandbox-profiles
MANIFEST="$PROFILE_ROOT/manifest.env"
STRICT="${SANDBOX_PROFILES_STRICT:-0}"
AGENT_HOME="/home/${AGENT_USER}"

log() { echo "[profiles] $*"; }

if [ ! -f "$MANIFEST" ]; then
    log "No profile manifest; nothing to apply"
    exit 0
fi
# shellcheck source=/dev/null
. "$MANIFEST"

# ── Packages ─────────────────────────────────────────────────────────────────
# Missing-only. At boot provision-system.sh has already installed these; on
# `profile apply` this is what installs newly declared ones. apt runs as root,
# and root egress is direct (the firewall's uid-0 ACCEPT), so no proxy needed.
MISSING=()
for p in ${PROFILE_PACKAGES:-}; do
    dpkg -s "$p" > /dev/null 2>&1 || MISSING+=("$p")
done
if [ ${#MISSING[@]} -gt 0 ]; then
    log "Installing packages: ${MISSING[*]}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq "${MISSING[@]}"
fi

# ── Files ────────────────────────────────────────────────────────────────────
# ensure_parents creates the parent directories for a home-relative path, one
# segment at a time, agent-owned. A symlinked segment aborts: these installs
# run as root, so a symlink the agent planted (~/.config -> /etc) must not be
# able to redirect one outside the home.
ensure_parents() {
    local rel_dir cur="$AGENT_HOME" seg
    rel_dir=$(dirname "$1")
    [ "$rel_dir" = "." ] && return 0
    local IFS=/
    for seg in $rel_dir; do
        [ -n "$seg" ] || continue
        cur="$cur/$seg"
        if [ -L "$cur" ]; then
            echo "[profiles] ERROR: $cur is a symlink; refusing to install through it" >&2
            return 1
        fi
        if [ ! -d "$cur" ]; then
            mkdir -m 0755 "$cur"
            chown "$AGENT_UID:$AGENT_GID" "$cur"
        fi
    done
}

# Profile files are canonical: re-installed on every boot and every apply, so
# local edits to them do not survive (same philosophy as lock-settings.sh).
# Later profiles overwrite earlier ones — list order in the config wins.
for name in ${PROFILES:-}; do
    list="$PROFILE_ROOT/$name/files.list"
    [ -f "$list" ] || continue
    while IFS= read -r rel; do
        [ -n "$rel" ] || continue
        src="$PROFILE_ROOT/$name/files/$rel"
        dst="$AGENT_HOME/$rel"
        ensure_parents "$rel"
        mode=0644
        [ -x "$src" ] && mode=0755
        # Remove first: install would write through a symlink the agent left
        # at the destination. A directory at $dst fails rm -f and aborts loud.
        rm -f "$dst"
        install -m "$mode" -o "$AGENT_UID" -g "$AGENT_GID" "$src" "$dst"
        log "  $name: ~/$rel"
    done < "$list"
done

# ── Shell ────────────────────────────────────────────────────────────────────
# Last declared shell wins (rendered into PROFILE_SHELL by code-vm). Never
# reverted when a profile is deactivated — recreate is the clean-slate path.
if [ -n "${PROFILE_SHELL:-}" ]; then
    if [ ! -x "$PROFILE_SHELL" ]; then
        echo "[profiles] ERROR: shell $PROFILE_SHELL does not exist or is not executable" >&2
        exit 1
    fi
    grep -qxF "$PROFILE_SHELL" /etc/shells || echo "$PROFILE_SHELL" >> /etc/shells
    CURRENT_SHELL=$(getent passwd "$AGENT_USER" | cut -d: -f7)
    if [ "$CURRENT_SHELL" != "$PROFILE_SHELL" ]; then
        chsh -s "$PROFILE_SHELL" "$AGENT_USER"
        log "Login shell: $PROFILE_SHELL"
    fi
fi

# ── Hooks ────────────────────────────────────────────────────────────────────
# Same hardened pattern as update-agent-clis.sh: root-driven but running as
# the agent, so nothing the agent can write may influence it — no login shell,
# system PATH only, BASH_ENV/ENV cleared. Egress goes through the proxy, so a
# hook reaches exactly what its profile's domains (plus the base allowlist)
# permit, and everything it does lands in the proxy log.
run_as_agent() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env -u BASH_ENV -u ENV \
        HOME="$AGENT_HOME" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        http_proxy=http://localhost:3128 \
        https_proxy=http://localhost:3128 \
        no_proxy=localhost,127.0.0.1 \
        bash "$1"
}

HOOK_FAILED=0
for name in ${PROFILE_HOOKS:-}; do
    hook="$PROFILE_ROOT/$name/hook"
    if [ ! -f "$hook" ]; then
        echo "[profiles] WARNING: hook for profile '$name' is missing" >&2
        HOOK_FAILED=1
        continue
    fi
    log "Running hook: $name"
    if ! (cd "$AGENT_HOME" && run_as_agent "$hook"); then
        echo "[profiles] WARNING: hook for profile '$name' failed" >&2
        HOOK_FAILED=1
    fi
done
if [ "$STRICT" = 1 ] && [ "$HOOK_FAILED" = 1 ]; then
    echo "[profiles] ERROR: one or more hooks failed" >&2
    exit 1
fi

log "Profiles applied"
