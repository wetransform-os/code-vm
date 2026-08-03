#!/bin/bash
###############################################################################
# init-firewall.sh — Squid allowlist + iptables default-deny egress firewall
#
#   agent → http_proxy=localhost:3128 → Squid (domain ACL) → internet
#   iptables: default-deny OUTPUT; only root and Squid exit directly
#
# Runs as root from sandbox-boot.sh before anything that touches the network,
# so no step in the boot sequence has unfiltered egress — including the agent
# CLI update, which goes through Squid like everything else.
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
    .opentofu.org
    .services.gradle.org .plugins.gradle.org .plugins-artifacts.gradle.org
    .repo1.maven.org .repo.maven.apache.org
    .dl-cdn.alpinelinux.org .awscli.amazonaws.com
    # Container registries
    .docker.io .docker.com
    .r2.cloudflarestorage.com
    .ghcr.io .gcr.io .quay.io .registry.k8s.io
)

DOMAIN_LIST=("${DEFAULT_DOMAINS[@]}")

# ── Fragment directory ──────────────────────────────────────────────────────
# Extra domains from the host config land here rather than in the baked list
# above, so that `code-vm allow` can rewrite exactly this file and reload Squid
# without a restart — one source of truth in the guest, no duplicate ACLs.
# The directory is tmpfs-backed, so guest state cannot drift from the host
# config: after a reboot it holds only what code-vm pushes back.
# 00-base.conf is always present so the wildcard include never matches an
# empty set.
# /run is already a tmpfs on a systemd guest, so fragments and the firewall
# mode file disappear on reboot without an explicit mount. Do NOT mount a tmpfs
# here: it would shadow anything written to /run/sandbox earlier in the boot,
# including the mode file this script reads.
install -d -m 0755 "$FRAGMENT_DIR"
printf '# base fragment; host-config domains are added by code-vm\n' > "$FRAGMENT_DIR/00-base.conf"
chmod 0444 "$FRAGMENT_DIR/00-base.conf"

# Boot-time application of the host config's extraDomains. code-vm rewrites
# this same path on later invocations; the name must stay in sync with
# session.HostFragmentName.
if [ -n "${EXTRA_ALLOWED_DOMAINS:-}" ]; then
    read -ra EXTRA <<< "$EXTRA_ALLOWED_DOMAINS"
    if [ ${#EXTRA[@]} -gt 0 ]; then
        {
            echo "# code-vm allowlist fragment rendered from the host config"
            for domain in "${EXTRA[@]}"; do
                echo "acl allowed_domains dstdomain $domain"
            done
        } > "$FRAGMENT_DIR/10-host-config.conf"
        chmod 0444 "$FRAGMENT_DIR/10-host-config.conf"
        echo "[firewall]   Host-config domains: ${EXTRA[*]}"
    fi
fi

# ── squid.conf ──────────────────────────────────────────────────────────────
# Order matters: Squid reads linearly, so every `acl allowed_domains` line —
# including the fragments — must precede the http_access rules.
{
    echo "http_port 3128"
    echo ""
    echo "# Security proxy, not a caching proxy. No cache_dir: the null store"
    echo "# is not a supported type on current Squid, and cache deny all is"
    echo "# what actually disables caching."
    echo "cache deny all"
    echo ""
    echo "# Squid's default is to wait 30 seconds for active connections on"
    echo "# shutdown, so every restart — one per firewall mode switch — left the"
    echo "# proxy refusing connections for half a minute. Nothing here needs a"
    echo "# grace period: clients retry."
    echo "shutdown_lifetime 1 second"
    echo ""
    echo "access_log /var/log/squid/access.log squid"
    echo ""
    if [ "$MODE" = allowlist ]; then
        echo "# Domain allowlist — .domain matches the domain and all subdomains"
        for domain in "${DOMAIN_LIST[@]}"; do
            echo "acl allowed_domains dstdomain $domain"
        done
        echo ""
        echo "# Host-config fragments (tmpfs; cleared on every boot)"
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
# Create the log only when absent. This script re-runs on every firewall mode
# switch, and truncating here would destroy the audit trail the user is most
# likely trying to inspect — reaching for `code-vm firewall audit` to find out
# what was denied must not erase what was denied.
if [ ! -f /var/log/squid/access.log ]; then
    install -m 0644 -o proxy -g proxy /dev/null /var/log/squid/access.log
fi
systemctl enable squid.service > /dev/null 2>&1 || true
systemctl restart squid.service

# Three consecutive connects, not one. A single successful connect is also what
# a Squid instance about to be replaced by a queued restart looks like, and the
# boot sequence goes on to install the agent CLIs through this port — which
# failed with ECONNREFUSED when it accepted a socket that then closed.
READY=false
STREAK=0
for _ in $(seq 1 60); do
    if (echo > /dev/tcp/localhost/3128) 2> /dev/null; then
        STREAK=$((STREAK + 1))
        if [ "$STREAK" -ge 3 ]; then
            READY=true
            break
        fi
    else
        STREAK=0
    fi
    sleep 0.5
done
if [ "$READY" != "true" ]; then
    echo "[firewall] ERROR: Squid did not stay up within 30 seconds." >&2
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
# Checks match whole rule specs from `iptables -S`, not substrings of the
# human-readable listing. The loose form was actively misleading: the
# agent-to-Squid ACCEPT rule added above also contains
# "owner UID match <agent uid>", so grepping for that reported the gateway
# REJECT as present even when it was gone.
RULES=$(iptables -S OUTPUT)
PROXY_UID=$(id -u proxy 2> /dev/null || echo 13)

VERIFY_OK=true

# has_rule matches a complete rule line, so no other rule can satisfy a check.
# Each check is spelled out rather than routed through a helper that returns its
# result: a helper called in a command substitution runs in a subshell, where
# setting VERIFY_OK=false would be silently discarded and the verification would
# always pass.
has_rule() { printf '%s\n' "$RULES" | grep -qxF -- "$1"; }

OUTPUT_POLICY=NOT_DROP
if has_rule "-P OUTPUT DROP"; then
    OUTPUT_POLICY=DROP
else
    echo "[firewall] ERROR: the OUTPUT policy is not DROP" >&2
    VERIFY_OK=false
fi

udp_drop=no
if has_rule "-A OUTPUT -p udp -j DROP"; then
    udp_drop=yes
else
    echo "[firewall] ERROR: the DNS-tunneling UDP drop is missing" >&2
    VERIFY_OK=false
fi

proxy_rule=no
if has_rule "-A OUTPUT -m owner --uid-owner $PROXY_UID -j ACCEPT"; then
    proxy_rule=yes
else
    echo "[firewall] ERROR: Squid's own egress rule (uid $PROXY_UID) is missing" >&2
    VERIFY_OK=false
fi

gw_reject=skipped
if [ -n "$GATEWAY" ]; then
    gw_reject=no
    if has_rule "-A OUTPUT -d ${GATEWAY}/32 -m owner --uid-owner ${AGENT_UID} -j REJECT --reject-with icmp-port-unreachable"; then
        gw_reject=yes
    else
        echo "[firewall] ERROR: the agent-to-host-gateway reject ($GATEWAY) is missing" >&2
        VERIFY_OK=false
    fi
fi

squid_running=no
if (echo > /dev/tcp/localhost/3128) 2> /dev/null; then
    squid_running=yes
else
    echo "[firewall] ERROR: Squid is not listening on 3128" >&2
    VERIFY_OK=false
fi

if [ "$VERIFY_OK" != true ]; then
    # Deliberately leave $VERIFY_FILE absent. The Lima readiness probe treats
    # its existence as "the firewall is up", so writing it before this gate —
    # as an earlier version did — let `limactl start` succeed and hand the
    # agent a VM whose firewall had failed its own checks. In the container
    # sandbox this same exit aborted entrypoint.sh and the container died; the
    # equivalent here is to withhold the readiness signal.
    echo "[firewall] ERROR: verification failed; refusing to report the firewall as ready." >&2
    exit 1
fi

# Written only once every check above has passed, so its presence is a real
# success signal rather than a record of whatever was found.
{
    echo "VERIFY=ok"
    echo "OUTPUT_POLICY=$OUTPUT_POLICY"
    echo "UDP_DROP=$udp_drop"
    echo "PROXY_UID_RULE=$proxy_rule"
    echo "AGENT_GATEWAY_REJECT=$gw_reject"
    echo "SQUID_RUNNING=$squid_running"
    echo "FRAGMENT_DIR=$FRAGMENT_DIR"
    echo "FIREWALL_MODE=$MODE"
} > "$VERIFY_FILE"
chmod 0444 "$VERIFY_FILE"

echo "[firewall] Active. DEFAULT DENY + Squid allowlist on :3128 (mode=$MODE)"
