"""llvm_dist: the slice of an official LLVM release that C/C++ builds use.

LLVM's Linux release archive unpacks to ~12GB (MLIR, Flang, LLDB, clangd,
...), and toolchains_llvm puts every bin/ file into each cgo action's inputs.
This repository keeps clang, lld, the binutils replacements and the clang
resource headers (~700MB), fetched from the same pinned archive by sha256,
and lays them out as toolchains_llvm's own distribution repository does, so
it plugs in through `llvm.toolchain_root`.

The official binaries load a few shared libraries a host may lack (lld NEEDs
libxml2.so.2, which Ubuntu 26.04 replaced with libxml2.so.16). runtime_libs
copies them out of pinned .debs into lib/, the binaries' RUNPATH
($ORIGIN/../lib), so the toolchain starts on such hosts too. A DT_RUNPATH
covers only the binary's own NEEDED entries, not those of the libraries it
loads (libxml2.so.2 NEEDs ICU's libicuuc.so.74), so the rpath_members' tag
becomes DT_RPATH, which the loader also searches for their dependencies.
"""

load(":deb_sysroot.bzl", "fetch_debs")

# Copies each runtime_libs soname (dereferenced) into lib/, and each
# package's copyright next to it under share/doc/.
_BUNDLE_RUNTIME_LIBS = """
set -euo pipefail
src=_runtime_debs/usr/lib/x86_64-linux-gnu
for so in $LIBS; do
  if [ ! -e "$src/$so" ]; then
    echo "runtime_libs: no runtime_debs package provides $so" >&2
    exit 1
  fi
  cp -L "$src/$so" "lib/$so"
done
for c in _runtime_debs/usr/share/doc/*/copyright; do
  d=share/doc/$(basename "$(dirname "$c")")
  mkdir -p "$d"
  cp -L "$c" "$d/copyright"
done

# rd FILE OFFSET SIZE: the little-endian unsigned integer at OFFSET.
rd() { od -An -t u"$3" -j "$2" -N"$3" "$1" | tr -d ' \n'; }

# Rewrites FILE's DT_RUNPATH tag (29) to DT_RPATH (15) in place: the same
# path string, also searched for the dependencies of the libraries FILE
# loads. Only an ELF64 little-endian file whose PT_DYNAMIC holds a
# DT_RUNPATH is accepted.
runpath_to_rpath() {
  local f=$1 phoff phentsize phnum i p dyn size j tag
  if [ "$(rd "$f" 0 4)" != 1179403647 ] || [ "$(rd "$f" 4 1)" != 2 ] || [ "$(rd "$f" 5 1)" != 1 ]; then
    echo "rpath_members: $f is not an ELF64 little-endian file" >&2
    return 1
  fi
  phoff=$(rd "$f" 32 8)
  phentsize=$(rd "$f" 54 2)
  phnum=$(rd "$f" 56 2)
  for ((i = 0; i < phnum; i++)); do
    p=$((phoff + i * phentsize))
    [ "$(rd "$f" "$p" 4)" = 2 ] || continue
    dyn=$(rd "$f" $((p + 8)) 8)
    size=$(rd "$f" $((p + 32)) 8)
    for ((j = 0; j < size; j += 16)); do
      tag=$(rd "$f" $((dyn + j)) 8)
      [ "$tag" = 0 ] && break
      if [ "$tag" = 29 ]; then
        printf '\017' | dd of="$f" bs=1 seek=$((dyn + j)) conv=notrunc 2>/dev/null
        [ "$(rd "$f" $((dyn + j)) 8)" = 15 ] && return 0
        echo "rpath_members: rewriting $f's DT_RUNPATH failed" >&2
        return 1
      fi
    done
  done
  echo "rpath_members: $f has no DT_RUNPATH" >&2
  return 1
}
for m in $RPATH_MEMBERS; do
  runpath_to_rpath "$m"
done
"""

def _llvm_dist_impl(rctx):
    if rctx.attr.sliced:
        # A members-only, zstd archive of the same release (tools/cc_toolchain/
        # repack_llvm.sh): the same bytes as the tar -xJf below, extracted by
        # Bazel itself in seconds instead of a minute-plus of single-threaded xz
        # on every cold CI runner. Action keys are unchanged (identical files).
        rctx.download_and_extract(url = rctx.attr.urls, sha256 = rctx.attr.sha256, type = "tar.zst")
        missing = [m for m in rctx.attr.members if not rctx.path(m).exists]
        if missing:
            fail("llvm_dist: the sliced archive lacks %s" % ", ".join(missing))
    else:
        archive = "_llvm.tar.xz"
        rctx.download(url = rctx.attr.urls, output = archive, sha256 = rctx.attr.sha256)
        res = rctx.execute(
            ["tar", "-xJf", archive, "--strip-components=1"] +
            [rctx.attr.strip_prefix + "/" + m for m in rctx.attr.members],
            timeout = 3600,
        )
        if res.return_code != 0:
            fail("llvm_dist: extracting %s failed:\n%s" % (rctx.attr.urls[0], res.stderr))
        rctx.delete(archive)

    if rctx.attr.rpath_members and not rctx.attr.runtime_libs:
        fail("llvm_dist: rpath_members %s needs the runtime_libs they load" % rctx.attr.rpath_members)
    if rctx.attr.runtime_libs:
        fetch_debs(rctx, rctx.attr.runtime_debs, rctx.attr.runtime_deb_urls, "_runtime_debs")
        res = rctx.execute(
            ["bash", "-c", _BUNDLE_RUNTIME_LIBS],
            environment = {
                "LIBS": " ".join(rctx.attr.runtime_libs),
                "RPATH_MEMBERS": " ".join(rctx.attr.rpath_members),
            },
        )
        if res.return_code != 0:
            fail("llvm_dist: bundling runtime_libs failed:\n" + res.stderr)
        rctx.delete("_runtime_debs")

    # toolchains_llvm symlinks a fixed tool list into its toolchain package;
    # tools this repository leaves out get a stub that fails loudly if run.
    for tool in rctx.attr.stub_tools:
        rctx.file(
            "bin/" + tool,
            "#!/bin/sh\necho '%s is not part of this LLVM slice (third_party/cc_toolchain/llvm_dist.bzl)' >&2\nexit 1\n" % tool,
            executable = True,
        )

    major = rctx.attr.llvm_version.split(".")[0]
    rctx.file(
        "BUILD.bazel",
        rctx.read(Label("@toolchains_llvm//toolchain:BUILD.llvm_repo.tpl")).format(LLVM_VERSION = major) + """
# The bundled runtime_libs (third_party/cc_toolchain/llvm_dist.bzl). Not
# action inputs: the binaries find them through their RUNPATH or RPATH.
filegroup(
    name = "runtime_libs",
    srcs = {libs},
)
""".format(libs = repr(["lib/" + so for so in rctx.attr.runtime_libs])),
    )
    return rctx.repo_metadata(reproducible = True)

llvm_dist = repository_rule(
    implementation = _llvm_dist_impl,
    attrs = {
        "llvm_version": attr.string(mandatory = True),
        "urls": attr.string_list(mandatory = True),
        "sha256": attr.string(mandatory = True),
        "strip_prefix": attr.string(doc = "The release archive's top directory (unsliced archives only)."),
        "sliced": attr.bool(
            doc = "urls is a members-only .tar.zst made by repack_llvm.sh, not the release .tar.xz.",
        ),
        "members": attr.string_list(
            mandatory = True,
            doc = "Archive paths (below strip_prefix) to extract.",
        ),
        "rpath_members": attr.string_list(
            doc = "Members (real files, not symlinks) that load a runtime_libs library " +
                  "with dependencies of its own: their DT_RUNPATH becomes DT_RPATH.",
        ),
        "runtime_debs": attr.string_dict(
            doc = "Pool path (pool/<component>/...) to the sha256 of each .deb runtime_libs come from.",
        ),
        "runtime_deb_urls": attr.string_list(
            doc = "URL templates for runtime_debs, as deb_sysroot's urls.",
        ),
        "runtime_libs": attr.string_list(
            doc = "Sonames copied from runtime_debs' usr/lib/x86_64-linux-gnu into lib/, " +
                  "the binaries' RUNPATH: libraries they NEED that a host may lack.",
        ),
        "stub_tools": attr.string_list(
            doc = "bin/ tools toolchains_llvm links to but no action here runs.",
        ),
    },
)
