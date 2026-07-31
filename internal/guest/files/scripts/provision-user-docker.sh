#!/bin/bash
###############################################################################
# provision-user-docker.sh — rootless Docker setup, runs as the agent user
#
# Invoked from provision-system.sh via setpriv. Expects HOME, USER,
# XDG_RUNTIME_DIR and CONTAINER_PROXY in the environment.
###############################################################################
set -euo pipefail

log() { echo "[provision-user] $*"; }

systemctl --user start dbus.service > /dev/null 2>&1 || true

if ! systemctl --user is-enabled docker.service > /dev/null 2>&1; then
    log "Running dockerd-rootless-setuptool.sh"
    dockerd-rootless-setuptool.sh install
fi

# dockerd pulls images itself, so the daemon needs the proxy.
install -d "$HOME/.config/systemd/user/docker.service.d"
cat > "$HOME/.config/systemd/user/docker.service.d/http-proxy.conf" << 'EOF'
[Service]
Environment="HTTP_PROXY=http://localhost:3128"
Environment="HTTPS_PROXY=http://localhost:3128"
Environment="NO_PROXY=localhost,127.0.0.1"
EOF

# Container *runtime* proxy env is opt-in. Injecting it by default would also
# apply to compose services, where a bare service name such as "db" matches no
# noProxy entry and would be sent to Squid — breaking service-to-service
# traffic, which is exactly what this sandbox exists to fix.
install -d "$HOME/.config/docker"
if [ "${CONTAINER_PROXY:-false}" = "true" ]; then
    GUEST_IP=$(ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1)
    cat > "$HOME/.config/docker/config.json" << EOF
{
  "proxies": {
    "default": {
      "httpProxy": "http://${GUEST_IP}:3128",
      "httpsProxy": "http://${GUEST_IP}:3128",
      "noProxy": "localhost,127.0.0.1,172.16.0.0/12,10.0.0.0/8,192.168.0.0/16"
    }
  }
}
EOF
    log "Container proxy env enabled (containerProxy=true)"
else
    printf '{}\n' > "$HOME/.config/docker/config.json"
fi

systemctl --user daemon-reload
systemctl --user enable --now docker.service
docker context use rootless > /dev/null
log "Rootless Docker ready"
