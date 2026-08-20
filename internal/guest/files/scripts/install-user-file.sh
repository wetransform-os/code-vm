#!/bin/bash
###############################################################################
# install-user-file.sh — place one host-staged file into the agent home
#
# Invoked as root by code-vm: install-user-file.sh <staged-src> <rel-dst> <mode>
#
# The staged source sits in limaadmin's 0700 staging dir, unreadable by the
# agent; and a root write into the agent-owned home is the TOCTOU class the
# profile applier already closed. So root only RELAYS: the file moves to a
# root:AGENT_GID 0640 drop on tmpfs, and the final install runs with agent
# privileges — a planted symlink can only redirect a write the agent could
# already make. The drop is removed afterwards; rendered secrets exist there
# only for the moment between relay and install.
###############################################################################
set -euo pipefail

# shellcheck source=/dev/null
. /etc/sandbox/provision.env

src="$1"
rel="$2"
mode="$3"

# Defense in depth: the host already validates rel before it ever reaches
# here, but this relay is root, so a path that escapes the agent home is
# rejected again on this end too.
case "$rel" in
    /* | */../* | */.. | ../* | ..)
        echo "install-user-file.sh: rejected unsafe rel path: $rel" >&2
        exit 1
        ;;
esac

AGENT_HOME="/home/${AGENT_USER}"

DROP_DIR=/run/sandbox/user-files
install -d -m 0750 -o root -g "$AGENT_GID" "$DROP_DIR"
drop=$(mktemp "$DROP_DIR/file-XXXXXXXX")

# Trapped immediately after mktemp creates the placeholder: if the install
# below fails, cleanup still removes it instead of leaving an empty drop on
# tmpfs.
cleanup() { rm -f "$drop"; }
trap cleanup EXIT

install -m 0640 -o root -g "$AGENT_GID" "$src" "$drop"
rm -f "$src"

# Same hardened pattern as the profile applier's agent runner: no login
# shell, system PATH only, BASH_ENV/ENV cleared. Positional args, not string
# interpolation.
# shellcheck disable=SC2016 # the inner bash -c program expands its own args
setpriv --reuid "$AGENT_UID" --regid "$AGENT_GID" --init-groups \
    env -u BASH_ENV -u ENV \
    HOME="$AGENT_HOME" \
    USER="$AGENT_USER" \
    XDG_RUNTIME_DIR="/run/user/${AGENT_UID}" \
    PATH=/usr/local/bin:/usr/bin:/bin \
    bash -c 'dst="$1/$2"; mkdir -p "$(dirname "$dst")" && rm -f "$dst" && install -m "$3" "$4" "$dst"' \
    _ "$AGENT_HOME" "$rel" "$mode" "$drop"
