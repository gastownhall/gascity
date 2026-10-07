#!/usr/bin/env bash
# fc-entry.sh -- S2 rig "entrypoint" for a NativeLink local worker.
#
# NativeLink spawns this in the action's exec_root with the action's own env,
# and argv = [this script's original argv position, then the real command].
# Analogous to the farm's `rbe-action-entry.c` -> `launch` -> `rbe-action-launch`
# chain (design 4.4), collapsed into one script for the rig: stage the cwd
# into a per-action ext4 disk (unprivileged, `mkfs.ext4 -d`, same trick as the
# S1 prototype and the S0 probe), boot the shared toolset rootfs + that disk
# in a Firecracker microVM with NO network device, run the real command as
# the guest's pid-1 child, and relay its stdout/stderr/exit code back
# faithfully so NativeLink's result (and AC caching decision) reflects the
# in-guest execution, not anything that ran on the host.
#
# Env knobs (set by the worker config via `additional_environment`, or by
# hand for manual runs):
#   MICROVM_BIN_DIR   -- dir with firecracker, vmlinux, toolset-rootfs.ext4
#   MICROVM_TAG       -- "oss" or "oss-fork", only used for log prefixing
set -uo pipefail

# The action's own declared env (set by the client, e.g. Bazel's default
# minimal spawn env) becomes THIS script's environment too -- NativeLink
# doesn't merge it with the worker process's own PATH. mkfs.ext4/debugfs
# live in /usr/sbin, which a bare "/bin:/usr/bin:/usr/local/bin" action PATH
# doesn't include; without `set -e` that failure was silent (stderr was
# redirected to /dev/null below), leaving the action disk as unformatted
# zeros and the guest failing to mount vdb. Force a PATH that always finds
# our own host-side tooling, independent of whatever the action asked for.
export PATH="/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"

bin=${MICROVM_BIN_DIR:-/data/tmp/microvm-s2/bin}
tag=${MICROVM_TAG:-action}
fc="$bin/firecracker"
vmlinux="$bin/vmlinux"
rootfs="$bin/toolset-rootfs.ext4"

t0=$(date +%s.%N)
work=$(mktemp -d /data/tmp/microvm-s2/run/"$tag".XXXXXX) || exit 97
cleanup() { [ -n "${MICROVM_KEEP_WORK:-}" ] || rm -rf --one-file-system "$work"; }
trap cleanup EXIT

real_cwd=$(pwd)

# --- stage (host) --------------------------------------------------------
stage="$work/stage"
mkdir -p "$stage/work"
# Hardlink the exec_root in (cheap; matches the design's "inputs are
# hardlinks of the CAS" staging model -- see 4.3). Falls back to copy if the
# action disk's tmpfs and the exec_root are on different filesystems.
cp -al "$real_cwd/." "$stage/work/" 2>/dev/null || cp -a "$real_cwd/." "$stage/work/"

{
	printf '#!/bin/sh\n'
	printf 'echo "MICROVM_GUEST kernel=$(uname -r) tag=%s"\n' "$tag"
	printf 'cd /mnt/work || exit 98\n'
	# argv is passed as a NUL-safe-ish shell-quoted command line; good enough
	# for this rig's own demo actions (no untrusted shell metacharacters).
	printf 'exec'
	for a in "$@"; do printf ' %q' "$a"; done
	printf '\n'
} > "$stage/run.sh"
chmod +x "$stage/run.sh"

: > "$stage/env.txt"
while IFS='=' read -r -d '' name value; do
	# Skip a few host-only/noisy vars; everything else the action set is
	# passed through verbatim.
	case "$name" in BASH_FUNC_*|_) continue ;; esac
	printf '%s=%s\n' "$name" "$value" >> "$stage/env.txt"
done < <(env -0)

action_disk="$work/action.ext4"
truncate -s 256M "$action_disk" || { echo "fc-entry: truncate failed" >&2; exit 96; }
mkfs.ext4 -q -F -d "$stage" "$action_disk" >/dev/null 2>"$work/mkfs.err" \
	|| { echo "fc-entry: mkfs.ext4 failed:" >&2; cat "$work/mkfs.err" >&2; exit 96; }
t1=$(date +%s.%N)

# --- boot + run (microVM) -------------------------------------------------
vmcfg="$work/vm.json"
cat > "$vmcfg" <<EOF
{
  "boot-source": { "kernel_image_path": "$vmlinux",
                   "boot_args": "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init root=/dev/vda rw" },
  "drives": [
    { "drive_id": "rootfs", "path_on_host": "$rootfs", "is_root_device": true, "is_read_only": true },
    { "drive_id": "action", "path_on_host": "$action_disk", "is_root_device": false, "is_read_only": false }
  ],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": 512 },
  "network-interfaces": []
}
EOF
# No network-interfaces entry at all: no tap is ever attached, for either
# tier, in this rig (3.1: fork tier always off; the rig does not yet
# implement the oss tier's opt-in `network` property -- see report).
boot_log="$work/boot.log"
timeout 30 "$fc" --no-api --config-file "$vmcfg" > "$boot_log" 2>&1
fc_rc=$?
t2=$(date +%s.%N)

guest_up=0
grep -q MICROVM_GUEST_UP "$boot_log" && guest_up=1

# --- extract (host, unprivileged via debugfs) -----------------------------
out="$work/stdout.log"; err="$work/stderr.log"; rc_file="$work/exit_code"
debugfs -R "dump /stdout.log $out" "$action_disk" >/dev/null 2>&1
debugfs -R "dump /stderr.log $err" "$action_disk" >/dev/null 2>&1
debugfs -R "dump /exit_code $rc_file" "$action_disk" >/dev/null 2>&1
# Copy any files the action wrote back under /work (declared outputs) back
# into the real exec_root so NativeLink can find + upload them.
rm -rf "$work/workout"
mkdir -p "$work/workout"
# debugfs warns (harmlessly, to stderr) that it can't chown as a non-root
# caller; the dump itself still succeeds, so don't let that warning show up
# as a fatal error.
debugfs -R "rdump /work $work/workout" "$action_disk" >/dev/null 2>&1
if [ -d "$work/workout/work" ]; then
	cp -a "$work/workout/work/." "$real_cwd/" 2>/dev/null
fi
t3=$(date +%s.%N)

awk -v a="$t0" -v b="$t1" -v c="$t2" -v d="$t3" -v tag="$tag" \
	'BEGIN{printf "MICROVM_TIMING tag=%s stage_ms=%.0f boot_run_ms=%.0f extract_ms=%.0f total_ms=%.0f\n", tag,(b-a)*1000,(c-b)*1000,(d-c)*1000,(d-a)*1000}' \
	>&2

if [ "$guest_up" != 1 ]; then
	echo "fc-entry: guest never came up (fc_rc=$fc_rc); boot log:" >&2
	sed 's/^/fc-entry boot| /' "$boot_log" >&2
	exit 99
fi

[ -f "$out" ] && cat "$out"
[ -f "$err" ] && cat "$err" >&2
if [ -f "$rc_file" ]; then
	exit "$(tr -dc '0-9' < "$rc_file")"
fi
echo "fc-entry: no exit_code side channel written by guest" >&2
exit 99
