#!/bin/bash
###############################################################################
# proxy-log.sh — read the Squid access log inside the sandbox VM
###############################################################################
set -uo pipefail

LOG=/var/log/squid/access.log
mode=${1:-all}

if [ ! -f "$LOG" ]; then
    echo "proxy-log: $LOG does not exist yet" >&2
    exit 1
fi

case "$mode" in
    all) cat "$LOG" ;;
    denied) grep -E 'DENIED' "$LOG" || true ;;
    allowed) grep -vE 'DENIED' "$LOG" || true ;;
    follow) tail -f "$LOG" ;;
    *)
        echo "usage: proxy-log.sh [all|denied|allowed|follow]" >&2
        exit 2
        ;;
esac
