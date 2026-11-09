#!/bin/bash
###############################################################################
# trim-inode-cache.sh — keep the guest's inode cache below virtiofsd's fd limit
#
# virtiofsd on the host holds one O_PATH file descriptor for every inode the
# guest has looked up on a shared directory and still has cached, and the
# guest kernel keeps inodes cached for as long as memory is not scarce. Tree
# walks over large workspaces (node_modules across a handful of worktrees)
# therefore pile up host fds that are never released. At virtiofsd's limit
# (1,048,576 on a stock Linux host) every new lookup fails, which the guest
# sees as "No such file or directory" for files that plainly exist, and the
# shared workspace is unusable until the VM restarts.
#
# Dropping the dentry and inode caches (drop_caches bit 2) makes the guest
# send FORGET for every unpinned inode, and virtiofsd closes the matching
# fds. Only the cache goes: page cache, open files and dirty data stay, so no
# sync is needed first (a dirty inode is simply not dropped). Writing 4 does
# not drop anything but silences the kernel's own "drop_caches:" log line for
# the rest of the boot (it logs that one write, then no more); the journal
# gets ours instead. The two cannot be combined: the kernel rejects anything
# above 4 with EINVAL.
# Run by sandbox-inode-cache.timer every few minutes, this trims only when
# the count of inodes (all filesystems, /proc/sys/fs/inode-nr) is above the
# threshold, so a quiet guest is never touched.
#
# The first field of inode-nr is the right gauge even though it includes
# in-use inodes: an inode held only by a cached dentry counts as in use, and
# those were 90% of the 1.8M in the incident, while the "unused" field sat
# at 178k. Inodes pinned by open files, cwds and mmaps are the only kind a
# trim cannot release; if they alone exceed the threshold, every run would
# drop the cache for nothing, so that case is reported rather than repeated
# silently.
#
# The threshold keeps the host far below its limit while leaving room for a
# normal working set: a stock guest idles at well under 100k inodes.
###############################################################################
set -euo pipefail

THRESHOLD=${INODE_TRIM_THRESHOLD:-250000}

read -r CACHED _ < /proc/sys/fs/inode-nr
if [ "$CACHED" -le "$THRESHOLD" ]; then
    exit 0
fi

echo 4 > /proc/sys/vm/drop_caches
echo 2 > /proc/sys/vm/drop_caches
read -r AFTER _ < /proc/sys/fs/inode-nr
echo "[inode-cache] trimmed: $CACHED cached inodes -> $AFTER (threshold $THRESHOLD)"
if [ "$AFTER" -gt "$THRESHOLD" ]; then
    echo "[inode-cache] WARNING: $AFTER inodes are pinned by open files and cannot be trimmed; raise INODE_TRIM_THRESHOLD or find what holds them"
fi
