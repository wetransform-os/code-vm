#!/bin/bash
###############################################################################
# lock-settings.sh — restore and lock the canonical Claude config
#
# Runs as root: from sandbox-boot.sh before the agent can start, and from
# `code-vm config apply` while the agent is live. Copies the canonical tree
# delivered by code-vm into the agent's home and makes every file root-owned
# and read-only, so the agent cannot rewrite its own permission rules.
#
# /usr/local/share/sandbox-config mirrors the agent's home:
#   .claude/settings.json -> /home/devuser/.claude/settings.json
#
# The home is agent-owned, and a directory owner can replace any entry in it —
# a root-owned read-only file included — with a symlink. Whatever the agent
# left on disk is therefore hostile input even at boot: a symlink planted in
# one session is still there on the next boot. Nothing here opens a path in
# the home for writing. Files are built in a root-only staging directory and
# renamed into place; rename(2) replaces a symlink instead of following it.
# Directories are entered with cd and verified with `pwd -P`, so a symlink
# anywhere on the way is detected, and once entered the working directory is
# an inode that a later rename of the path cannot redirect.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

CONFIG_SRC=/usr/local/share/sandbox-config
SETTINGS_REL=.claude/settings.json
SETTINGS_LOCAL_REL=.claude/settings.local.json

die() {
    echo "[lock-settings] ERROR: $*" >&2
    exit 1
}

[ -d "$CONFIG_SRC" ] || die "canonical config missing at $CONFIG_SRC"

# /home is root-owned, so the agent cannot swap its own home directory; its
# physical path is a trustworthy anchor for every check below.
AGENT_HOME=$(realpath -e "/home/${AGENT_USER}") || die "agent home missing"
[ -d "$AGENT_HOME" ] || die "agent home $AGENT_HOME is not a directory"

# Root-only staging on the same filesystem as the home: `mv -T` must be a
# rename, not a cross-device copy that opens the destination path.
STAGE=$(mktemp -d /home/.lock-settings.XXXXXX)
trap 'rm -rf "$STAGE"' EXIT
[ "$(stat -c %d "$STAGE")" = "$(stat -c %d "$AGENT_HOME")" ] ||
    die "staging and the agent home are on different filesystems"

# enter DIR: cd and prove the working directory really is DIR, whatever the
# path looked like a moment ago.
enter() {
    cd "$1" 2> /dev/null || die "cannot enter $1"
    [ "$(pwd -P)" = "$1" ] || die "$1 resolves to $(pwd -P); refusing to write through it"
}

# ensure_dir REL: make every component of REL (relative to the home) a real
# agent-owned directory, and leave the shell inside it.
ensure_dir() {
    local cur="$AGENT_HOME" comp
    enter "$cur"
    IFS=/ read -ra comps <<< "$1"
    for comp in "${comps[@]}"; do
        [ "$comp" = "." ] || [ -z "$comp" ] && continue
        if [ -L "$comp" ]; then
            die "$cur/$comp is a symlink; refusing to write through it"
        fi
        if [ ! -d "$comp" ]; then
            install -d -m 0755 -o "$AGENT_UID" -g "$AGENT_GID" "$comp"
        fi
        cur="$cur/$comp"
        enter "$cur"
    done
}

# place STAGED REL: move a fully prepared staged file to REL in the home,
# root-owned and read-only. The destination entry is replaced atomically;
# an entry that became a directory makes the rename fail rather than descend.
place() {
    local staged="$1" rel="$2"
    chown "root:${AGENT_GID}" "$staged"
    chmod 0444 "$staged"
    ensure_dir "$(dirname "$rel")"
    mv -T "$staged" "./$(basename "$rel")"
    echo "[lock-settings]   Locked: $rel"
}

# Claude Code records plugin enablement in settings.json under enabledPlugins.
# Replacing the file would silently disable every plugin the user installed,
# even though the plugin files persist on the guest disk. Capture it from a
# staged copy taken without following symlinks: the copy is inspected in the
# root-only staging directory, so the agent cannot race the read.
PREV_ENABLED_PLUGINS='{}'
ensure_dir "$(dirname "$SETTINGS_REL")"
if [ -e "$(basename "$SETTINGS_REL")" ] || [ -L "$(basename "$SETTINGS_REL")" ]; then
    cp -P "./$(basename "$SETTINGS_REL")" "$STAGE/previous" 2> /dev/null || true
    if [ -f "$STAGE/previous" ] && [ ! -L "$STAGE/previous" ]; then
        PREV_ENABLED_PLUGINS="$(jq -c '.enabledPlugins // {}' "$STAGE/previous" 2> /dev/null || echo '{}')"
    fi
fi

n=0
while IFS= read -r src_file; do
    rel="${src_file#"$CONFIG_SRC"/}"
    staged="$STAGE/file.$n"
    n=$((n + 1))
    cp "$src_file" "$staged"
    if [ "$rel" = "$SETTINGS_REL" ] && [ "$PREV_ENABLED_PLUGINS" != "{}" ] && [ "$PREV_ENABLED_PLUGINS" != "null" ]; then
        # shellcheck disable=SC2016 # jq program, not a shell expansion
        jq --argjson ep "$PREV_ENABLED_PLUGINS" '.enabledPlugins = ($ep + (.enabledPlugins // {}))' \
            "$staged" > "$staged.merged"
        mv -T "$staged.merged" "$staged"
        echo "[lock-settings] Preserved enabledPlugins across restart"
    fi
    place "$staged" "$rel"
done < <(find "$CONFIG_SRC" -type f | sort)

# Claim settings.local.json: Claude Code treats it as an override file, so an
# unclaimed path is a permission-bypass vector.
echo '{}' > "$STAGE/local"
place "$STAGE/local" "$SETTINGS_LOCAL_REL"

echo "[lock-settings] Config restored from canonical and locked"
