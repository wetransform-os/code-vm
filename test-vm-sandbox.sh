#!/bin/bash
###############################################################################
# test-vm-sandbox.sh — VM sandbox security and functionality suite
#
# Requires KVM. Run via: mise run test:vm
###############################################################################
set -uo pipefail

INSTANCE=code-sandbox
# Absolute path: several assertions cd elsewhere before invoking it.
CODE_VM="$(pwd)/dist/code-vm"
AGENT_USER=devuser

PASS=0
FAIL=0

pass() {
    PASS=$((PASS + 1))
    echo "  PASS: $1"
}

fail() {
    FAIL=$((FAIL + 1))
    echo "  FAIL: $1"
}

# Run a command as root in the guest.
adm() { limactl shell "$INSTANCE" sudo "$@"; }

# Run a command as the agent user through the real CLI.
agent() { "$CODE_VM" -- "$@"; }

assert_ok() {
    local desc="$1"
    shift
    if "$@" > /dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

assert_fails() {
    local desc="$1"
    shift
    if "$@" > /dev/null 2>&1; then fail "$desc (command unexpectedly succeeded)"; else pass "$desc"; fi
}

echo ""
echo "================================================================"
echo "  VM Sandbox Test Suite"
echo "================================================================"
echo ""

echo "[test] Building code-vm..."
if mise run build; then pass "code-vm builds"; else fail "code-vm builds"; exit 1; fi

echo "[test] Starting the sandbox VM (first boot provisions; this takes minutes)..."
if "$CODE_VM" start; then pass "VM starts"; else fail "VM starts"; exit 1; fi

echo ""
echo "── Agent user isolation ──────────────────────────────────────────"

assert_fails "agent cannot sudo" \
    limactl shell "$INSTANCE" sudo -u "$AGENT_USER" sudo -n true

if [ "$(adm id -u "$AGENT_USER")" = "$(id -u)" ]; then
    pass "agent UID matches the host user"
else
    fail "agent UID matches the host user (guest=$(adm id -u "$AGENT_USER") host=$(id -u))"
fi

if adm id -nG "$AGENT_USER" | tr ' ' '\n' | grep -qx sudo; then
    fail "agent is not in the sudo group"
else
    pass "agent is not in the sudo group"
fi

echo ""
echo "── Host filesystem exposure ──────────────────────────────────────"

assert_fails "host \$HOME is not mounted in the guest" \
    adm test -d "$HOME/.ssh"

PROJECTS_ROOT=$(sed -n 's/^projectsRoot: *//p' "${HOME}/.config/code-vm/config.yaml" 2>/dev/null)
PROJECTS_ROOT=${PROJECTS_ROOT:-$HOME/projects}
PROJECTS_ROOT=${PROJECTS_ROOT/#\~/$HOME}
assert_ok "projects root is mounted" adm test -d "$PROJECTS_ROOT"

echo ""
echo "── Docker ────────────────────────────────────────────────────────"

assert_ok "rootless docker responds to the agent" agent docker info
assert_fails "rootful docker.service is masked" adm systemctl is-enabled docker.service

if adm test -S "/run/user/$(id -u)/docker.sock"; then
    pass "rootless docker socket exists at the agent's runtime dir"
else
    fail "rootless docker socket exists at the agent's runtime dir"
fi

echo ""
echo "── Exec path ─────────────────────────────────────────────────────"

if [ "$(agent id -u)" = "$(id -u)" ]; then
    pass "code-vm runs as the agent user with the host UID"
else
    fail "code-vm runs as the agent user with the host UID"
fi

if [ "$(agent id -un)" = "$AGENT_USER" ]; then
    pass "code-vm runs as $AGENT_USER"
else
    fail "code-vm runs as $AGENT_USER (got $(agent id -un))"
fi

WORK_SUBDIR="$PROJECTS_ROOT/.code-vm-test-cwd"
mkdir -p "$WORK_SUBDIR"
if [ "$(cd "$WORK_SUBDIR" && agent pwd)" = "$WORK_SUBDIR" ]; then
    pass "working directory is preserved into the guest"
else
    fail "working directory is preserved into the guest"
fi
rmdir "$WORK_SUBDIR"

if agent env | grep -q '^DOCKER_HOST=unix:///run/user/'; then
    pass "DOCKER_HOST is exported to the agent"
else
    fail "DOCKER_HOST is exported to the agent"
fi

if agent env | grep -q '^https_proxy=http://localhost:3128$'; then
    pass "proxy env is exported to the agent"
else
    fail "proxy env is exported to the agent"
fi

# Captured first: code-vm exits non-zero here by design, and under pipefail
# that would fail the pipeline even though grep matches.
UNCOVERED_OUT=$( (cd /tmp && "$CODE_VM" -- true) 2>&1 )
if echo "$UNCOVERED_OUT" | grep -q "code-vm mount"; then
    pass "running outside a shared directory fails with actionable advice"
else
    fail "running outside a shared directory fails with actionable advice"
fi

echo ""
echo "================================================================"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "================================================================"
[ "$FAIL" -eq 0 ]
