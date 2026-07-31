#!/bin/bash
###############################################################################
# lock-settings.sh — restore and lock the canonical Claude config
#
# Runs as root from sandbox-boot.sh, before the agent can start. Copies the
# canonical tree delivered by code-vm into the agent's home and makes every
# file root-owned and read-only, so the agent cannot rewrite its own
# permission rules.
#
# /usr/local/share/sandbox-config mirrors the agent's home:
#   .claude/settings.json -> /home/devuser/.claude/settings.json
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

CONFIG_SRC=/usr/local/share/sandbox-config
CONFIG_DST="/home/${AGENT_USER}"
CLAUDE_DIR="$CONFIG_DST/.claude"
SETTINGS="$CLAUDE_DIR/settings.json"
SETTINGS_LOCAL="$CLAUDE_DIR/settings.local.json"

if [ ! -d "$CONFIG_SRC" ]; then
    echo "[lock-settings] ERROR: canonical config missing at $CONFIG_SRC"
    exit 1
fi

apply_tree() {
    local src="$1" src_file rel dst
    while IFS= read -r src_file; do
        rel="${src_file#"$src"/}"
        dst="$CONFIG_DST/$rel"
        install -d "$(dirname "$dst")"
        cp "$src_file" "$dst"
        chown "root:${AGENT_USER}" "$dst"
        chmod 0444 "$dst"
        echo "[lock-settings]   Locked: $rel"
    done < <(find "$src" -type f)
}

# Claude Code records plugin enablement in settings.json under enabledPlugins.
# The copy below would silently disable every plugin the user installed, even
# though the plugin files persist on the guest disk. Capture and re-merge it.
PREV_ENABLED_PLUGINS='{}'
if [ -f "$SETTINGS" ]; then
    PREV_ENABLED_PLUGINS="$(jq -c '.enabledPlugins // {}' "$SETTINGS" 2> /dev/null || echo '{}')"
fi

install -d -o "$AGENT_USER" -g "$AGENT_USER" "$CLAUDE_DIR"
apply_tree "$CONFIG_SRC"

merge_into_settings() {
    # $1: jq program, remaining args passed to jq
    local prog="$1"
    shift
    chmod 0644 "$SETTINGS"
    jq "$@" "$prog" "$SETTINGS" > "${SETTINGS}.tmp"
    mv "${SETTINGS}.tmp" "$SETTINGS"
    chown "root:${AGENT_USER}" "$SETTINGS"
    chmod 0444 "$SETTINGS"
}

if [ "$PREV_ENABLED_PLUGINS" != "{}" ] && [ "$PREV_ENABLED_PLUGINS" != "null" ]; then
    # shellcheck disable=SC2016 # jq program, not a shell expansion
    merge_into_settings '.enabledPlugins = ($ep + (.enabledPlugins // {}))' \
        --argjson ep "$PREV_ENABLED_PLUGINS"
    echo "[lock-settings] Preserved enabledPlugins across restart"
fi

# Claim settings.local.json: Claude Code treats it as an override file, so an
# unclaimed path is a permission-bypass vector.
echo '{}' > "$SETTINGS_LOCAL"
chown "root:${AGENT_USER}" "$SETTINGS_LOCAL"
chmod 0444 "$SETTINGS_LOCAL"

echo "[lock-settings] Config restored from canonical and locked"
