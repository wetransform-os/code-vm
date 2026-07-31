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
echo "── Allowlist is host-controlled ──────────────────────────────────"
# A workspace file must never widen egress. This is the regression guard for
# dropping .sandbox-domains, whose whole problem was that the agent could
# author it.

DOMAIN_TEST_DIR="$PROJECTS_ROOT/.code-vm-domains-test"
mkdir -p "$DOMAIN_TEST_DIR"
echo ".example.org" > "$DOMAIN_TEST_DIR/.sandbox-domains"

if (cd "$DOMAIN_TEST_DIR" && "$CODE_VM" -- curl -fsS -o /dev/null --max-time 20 https://example.org) 2> /dev/null; then
    fail "a workspace .sandbox-domains file must NOT widen the allowlist"
else
    pass "a workspace .sandbox-domains file does not widen the allowlist"
fi
rm -rf "$DOMAIN_TEST_DIR"

# The agent must not be able to reach the staging area or the fragment dir.
assert_fails "agent cannot write the allowlist fragment directory" \
    agent bash -c 'echo "acl allowed_domains dstdomain .attacker.example" > /run/sandbox/squid-allow.d/99-evil.conf'
assert_fails "agent cannot read the admin staging directory" \
    agent ls /home/limaadmin/.code-vm-staging

echo ""
echo "── code-vm allow ─────────────────────────────────────────────────"
# Runs after the egress assertions above: it widens the allowlist for the rest
# of the run, and the restart-hygiene section re-checks the tmpfs behaviour.

CONFIG_FILE="$HOME/.config/code-vm/config.yaml"
cp "$CONFIG_FILE" "$CONFIG_FILE.suite-backup"

if "$CODE_VM" allow --yes example.org 2>&1 | grep -q 'active now'; then
    pass "allow reports the domain as applied live"
else
    fail "allow reports the domain as applied live"
fi

if yq -e '.extraDomains | contains([".example.org"])' "$CONFIG_FILE" > /dev/null 2>&1; then
    pass "allow records the domain in the host config"
else
    fail "allow records the domain in the host config (got: $(yq -o=json -I=0 '.extraDomains' "$CONFIG_FILE"))"
fi

assert_ok "the newly allowed domain is reachable without a VM restart" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

if [ "$(adm stat -c '%U:%G %a' /run/sandbox/squid-allow.d/10-host-config.conf)" = "root:root 444" ]; then
    pass "host-config fragment is root-owned and read-only"
else
    fail "host-config fragment is root-owned and read-only (got $(adm stat -c '%U:%G %a' /run/sandbox/squid-allow.d/10-host-config.conf 2> /dev/null))"
fi

if "$CODE_VM" allow --yes example.org 2>&1 | grep -q 'already allowed'; then
    pass "allow is idempotent"
else
    fail "allow is idempotent"
fi

if "$CODE_VM" allow --yes 'sub.example.org' 2>&1 | grep -q 'already allowed'; then
    pass "allow recognises a domain already covered by a parent entry"
else
    fail "allow recognises a domain already covered by a parent entry"
fi

# Captured, not piped: code-vm exits non-zero here by design, and under
# pipefail that would fail the pipeline even though grep matched.
MALFORMED_OUT=$("$CODE_VM" allow --yes 'not a domain' 2>&1)
if echo "$MALFORMED_OUT" | grep -q 'cannot read a domain'; then
    pass "allow rejects malformed input"
else
    fail "allow rejects malformed input (got: $MALFORMED_OUT)"
fi

# Removing the domain from the config must take effect, or a revoked domain
# would stay allowed for the VM's lifetime.
cp "$CONFIG_FILE.suite-backup" "$CONFIG_FILE"
"$CODE_VM" -- true > /dev/null 2>&1
assert_fails "removing the domain from the host config revokes it" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org
rm -f "$CONFIG_FILE.suite-backup"

# A mount that exposes the config would let the agent edit its own allowlist.
if "$CODE_VM" --config "$CONFIG_FILE" status > /dev/null 2>&1; then
    pass "status works with the config outside every mount"
else
    fail "status works with the config outside every mount"
fi

echo ""
echo "── Session setup ─────────────────────────────────────────────────"

HOST_GIT_EMAIL=$(git config --get user.email || true)
if [ -n "$HOST_GIT_EMAIL" ]; then
    if agent git config --get user.email | grep -qx "$HOST_GIT_EMAIL"; then
        pass "host git identity is seeded into the guest"
    else
        fail "host git identity is seeded into the guest"
    fi
fi

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
echo "── Workspace file ownership ──────────────────────────────────────"

OWN_DIR="$PROJECTS_ROOT/.code-vm-own-test"
mkdir -p "$OWN_DIR"
echo host > "$OWN_DIR/from-host"

if [ "$(adm stat -c '%u' "$OWN_DIR/from-host")" = "$(id -u)" ]; then
    pass "host-created file is owned by the agent UID in the guest"
else
    fail "host-created file is owned by the agent UID in the guest"
fi

(cd "$OWN_DIR" && agent bash -c 'echo guest > from-guest')
if [ -f "$OWN_DIR/from-guest" ] && [ "$(stat -c '%u' "$OWN_DIR/from-guest")" = "$(id -u)" ]; then
    pass "guest-created file is owned by the host user on the host"
else
    fail "guest-created file is owned by the host user on the host"
fi

rm -rf "$OWN_DIR"

echo ""
echo "── Testcontainers primitives ─────────────────────────────────────"

AGENT_SOCK="/run/user/$(id -u)/docker.sock"

assert_ok "Docker API responds over DOCKER_HOST" \
    agent docker version --format '{{.Server.Version}}'

assert_ok "the daemon socket can be bind-mounted into a container" \
    agent docker run --rm -v "$AGENT_SOCK:/var/run/docker.sock" docker:cli docker version

if agent docker run -d --rm --name code-vm-ryuk \
    -v "$AGENT_SOCK:/var/run/docker.sock" \
    -e RYUK_PORT=8080 testcontainers/ryuk:0.14.0 > /dev/null 2>&1; then
    pass "Ryuk starts (it is incompatible with rootless Podman, which is why this exists)"
    agent docker rm -f code-vm-ryuk > /dev/null 2>&1
else
    fail "Ryuk starts"
fi

agent docker run -d --rm --name code-vm-ports -p 18080:80 nginx:alpine > /dev/null 2>&1
sleep 3
if agent curl -fsS -o /dev/null --max-time 10 --noproxy 127.0.0.1 http://127.0.0.1:18080; then
    pass "published container ports are reachable inside the guest"
else
    fail "published container ports are reachable inside the guest"
fi
agent docker rm -f code-vm-ports > /dev/null 2>&1

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
echo "── Firewall modes ────────────────────────────────────────────────"

if "$CODE_VM" firewall | grep -qx allowlist; then
    pass "default mode is allowlist"
else
    fail "default mode is allowlist (got $("$CODE_VM" firewall))"
fi

assert_fails "mode=open is rejected without confirmation flags" \
    bash -c "echo n | \"$CODE_VM\" firewall open"

"$CODE_VM" firewall audit > /dev/null
assert_ok "audit mode reaches a non-allowlisted domain through the proxy" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "audit mode still forces traffic through the proxy" \
    agent env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
    curl -fsS -o /dev/null --max-time 20 https://example.org

"$CODE_VM" firewall open --yes > /dev/null
assert_ok "open mode allows direct egress without the proxy" \
    agent env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY \
    curl -fsS -o /dev/null --max-time 20 https://example.org
assert_fails "open mode still blocks DNS tunneling" \
    agent timeout 10 nslookup example.org 1.1.1.1
# shellcheck disable=SC2016 # the gateway lookup must expand inside the guest
assert_fails "open mode still blocks the host gateway" \
    agent bash -c 'timeout 5 bash -c "echo > /dev/tcp/$(ip route show default | awk "{print \$3; exit}")/22"'

assert_fails "the agent cannot change the firewall mode" \
    agent /usr/local/lib/sandbox/set-firewall-mode.sh allowlist
assert_fails "the agent cannot write the mode file" \
    agent bash -c 'echo open > /run/sandbox/firewall-mode'

"$CODE_VM" firewall allowlist > /dev/null
assert_fails "returning to allowlist blocks the domain again" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

# A mode switch re-runs init-firewall.sh. It must not truncate the access log:
# reaching for `firewall audit` to find out what was denied would otherwise
# erase the evidence.
LOG_LINES_BEFORE=$(adm sh -c 'wc -l < /var/log/squid/access.log' 2> /dev/null || echo 0)
"$CODE_VM" firewall audit > /dev/null
"$CODE_VM" firewall allowlist > /dev/null
LOG_LINES_AFTER=$(adm sh -c 'wc -l < /var/log/squid/access.log' 2> /dev/null || echo 0)
if [ "$LOG_LINES_BEFORE" -gt 0 ] && [ "$LOG_LINES_AFTER" -ge "$LOG_LINES_BEFORE" ]; then
    pass "a firewall mode switch preserves the proxy audit log"
else
    fail "a firewall mode switch preserves the proxy audit log (before=$LOG_LINES_BEFORE after=$LOG_LINES_AFTER)"
fi

echo ""
echo "── Restart hygiene ───────────────────────────────────────────────"
# Runs last: the mode file and the allowlist fragments live in tmpfs, so a
# restart must revert to allowlist and leave the guest holding only what the
# host config puts back.

"$CODE_VM" firewall audit > /dev/null
"$CODE_VM" stop > /dev/null 2>&1
"$CODE_VM" start > /dev/null 2>&1

if "$CODE_VM" firewall | grep -qx allowlist; then
    pass "firewall mode reverts to allowlist on VM restart"
else
    fail "firewall mode reverts to allowlist on VM restart"
fi

if [ "$(adm sh -c 'ls /run/sandbox/squid-allow.d | tr "\n" " "')" = "00-base.conf " ]; then
    pass "guest holds no allowlist fragment the host config did not put there"
else
    fail "guest holds no allowlist fragment the host config did not put there (got: $(adm sh -c 'ls /run/sandbox/squid-allow.d | tr "\n" " "'))"
fi

assert_fails "a domain absent from the host config is blocked after restart" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

assert_ok "settings stay locked after restart" \
    bash -c "[ \"\$(limactl shell $INSTANCE sudo stat -c '%U:%G %a' /home/$AGENT_USER/.claude/settings.json)\" = 'root:$AGENT_USER 444' ]"

# Host-config domains must survive a restart — that is the difference from the
# tmpfs-only mechanism this replaced. init-firewall.sh writes the fragment at
# boot from provision.env, so the domain is live before any invocation.
cp "$CONFIG_FILE" "$CONFIG_FILE.restart-backup"
"$CODE_VM" allow --yes example.org > /dev/null 2>&1
"$CODE_VM" stop > /dev/null 2>&1
"$CODE_VM" start > /dev/null 2>&1

if adm test -f /run/sandbox/squid-allow.d/10-host-config.conf; then
    pass "host-config fragment is rebuilt at boot"
else
    fail "host-config fragment is rebuilt at boot"
fi

assert_ok "a host-config domain is allowed again after restart" \
    agent curl -fsS -o /dev/null --max-time 20 https://example.org

mv "$CONFIG_FILE.restart-backup" "$CONFIG_FILE"

echo ""
echo "================================================================"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "================================================================"
[ "$FAIL" -eq 0 ]
