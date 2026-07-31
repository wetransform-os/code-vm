#!/bin/bash
###############################################################################
# test-vm-sandbox.sh — VM sandbox security and functionality suite
#
# Requires KVM. Run via: mise run test:vm
###############################################################################
set -uo pipefail

INSTANCE=code-sandbox
CODE_VM=./dist/code-vm
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

# Run a command as the agent user in the guest.
agent() { limactl shell "$INSTANCE" sudo /usr/local/bin/sandbox-exec "$@"; }

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
echo "================================================================"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "================================================================"
[ "$FAIL" -eq 0 ]
