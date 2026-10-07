#!/usr/bin/env bash
# build-rootfs.sh -- build the shared, read-only "toolset" rootfs (vda) used
# by every S2 action microVM: static guest-init as /init, busybox as /bin/sh.
# Unprivileged: built entirely with `mkfs.ext4 -d <dir>` (no loop mount), same
# trick the design's S1 prototype and the S0 probe both rely on.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
out=${1:-/data/tmp/microvm-s2/bin/toolset-rootfs.ext4}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cc -static -O2 -Wall -o "$work/init" "$here/guest-init.c"

root="$work/root"
mkdir -p "$root"/{bin,dev,mnt,proc,sys,lib,lib64,lib/x86_64-linux-gnu}
cp "$work/init" "$root/init"
cp /usr/bin/busybox "$root/bin/busybox"
# A handful of applets as symlinks, enough for the rig's demo actions
# (compile-ish / echo-ish shell one-liners). Real actions on the farm bring
# their own toolchain in the action's input tree; this rootfs only needs a
# shell, not a full userland (4.1/4.3 of the design).
for applet in sh cat echo true false sleep printf uname mkdir ls env; do
	ln -s busybox "$root/bin/$applet"
done
# Bazel's genrule-setup.sh (and most genrule `cmd`s) hardcode `/bin/bash -c
# ...`. busybox has no "bash" applet name to alias, so bring the host's real
# (dynamically linked) /bin/bash plus its 3 shared libs in verbatim, at the
# same paths, rather than silently rewriting the action's own argv[0] -- the
# rig should run what the action actually asked for.
cp /bin/bash "$root/bin/bash"
cp /lib64/ld-linux-x86-64.so.2 "$root/lib64/"
cp /lib/x86_64-linux-gnu/libtinfo.so.6 "$root/lib/x86_64-linux-gnu/"
cp /lib/x86_64-linux-gnu/libc.so.6 "$root/lib/x86_64-linux-gnu/"

# Size: busybox (~2.1MB static) + init (~1MB static) + slack for directories.
# No journal: this rootfs is shared read-only across every action's microVM
# (drives[].is_read_only=true in fc-entry.sh); a journal needs replay on an
# unclean mount, which fails outright on a read-only block device.
truncate -s 16M "$out"
mkfs.ext4 -q -F -O ^has_journal -d "$root" "$out"
echo "built $out ($(du -h "$out" | cut -f1))"
