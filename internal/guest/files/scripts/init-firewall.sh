#!/bin/bash
###############################################################################
# init-firewall.sh — Squid allowlist + iptables default-deny egress firewall
#
#   agent → http_proxy=localhost:3128 → Squid (domain ACL) → internet
#   iptables: default-deny OUTPUT; only root and Squid exit directly
#
# Runs as root from sandbox-boot.sh, last in the sequence: closing egress
# before the CLI updates would break them.
#
# The firewall mode (allowlist|audit|open) is read from a tmpfs file, so a VM
# restart always reverts to allowlist. See set-firewall-mode.sh.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

SQUID_CONF=/etc/squid/squid.conf
FRAGMENT_DIR=/run/sandbox/squid-allow.d
VERIFY_FILE=/run/firewall-verify
MODE_FILE=/run/sandbox/firewall-mode

echo "[firewall] Initializing egress firewall..."

if ! iptables -L OUTPUT -n > /dev/null 2>&1; then
    echo "[firewall] ERROR: iptables is not functional; the VM would have NO egress restrictions."
    exit 1
fi

# The mode file lives in /run, which is tmpfs, so a reboot always reverts to
# the allowlist. There is deliberately no config key for this: a loosened
# firewall must not be able to become the durable default.
install -d -m 0755 /run/sandbox
MODE=allowlist
if [ -r "$MODE_FILE" ]; then
    MODE=$(tr -d '[:space:]' < "$MODE_FILE")
fi
case "$MODE" in
    allowlist | audit | open) ;;
    *)
        echo "[firewall] WARNING: unknown mode '$MODE'; falling back to allowlist"
        MODE=allowlist
        ;;
esac
echo "[firewall] Mode: $MODE"

# ── Domain allowlist ────────────────────────────────────────────────────────
# Container registries are unconditional: this sandbox always has Docker.
DEFAULT_DOMAINS=(
    .anthropic.com .claude.ai .platform.claude.com .code.claude.com .docs.claude.com
    .opencode.ai .models.dev .opncd.ai
    .github.com .githubusercontent.com .githubassets.com
    .pypi.org .pythonhosted.org
    .npmjs.org .npmjs.com .nodejs.org
    .crates.io .rust-lang.org
    proxy.golang.org sum.golang.org pkg.go.dev
    .google.com .bing.com .duckduckgo.com .wikipedia.org
    .stackoverflow.com .readthedocs.io .docs.rs .developer.mozilla.org
    .cloudflare.com .fastly.net
    .json-schema.org .schemastore.org
    .mise.jdx.dev .mise-versions.jdx.dev .mise-java.jdx.dev .mise.run .fnox.jdx.dev
    .dl.k8s.io .releases.hashicorp.com .get.helm.sh
    .opentofu.org .registry.opentofu.org
    .services.gradle.org .plugins.gradle.org .plugins-artifacts.gradle.org
    .repo1.maven.org .repo.maven.apache.org
    .dl-cdn.alpinelinux.org .awscli.amazonaws.com
    # Container registries
    .docker.io .docker.com .hub.docker.com
    .production.cloudflare.docker.com .r2.cloudflarestorage.com
    .ghcr.io .gcr.io .quay.io .registry.k8s.io
)

DOMAIN_LIST=("${DEFAULT_DOMAINS[@]}")
if [ -n "${EXTRA_ALLOWED_DOMAINS:-}" ]; then
    read -ra EXTRA <<< "$EXTRA_ALLOWED_DOMAINS"
    [ ${#EXTRA[@]} -gt 0 ] && DOMAIN_LIST+=("${EXTRA[@]}")
fi

# ── Fragment directory ──────────────────────────────────────────────────────
# Per-workspace allowlists live here, written by code-vm session setup. It is
# tmpfs-backed, so stale entries from projects no longer in use cannot widen
# the allowlist beyond one VM lifetime. 00-base.conf is always present so the
# wildcard include never matches an empty set.
# /run is already a tmpfs on a systemd guest, so fragments and the firewall
# mode file disappear on reboot without an explicit mount. Do NOT mount a tmpfs
# here: it would shadow anything written to /run/sandbox earlier in the boot,
# including the mode file this script reads.
install -d -m 0755 "$FRAGMENT_DIR"
printf '# base fragment; per-workspace fragments are added by code-vm\n' > "$FRAGMENT_DIR/00-base.conf"
chmod 0444 "$FRAGMENT_DIR/00-base.conf"

# ── squid.conf ──────────────────────────────────────────────────────────────
# Order matters: Squid reads linearly, so every `acl allowed_domains` line —
# including the fragments — must precede the http_access rules.
{
    echo "http_port 3128"
    echo ""
    echo "# Security proxy, not a caching proxy"
    echo "cache_dir null /tmp"
    echo "cache deny all"
    echo ""
    echo "access_log /var/log/squid/access.log squid"
    echo ""
    if [ "$MODE" = allowlist ]; then
        echo "# Domain allowlist — .domain matches the domain and all subdomains"
        for domain in "${DOMAIN_LIST[@]}"; do
            echo "acl allowed_domains dstdomain $domain"
        done
        echo ""
        echo "# Per-workspace fragments (tmpfs; cleared on every boot)"
        echo "include $FRAGMENT_DIR/*.conf"
        echo ""
        echo "acl CONNECT method CONNECT"
        echo "http_access allow CONNECT allowed_domains"
        echo "http_access allow allowed_domains"
        echo "http_access deny all"
    else
        echo "# mode=$MODE: domain filtering disabled; the proxy remains an audit log"
        echo "http_access allow all"
    fi
} > "$SQUID_CONF"

echo "[firewall] Generated $SQUID_CONF (${#DOMAIN_LIST[@]} base entries)"

# ── Start Squid ─────────────────────────────────────────────────────────────
# World-readable log dir so proxy-log works without granting write access.
chmod o+rx /var/log/squid/
install -m 0644 -o proxy -g proxy /dev/null /var/log/squid/access.log
systemctl enable squid.service > /dev/null 2>&1 || true
systemctl restart squid.service

READY=false
for _ in $(seq 1 20); do
    if (echo > /dev/tcp/localhost/3128) 2> /dev/null; then
        READY=true
        break
    fi
    sleep 0.5
done
if [ "$READY" != "true" ]; then
    echo "[firewall] ERROR: Squid did not start within 10 seconds."
    exit 1
fi
echo "[firewall] Squid ready on :3128"

# ── iptables ────────────────────────────────────────────────────────────────
GUEST_IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)
GATEWAY=$(ip route show default | awk '{print $3; exit}')

iptables -F OUTPUT
iptables -F INPUT
iptables -F FORWARD
iptables -P INPUT ACCEPT
iptables -P OUTPUT DROP
iptables -P FORWARD DROP

iptables -A OUTPUT -o lo -j ACCEPT
iptables -A INPUT -i lo -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

# DNS first: Lima's host resolver may live on the gateway, which the agent
# rule below rejects. First match wins, so these must be appended earlier.
# /etc/resolv.conf only names the systemd-resolved stub (127.0.0.53); the
# upstream servers resolved queries actually go to live in the resolved copy,
# and without them Squid cannot resolve anything.
DNS_SERVERS=$(cat /etc/resolv.conf /run/systemd/resolve/resolv.conf 2> /dev/null |
    grep -oP '^\s*nameserver\s+\K\S+' | sort -u || true)
for dns in $DNS_SERVERS; do
    iptables -A OUTPUT -d "$dns" -p udp --dport 53 -j ACCEPT
    iptables -A OUTPUT -d "$dns" -p tcp --dport 53 -j ACCEPT
    echo "[firewall]   Allowed DNS: $dns"
done

# Lima's hostResolver DNATs DNS traffic (LIMADNS nat chain) to the host
# gateway on dynamic per-boot ports, so by the time packets reach this filter
# chain they no longer look like dst <resolver>:53. Allow the exact post-DNAT
# destinations, or every name lookup dies in the UDP drop below.
while read -r proto dnat_ip dnat_port; do
    [ -n "$dnat_port" ] || continue
    iptables -A OUTPUT -d "$dnat_ip" -p "$proto" --dport "$dnat_port" -j ACCEPT
    echo "[firewall]   Allowed DNS (Lima DNAT): $proto $dnat_ip:$dnat_port"
done < <(iptables -t nat -S LIMADNS 2> /dev/null |
    sed -n 's/.*-p \([a-z]*\).*--to-destination \([0-9.]*\):\([0-9]*\).*/\1 \2 \3/p')

# Block DNS tunneling to any other resolver.
iptables -A OUTPUT -p udp -j DROP

# Rootless Docker NATs container traffic out as the agent UID, so containers
# reach Squid at the guest's own address. This is the only non-loopback proxy
# path the agent needs.
iptables -A OUTPUT -m owner --uid-owner "$AGENT_UID" -d "$GUEST_IP" -p tcp --dport 3128 -j ACCEPT

# The agent has no business reaching host services: Squid runs in the guest.
if [ -n "$GATEWAY" ]; then
    iptables -A OUTPUT -m owner --uid-owner "$AGENT_UID" -d "$GATEWAY" -j REJECT
    echo "[firewall]   Rejected: agent -> host gateway $GATEWAY"
fi

# mode=open: let the agent reach the internet directly, for tooling that
# ignores http_proxy. Placed after the UDP drop and the gateway REJECT, so
# neither DNS tunneling nor host access is opened up.
if [ "$MODE" = open ]; then
    iptables -A OUTPUT -m owner --uid-owner "$AGENT_UID" -j ACCEPT
    echo "[firewall]   mode=open: agent egress is UNFILTERED and UNLOGGED"
fi

# Anthropic API CIDR — direct, and a fallback if Squid is unavailable.
iptables -A OUTPUT -d 160.79.104.0/23 -p tcp --dport 443 -j ACCEPT

# Root (boot sequence, provisioning) and Squid's own workers exit directly.
iptables -A OUTPUT -m owner --uid-owner 0 -j ACCEPT
iptables -A OUTPUT -m owner --uid-owner proxy -j ACCEPT

iptables -A OUTPUT -m limit --limit 5/min -j LOG --log-prefix "[FIREWALL-BLOCKED] " --log-level 4
iptables -A OUTPUT -j REJECT --reject-with icmp-port-unreachable

# ── Self-verify ─────────────────────────────────────────────────────────────
# The Lima readiness probe waits for this file, so `limactl start` cannot
# return before the firewall is up.
VERIFY_OK=true
OUTPUT_POLICY=$(iptables -L OUTPUT -n | head -1 | grep -o "DROP" || echo "NOT_DROP")
[ "$OUTPUT_POLICY" = "DROP" ] || VERIFY_OK=false

udp_drop=no
iptables -L OUTPUT -n | grep -qE "DROP[[:space:]]+17|DROP.*udp" && udp_drop=yes
[ "$udp_drop" = yes ] || VERIFY_OK=false

PROXY_UID=$(id -u proxy 2> /dev/null || echo 13)
proxy_rule=no
iptables -L OUTPUT -n -v | grep -q "owner UID match $PROXY_UID" && proxy_rule=yes
[ "$proxy_rule" = yes ] || VERIFY_OK=false

gw_reject=no
if [ -n "$GATEWAY" ]; then
    iptables -L OUTPUT -n -v | grep -q "owner UID match $AGENT_UID" && gw_reject=yes
    [ "$gw_reject" = yes ] || VERIFY_OK=false
fi

squid_running=no
(echo > /dev/tcp/localhost/3128) 2> /dev/null && squid_running=yes

{
    echo "OUTPUT_POLICY=$OUTPUT_POLICY"
    echo "UDP_DROP=$udp_drop"
    echo "PROXY_UID_RULE=$proxy_rule"
    echo "AGENT_GATEWAY_REJECT=$gw_reject"
    echo "SQUID_RUNNING=$squid_running"
    echo "FRAGMENT_DIR=$FRAGMENT_DIR"
    echo "FIREWALL_MODE=$MODE"
} > "$VERIFY_FILE"
chmod 0444 "$VERIFY_FILE"

if [ "$VERIFY_OK" != true ]; then
    echo "[firewall] ERROR: verification failed; rules are incorrect."
    exit 1
fi
echo "[firewall] Active. DEFAULT DENY + Squid allowlist on :3128 (mode=$MODE)"
