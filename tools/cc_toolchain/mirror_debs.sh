#!/usr/bin/env bash
# Publishes every Ubuntu package MODULE.bazel pins (deb_sysroot "packages")
# to gascity's content-addressed .deb mirror: release TAG, one asset per
# package named <sha256>.deb, the first of MODULE.bazel's NOBLE_DEB_URLS.
#
#   tools/cc_toolchain/mirror_debs.sh [TAG]
#
# TAG defaults to the release NOBLE_DEB_URLS names. Each package is downloaded
# from the snapshot it was pinned from (Launchpad's librarian as a fallback),
# checked against its pin, and uploaded unless the release already has it, so
# re-running after adding or bumping a package uploads just the new ones.
# Needs gh with write access to gastownhall/gascity.
set -euo pipefail
repo=gastownhall/gascity
root=$(git rev-parse --show-toplevel)
module="$root/MODULE.bazel"
mirror=$(grep -o "https://github.com/$repo/releases/download/[^/]*/{sha256}.deb" "$module")
tag=${1:-$(basename "$(dirname "$mirror")")}
snapshot=$(grep -o 'https://snapshot\.ubuntu\.com/ubuntu/[0-9TZ]*/' "$module" | head -1)

if ! gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
	gh release create "$tag" --repo "$repo" --prerelease --latest=false \
		--title "Ubuntu 24.04 packages for the Bazel C/C++ sysroots (${snapshot##*/ubuntu/})" \
		--notes "Toolchain artifact for Bazel, not a gascity release. The Ubuntu .debs MODULE.bazel pins (deb_sysroot packages), each named by its sha256 and byte-identical to ${snapshot}pool/...; published by tools/cc_toolchain/mirror_debs.sh."
fi
have=$(gh release view "$tag" --repo "$repo" --json assets -q '.assets[].name')

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
grep -oE '"pool/[^"]+_amd64\.deb": "[0-9a-f]{64}"' "$module" | tr -d '":' | sort -u |
	while read -r path sha; do
		if grep -qx "$sha.deb" <<<"$have"; then
			continue
		fi
		out="$work/$sha.deb"
		curl -fsSL --retry 5 -o "$out" "$snapshot$path" ||
			curl -fsSL --retry 5 -o "$out" "https://launchpad.net/ubuntu/+archive/primary/+files/${path##*/}"
		echo "$sha  $out" | sha256sum -c --quiet -
		gh release upload "$tag" --repo "$repo" "$out"
		echo "mirrored $path -> $sha.deb"
		rm -f "$out"
	done
