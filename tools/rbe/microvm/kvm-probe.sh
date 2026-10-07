#!/usr/bin/env bash
# rbe-west microVM feasibility probe (bd mc: microvm single pool).
#
# Decides the feasibility gate from Q1 of rbe-microvm-single-pool-design.md:
# does a Blacksmith blacksmith-32vcpu-ubuntu-2404 runner expose nested KVM
# (/dev/kvm + vmx/svm), and does a Firecracker microVM actually boot on it?
#
# READ-ONLY on the farm: this talks to nothing on rbe-west. It downloads only
# pinned, checksummed artifacts (Firecracker release + the Firecracker-CI guest
# kernel), boots one throwaway microVM that runs `/bin/true`-equivalent inside,
# and prints a one-line verdict. Nothing is installed system-wide; everything
# lands in a mktemp dir that is removed on exit.
#
# Run it as a step on a throwaway branch of gascity (see workflow-dispatch.yml
# in this directory), NOT inline in rbe-worker-pool.yml / rbe-fork-pool.yml.
# The operator pushes the branch and dispatches it; this script never runs the
# NativeLink worker, so a bad probe cannot take a pool VM's slot.
#
# Exit 0 always (so the workflow step is green and the log is the artifact);
# the verdict line is MICROVM_KVM=yes|no and FC_BOOT=ok|skip|fail.
set -u

say() { printf '%s\n' "$*"; }
hr() { printf -- '---- %s ----\n' "$*"; }

# Pinned artifacts. Update deliberately; the probe verifies every checksum.
FC_VER=v1.17.0
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VER}/firecracker-${FC_VER}-x86_64.tgz"
FC_SHA=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
# Firecracker-CI guest kernel (6.1). Its config carries virtio-pmem, FS_DAX,
# overlayfs, vsock, ext4 — the features the per-action design relies on.
KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
KERNEL_SHA=e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2

d=$(mktemp -d /tmp/microvm-probe.XXXXXX) || { say "MICROVM_KVM=unknown FC_BOOT=fail (mktemp)"; exit 0; }
cleanup() { rm -rf --one-file-system "$d" 2>/dev/null || true; }
trap cleanup EXIT

hr "host"
say "kernel=$(uname -r) nproc=$(nproc) mem_gb=$(awk '/MemTotal/{printf "%.0f",$2/1048576}' /proc/meminfo)"
say "os=$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-?}")"
say "cpu=$(awk -F: '/model name/{print $2; exit}' /proc/cpuinfo | sed 's/^ //')"
say "virt=$(systemd-detect-virt 2>/dev/null || echo '?')"

hr "kvm capability"
vmx_svm=$(grep -cE '^flags.*\b(vmx|svm)\b' /proc/cpuinfo 2>/dev/null || echo 0)
dev_kvm=no; [ -c /dev/kvm ] && dev_kvm=yes
kvm_rw=no; [ -r /dev/kvm ] && [ -w /dev/kvm ] && kvm_rw=yes
say "dev_kvm=$dev_kvm (rw=$kvm_rw) vmx_svm_cpus=$vmx_svm"
[ -c /dev/kvm ] && { ls -l /dev/kvm; id; }
# kvm-ok is in the cpu-checker package; show it if present (never install it).
command -v kvm-ok >/dev/null && { hr "kvm-ok"; kvm-ok 2>&1 || true; }

KVM=no
if [ "$dev_kvm" = yes ] && [ "$vmx_svm" -gt 0 ]; then KVM=yes; fi

FC_BOOT=skip
if [ "$KVM" != yes ]; then
	say ""
	say "MICROVM_KVM=$KVM FC_BOOT=$FC_BOOT (no /dev/kvm or no vmx/svm: Firecracker/CH/Kata-QEMU ruled out; see design Q3)"
	exit 0
fi

# If /dev/kvm exists but is not group-accessible to us, show whether sudo helps.
SUDO=
if [ "$kvm_rw" != yes ]; then
	if sudo -n true 2>/dev/null; then SUDO="sudo -n"; say "note: /dev/kvm not rw for $(id -un); using passwordless sudo for the boot"; fi
fi

hr "download + verify (pinned)"
if ! curl -fsSL -o "$d/fc.tgz" "$FC_URL"; then say "MICROVM_KVM=$KVM FC_BOOT=fail (download firecracker)"; exit 0; fi
echo "${FC_SHA}  $d/fc.tgz" | sha256sum -c --quiet - || { say "MICROVM_KVM=$KVM FC_BOOT=fail (firecracker checksum)"; exit 0; }
if ! curl -fsSL -o "$d/vmlinux" "$KERNEL_URL"; then say "MICROVM_KVM=$KVM FC_BOOT=fail (download kernel)"; exit 0; fi
echo "${KERNEL_SHA}  $d/vmlinux" | sha256sum -c --quiet - || { say "MICROVM_KVM=$KVM FC_BOOT=fail (kernel checksum)"; exit 0; }
tar -C "$d" -xzf "$d/fc.tgz"
FC=$(echo "$d"/release-*/firecracker-"$FC_VER"-x86_64)
chmod +x "$FC"
say "firecracker=$("$FC" --version | head -1)"

hr "minimal rootfs (busybox from the host, or a 1-file initramfs)"
# A tiny ext4 rootfs with a single init that prints a cookie, syncs and powers
# off via the magic sysrq / reboot(2). We build init as a static C program so
# the probe needs no busybox/guest userland.
cat >"$d/init.c" <<'EOF'
#include <stdio.h>
#include <unistd.h>
#include <sys/reboot.h>
#include <linux/reboot.h>
int main(void){
    printf("MICROVM_GUEST_UP pid=%d uid=%d\n", getpid(), getuid());
    fflush(stdout);
    /* Firecracker has no ACPI power button wired for this path; LINUX_REBOOT
       with POWER_OFF from pid 1 exits the VM cleanly. */
    reboot(LINUX_REBOOT_CMD_RESTART);
    for(;;) pause();
    return 0;
}
EOF
if ! cc -static -O2 -o "$d/init" "$d/init.c" 2>"$d/cc.log"; then
	say "note: no static cc; trying musl/dash fallbacks"; cat "$d/cc.log"
fi
if [ ! -x "$d/init" ]; then
	# Fallback: use the host /bin/busybox or /bin/sh statically if present.
	for cand in /bin/busybox /usr/bin/busybox; do
		[ -x "$cand" ] && { cp "$cand" "$d/init"; break; }
	done
fi
if [ ! -x "$d/init" ]; then say "MICROVM_KVM=$KVM FC_BOOT=fail (no init binary; install gcc or busybox)"; exit 0; fi

# 16 MiB ext4 image with /init.
truncate -s 16M "$d/rootfs.ext4"
mkfs.ext4 -q -F "$d/rootfs.ext4" >/dev/null 2>&1 || { say "MICROVM_KVM=$KVM FC_BOOT=fail (mkfs.ext4 missing)"; exit 0; }
mnt="$d/mnt"; mkdir -p "$mnt"
if ! $SUDO mount -o loop "$d/rootfs.ext4" "$mnt" 2>"$d/mount.log"; then
	say "note: loop mount needs privilege; building rootfs via debugfs instead"
	# debugfs can write /init without mounting (unprivileged).
	printf 'cd /\nwrite %s init\n' "$d/init" | debugfs -w "$d/rootfs.ext4" >/dev/null 2>&1
else
	$SUDO cp "$d/init" "$mnt/init"; $SUDO chmod +x "$mnt/init"; sync; $SUDO umount "$mnt"
fi

hr "boot one microVM"
cat >"$d/vm.json" <<EOF
{
  "boot-source": { "kernel_image_path": "$d/vmlinux",
                   "boot_args": "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init root=/dev/vda rw" },
  "drives": [ { "drive_id": "rootfs", "path_on_host": "$d/rootfs.ext4", "is_root_device": true, "is_read_only": false } ],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": 128 }
}
EOF
log="$d/boot.log"
t0=$(date +%s.%N)
# Production workers get /dev/kvm via the kvm group; mirror that with an ACL
# rather than running the VMM as root.
[ -n "$SUDO" ] && $SUDO setfacl -m "u:$(id -un):rw" /dev/kvm && say "note: granted $(id -un) rw on /dev/kvm via ACL"
timeout 30 "$FC" --no-api --config-file "$d/vm.json" >"$log" 2>&1
rc=$?
t1=$(date +%s.%N)
ms=$(awk "BEGIN{printf \"%.0f\", ($t1-$t0)*1000}")
sed 's/^/  guest| /' "$log" | tail -40
if grep -q MICROVM_GUEST_UP "$log"; then
	FC_BOOT=ok
	say ""
	say "MICROVM_KVM=$KVM FC_BOOT=$FC_BOOT boot_plus_run_ms=$ms firecracker=$FC_VER kernel=6.1.155"
else
	FC_BOOT=fail
	say ""
	say "MICROVM_KVM=$KVM FC_BOOT=$FC_BOOT (guest did not print the cookie; rc=$rc, see guest log above)"
fi
exit 0
