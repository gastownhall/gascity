#!/usr/bin/env bash
# The pinned LLVM toolchain's own executables must start on any glibc 2.34+
# Linux x86_64 host, not just on the Ubuntu 24.04 the RBE workers run.
#
#   llvm_dist_runtime_libs_test.sh READELF FILE...
#
# FILE... are the llvm_dist repository's bin/ and lib/ files as runfiles
# (MODULE.bazel's llvm_dist_linux_x86_64). Every shared library an ELF among
# them NEEDs must be one of HOST_SONAMES, whose ABI every such host keeps
# under the same soname, or be bundled in the repository's lib/, which is the
# official binaries' RUNPATH ($ORIGIN/../lib). Anything else (ICU's
# per-release sonames, libxml2.so.2 that Ubuntu 26.04 replaced with
# libxml2.so.16) fails the toolchain on a newer host. Then ld.lld, the one
# binary that NEEDs a bundled library, has to actually start.
set -euo pipefail
readelf=${1:?llvm-readelf}
shift

HOST_SONAMES=" ld-linux-x86-64.so.2 libc.so.6 libm.so.6 libgcc_s.so.1 libstdc++.so.6 libz.so.1 liblzma.so.5 "

root=""
for f in "$@"; do
	case $f in
	*/bin/ld.lld)
		root=${f%/bin/ld.lld}
		;;
	esac
done
if [ -z "$root" ]; then
	echo "FAIL: no bin/ld.lld among the inputs" >&2
	exit 1
fi

bundled=" "
for f in "$@"; do
	case $f in
	"$root"/lib/*.so*)
		bundled+="${f##*/} "
		;;
	esac
done

fail=0
checked=0
for f in "$@"; do
	[ "$(head -c4 "$f" | od -An -c | tr -d ' ')" = '177ELF' ] || continue
	checked=$((checked + 1))
	while IFS= read -r soname; do
		case $HOST_SONAMES$bundled in
		*" $soname "*) ;;
		*)
			echo "FAIL: ${f#"$root"/} NEEDs $soname: neither a stable host soname nor bundled in lib/" >&2
			fail=1
			;;
		esac
	done < <("$readelf" --needed-libs "$f" | sed -n 's/^  \([^ ].*\)$/\1/p')
done
if [ "$checked" -eq 0 ]; then
	echo "FAIL: no ELF files among the inputs" >&2
	exit 1
fi

if ! out=$(env -u LD_LIBRARY_PATH "$root/bin/ld.lld" --version 2>&1); then
	echo "FAIL: $root/bin/ld.lld --version: $out" >&2
	fail=1
fi
[ "$fail" -eq 0 ] || exit 1
echo "ok: $checked ELF files; bundled:${bundled% }; $out"
