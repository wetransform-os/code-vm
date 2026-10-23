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
#
# A missing component is created with a plain mkdir, never `install -d` or
# `mkdir -p`: those treat an entry that appeared meanwhile as success and, if
# it is a symlink to a directory, go on to apply mode and ownership to the
# directory it points at. mkdir(2) fails on any existing entry without
# touching what it names, so an entry the agent races in only aborts the
# run. Ownership and mode are applied to `.` once `enter` has proven the
# working directory is the real one — a path can be swapped, an inode
# cannot.
ensure_dir() {
    local cur="$AGENT_HOME" comp
    enter "$cur"
    IFS=/ read -ra comps <<< "$1"
    for comp in "${comps[@]}"; do
        [ "$comp" = "." ] || [ -z "$comp" ] && continue
        if [ -L "$comp" ]; then
            die "$cur/$comp is a symlink; refusing to write through it"
        fi
        created=0
        if [ ! -d "$comp" ]; then
            mkdir "$comp" || die "cannot create $cur/$comp"
            created=1
        fi
        cur="$cur/$comp"
        enter "$cur"
        if [ "$created" = 1 ]; then
            chown "$AGENT_UID:$AGENT_GID" .
            chmod 0755 .
        fi
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
# snapshot taken into the root-only staging directory, where the agent cannot
# race the read. The snapshot is the one place this script opens an entry
# the agent controls, so the open itself must be harmless whatever the entry
# turned into since the last look: nofollow refuses a symlink, nonblock makes
# a FIFO (which would otherwise hang the boot until something writes to it)
# return at once, and the byte cap bounds what a writer on the other end can
# push into staging. Anything that is not readable JSON simply yields no
# previous plugins — the agent can only ever lose its own enablement.
PREV_ENABLED_PLUGINS='{}'
ensure_dir "$(dirname "$SETTINGS_REL")"
dd if="./$(basename "$SETTINGS_REL")" of="$STAGE/previous" bs=1M count=4 \
    iflag=nonblock,nofollow status=none 2> /dev/null || true
if [ -s "$STAGE/previous" ]; then
    PREV_ENABLED_PLUGINS="$(jq -c '.enabledPlugins // {}' "$STAGE/previous" 2> /dev/null || echo '{}')"
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
