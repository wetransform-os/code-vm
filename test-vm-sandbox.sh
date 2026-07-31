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

# Pre-clean state a previous warm-VM suite run may have left behind, so the
# egress assertions below see boot-fresh conditions. A clean guest is a no-op.
adm sh -c 'rm -f /run/sandbox/squid-allow.d/10-*.conf && squid -k reconfigure' > /dev/null 2>&1 || true

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
echo "── Firewall ──────────────────────────────────────────────────────"

VERIFY=$(adm cat /run/firewall-verify)
for kv in "OUTPUT_POLICY=DROP" "UDP_DROP=yes" "PROXY_UID_RULE=yes" \
    "AGENT_GATEWAY_REJECT=yes" "SQUID_RUNNING=yes"; do
    if echo "$VERIFY" | grep -qx "$kv"; then
        pass "firewall self-verify: $kv"
    else
        fail "firewall self-verify: $kv (got: $(echo "$VERIFY" | tr '\n' ' '))"
    fi
done

# No -f here: the API root returns 404, and for HTTPS a Squid denial breaks
# the CONNECT tunnel itself, so any completed HTTP exchange proves reachability.
assert_ok "allowlisted domain reachable through the proxy" \
    agent curl -sS -o /dev/null --max-time 20 https://api.anthropic.com
assert_fails "non-allowlisted domain blocked" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "direct egress bypassing the proxy is blocked" \
    agent env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
    curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "DNS tunneling to an external resolver is blocked" \
    agent timeout 10 nslookup example.org 1.1.1.1

if [ "$(adm sh -c 'ls /run/sandbox/squid-allow.d | tr "\n" " "')" = "00-base.conf " ]; then
    pass "allowlist fragment dir holds only the base fragment after boot"
else
    fail "allowlist fragment dir holds only the base fragment after boot"
fi

echo ""
echo "── Settings lock ─────────────────────────────────────────────────"

SETTINGS="/home/$AGENT_USER/.claude/settings.json"
if [ "$(adm stat -c '%U:%G %a' "$SETTINGS")" = "root:$AGENT_USER 444" ]; then
    pass "settings.json is root-owned and read-only"
else
    fail "settings.json is root-owned and read-only (got $(adm stat -c '%U:%G %a' "$SETTINGS"))"
fi

assert_fails "agent cannot write settings.json" \
    agent bash -c "echo '{}' > $SETTINGS"
assert_fails "agent cannot write settings.local.json" \
    agent bash -c "echo '{}' > /home/$AGENT_USER/.claude/settings.local.json"
assert_fails "agent cannot write /etc" \
    agent bash -c "echo x > /etc/code-vm-probe"

echo ""
echo "── Docker networking ─────────────────────────────────────────────"

assert_ok "docker build works" \
    agent bash -c 'printf "FROM alpine:3.23\nRUN echo ok\n" | docker build -q -t code-vm-test:latest -'
# Rootless dockerd ACCEPTS --privileged, but the capabilities it grants are
# confined to the user namespace (observed: the design expected an outright
# refusal). The invariant that matters is that a privileged container still
# cannot touch host kernel state.
assert_fails "privileged containers cannot write host kernel state" \
    agent docker run --rm --privileged alpine:3.23 sh -c 'echo 1 > /proc/sys/kernel/sysrq'

COMPOSE_DIR="$PROJECTS_ROOT/.code-vm-compose-test"
mkdir -p "$COMPOSE_DIR"
cat > "$COMPOSE_DIR/compose.yaml" << 'YAML'
services:
  server:
    image: alpine:3.23
    command: ["sleep", "60"]
  client:
    image: alpine:3.23
    command: ["sleep", "60"]
YAML
(cd "$COMPOSE_DIR" && agent docker compose up -d > /dev/null 2>&1)
if (cd "$COMPOSE_DIR" && agent docker compose exec -T client getent hosts server > /dev/null 2>&1); then
    pass "compose service-name DNS resolves"
else
    fail "compose service-name DNS resolves"
fi
(cd "$COMPOSE_DIR" && agent docker compose down -v > /dev/null 2>&1)
rm -rf "$COMPOSE_DIR"

echo ""
echo "── Session setup ─────────────────────────────────────────────────"
# The widened domain persists for the VM's lifetime, so this section must run
# AFTER the egress assertions above; the restart-hygiene section at the end
# verifies the fragment (and the widening) disappears on reboot.

DOMAIN_TEST_DIR="$PROJECTS_ROOT/.code-vm-domains-test"
mkdir -p "$DOMAIN_TEST_DIR"
echo ".example.org" > "$DOMAIN_TEST_DIR/.sandbox-domains"

if (cd "$DOMAIN_TEST_DIR" && "$CODE_VM" -- curl -fsS -o /dev/null --max-time 20 https://example.org); then
    pass "per-workspace .sandbox-domains widens the allowlist"
else
    fail "per-workspace .sandbox-domains widens the allowlist"
fi

FRAGMENTS=$(adm sh -c 'ls /run/sandbox/squid-allow.d')
if echo "$FRAGMENTS" | grep -q '^10-.*\.conf$'; then
    pass "workspace fragment is installed"
else
    fail "workspace fragment is installed (got: $(echo "$FRAGMENTS" | tr '\n' ' '))"
fi

if [ "$(adm stat -c '%U:%G %a' "/run/sandbox/squid-allow.d/$(echo "$FRAGMENTS" | grep '^10-' | head -1)")" = "root:root 444" ]; then
    pass "fragment is root-owned and read-only"
else
    fail "fragment is root-owned and read-only"
fi

HOST_GIT_EMAIL=$(git config --get user.email || true)
if [ -n "$HOST_GIT_EMAIL" ]; then
    if agent git config --get user.email | grep -qx "$HOST_GIT_EMAIL"; then
        pass "host git identity is seeded into the guest"
    else
        fail "host git identity is seeded into the guest"
    fi
fi

rm -rf "$DOMAIN_TEST_DIR"

echo ""
echo "── Credential injection ──────────────────────────────────────────"

CRED_DIR="$PROJECTS_ROOT/.code-vm-cred-test"
mkdir -p "$CRED_DIR"
CRED_DEST="/home/$AGENT_USER/.code-vm-test.properties"
cat > "$CRED_DIR/.sandbox-secrets.yaml" << YAML
secrets:
  TEST_USER:
    source: printf sandbox-user
targets:
  - template: gradle-properties
    dest: $CRED_DEST
    secrets:
      - name: TEST_USER
        as: testUser
YAML

(cd "$CRED_DIR" && "$CODE_VM" -- true > /dev/null 2>&1)

if [ "$(adm stat -c '%U:%G %a' "$CRED_DEST" 2> /dev/null)" = "root:$AGENT_USER 444" ]; then
    pass "rendered credential is root-owned and read-only"
else
    fail "rendered credential is root-owned and read-only (got $(adm stat -c '%U:%G %a' "$CRED_DEST" 2> /dev/null))"
fi

if adm grep -q 'testUser=sandbox-user' "$CRED_DEST"; then
    pass "credential rendered through the gradle-properties template"
else
    fail "credential rendered through the gradle-properties template"
fi

assert_fails "agent cannot overwrite the rendered credential" \
    agent bash -c "echo x > $CRED_DEST"

# shellcheck disable=SC2016 # jq program, not a shell expansion
if adm jq -e --arg d "$CRED_DEST" '.permissions.deny | index("Read(" + $d + ")")' \
    "/home/$AGENT_USER/.claude/settings.json" > /dev/null; then
    pass "credential deny rule merged into settings.json"
else
    fail "credential deny rule merged into settings.json"
fi

assert_fails "secret payload is wiped from the guest" \
    adm test -f /run/sandbox-secrets/payload.json

adm rm -f "$CRED_DEST"
rm -rf "$CRED_DIR"

echo ""
echo "── Resource limits ───────────────────────────────────────────────"

if adm systemctl show "user-$(id -u).slice" -p TasksMax | grep -q "TasksMax=2048"; then
    pass "TasksMax is applied to the agent slice"
else
    fail "TasksMax is applied to the agent slice"
fi

echo ""
echo "── Lifecycle commands ────────────────────────────────────────────"

assert_ok "status reports the running instance" \
    bash -c "\"$CODE_VM\" status | grep -q 'code-sandbox (Running)'"
assert_ok "status shows the firewall verification" \
    bash -c "\"$CODE_VM\" status | grep -q 'OUTPUT_POLICY=DROP'"
assert_ok "proxy-log denied mode runs" "$CODE_VM" proxy-log denied
assert_fails "proxy-log rejects an unknown mode" "$CODE_VM" proxy-log everything

# code-vm mount rewrites ~/.config/code-vm/config.yaml, so the temp mount is
# removed from the config again below. The VM keeps the stale mount until its
# next restart, which is harmless.
# Output is captured, not piped into grep -q: an early-exiting grep closes
# the pipe and SIGPIPEs limactl mid-restart, and pipefail would fail the
# pipeline on code-vm's own exit status.
MOUNT_TEST_DIR=$(mktemp -d)
MOUNT_OUT=$("$CODE_VM" mount "$MOUNT_TEST_DIR" 2>&1)
if echo "$MOUNT_OUT" | grep -q "Added"; then
    pass "mount adds a new shared directory"
else
    fail "mount adds a new shared directory"
fi
if (cd "$MOUNT_TEST_DIR" && "$CODE_VM" -- pwd | grep -qx "$MOUNT_TEST_DIR"); then
    pass "newly mounted directory is usable as a workspace"
else
    fail "newly mounted directory is usable as a workspace"
fi
MOUNT_OUT=$("$CODE_VM" mount "$MOUNT_TEST_DIR" 2>&1)
if echo "$MOUNT_OUT" | grep -q "already shared"; then
    pass "mount is idempotent"
else
    fail "mount is idempotent"
fi
yq -i 'del(.extraMounts[] | select(. == "'"$MOUNT_TEST_DIR"'"))' "$HOME/.config/code-vm/config.yaml"
rmdir "$MOUNT_TEST_DIR"

echo ""
echo "── Host exposure ─────────────────────────────────────────────────"

# The portForwards ignore rule must cover wildcard binds: without guestIP
# 0.0.0.0 + proto any, Lima forwarded the guest's Squid port to the host.
if ss -tln 2> /dev/null | grep -q ':3128 '; then
    fail "guest Squid port is not forwarded to the host"
else
    pass "guest Squid port is not forwarded to the host"
fi

echo ""
echo "================================================================"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "================================================================"
[ "$FAIL" -eq 0 ]
