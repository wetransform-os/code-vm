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

# /etc/environment sets these for every PAM session, including this
# sudo-invoked one — so without clearing them, apt-get below inherits Squid as
# its proxy instead of going direct as the uid-0 firewall rule intends, and
# fails closed because the archive mirrors are not on the allowlist.
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY

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
# `profile apply` this is what installs newly declared ones. The proxy vars
# were unset above because PAM injects them into this sudo session regardless;
# apt-get itself runs as root here, but its HTTP(S) fetch workers drop to the
# dedicated `_apt` user, which has its own direct-egress firewall rule (see
# init-firewall.sh), so no proxy is needed either way.
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

# ── Agent execution helpers ──────────────────────────────────────────────────
# Root-driven but running as the agent, so nothing the agent can write may
# influence execution: no login shell, system PATH only, BASH_ENV/ENV cleared.
# Egress goes through the proxy, so anything run this way reaches exactly what
# the active profiles' domains (plus the base allowlist) permit, and it all
# lands in the proxy log. Two variants sharing identical hardening: one runs a
# script file (hooks), the other a command string (file installs below).
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

run_as_agent_sh() {
    setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
        env -u BASH_ENV -u ENV \
        HOME="$AGENT_HOME" \
        USER="$AGENT_USER" \
        XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
        PATH=/usr/local/bin:/usr/bin:/bin \
        http_proxy=http://localhost:3128 \
        https_proxy=http://localhost:3128 \
        no_proxy=localhost,127.0.0.1 \
        bash -c "$@"
}

# ── Files ────────────────────────────────────────────────────────────────────
# Installs run with the agent's own privileges (run_as_agent_sh above), not
# root's. Source files under $PROFILE_ROOT are root-owned 0444/0555,
# world-readable, so the agent can always read them; the install itself
# (mkdir -p, rm -f, install) touches only the agent's home, using exactly the
# access the agent already has there. A symlink the agent plants at a
# destination or parent segment can therefore only redirect the write to
# somewhere the agent could write anyway — there is no privilege boundary left
# to cross, so the root-write TOCTOU class this replaces is structurally gone.

# Profile files are canonical: re-installed on every boot and every apply, so
# local edits to them do not survive (same philosophy as lock-settings.sh).
# Later profiles overwrite earlier ones — list order in the config wins.
# PROFILE_FILES (not PROFILES) drives the loop: only profiles the manifest says
# ship files are consulted, so a stale files.list left on disk by a profile
# version that has since dropped its files is never read — mode:data delivery
# can overwrite files but never delete them, and an empty files.list is not
# representable as a Lima block scalar.
for name in ${PROFILE_FILES:-}; do
    list="$PROFILE_ROOT/$name/files.list"
    [ -f "$list" ] || continue
    while IFS= read -r rel; do
        [ -n "$rel" ] || continue
        src="$PROFILE_ROOT/$name/files/$rel"
        dst="$AGENT_HOME/$rel"
        mode=0644
        [ -x "$src" ] && mode=0755
        # Positional args, not string interpolation into the bash -c body:
        # robust regardless of charset, though $src/$dst are host-validated
        # ([a-zA-Z0-9._/-] for $rel; $AGENT_HOME/$PROFILE_ROOT are fixed safe
        # paths) so this belt-and-braces quoting is defense in depth.
        # shellcheck disable=SC2016 # the inner bash -c program expands its own args
        run_as_agent_sh 'mkdir -p "$(dirname "$2")" && rm -f "$2" && install -m "$1" "$3" "$2"' _ "$mode" "$dst" "$src"
        log "  $name: ~/$rel"
    done < "$list"
done

# ── Shell ────────────────────────────────────────────────────────────────────
# Last declared shell wins (rendered into PROFILE_SHELL by code-vm). Never
# reverted when a profile is deactivated — recreate is the clean-slate path.
if [ -n "${PROFILE_SHELL:-}" ]; then
    # -f as well as -x: -x alone is true for searchable directories, and a
    # directory in /etc/shells + passwd bricks every later session (exit 126).
    if [ ! -f "$PROFILE_SHELL" ] || [ ! -x "$PROFILE_SHELL" ]; then
        echo "[profiles] ERROR: shell $PROFILE_SHELL is not an executable file" >&2
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
# Same hardened pattern as the files step above (via run_as_agent, its
# script-file counterpart to run_as_agent_sh): root-driven but running as the
# agent, so nothing the agent can write may influence it — no login shell,
# system PATH only, BASH_ENV/ENV cleared. Egress goes through the proxy, so a
# hook reaches exactly what its profile's domains (plus the base allowlist)
# permit, and everything it does lands in the proxy log.
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
