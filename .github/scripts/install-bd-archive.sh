#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-bd-archive.sh VERSION [--cache]

Downloads a bd release tarball, verifies its pinned SHA-256, and installs bd.
Use --cache on self-hosted runners to install under RUNNER_TOOL_CACHE/HOME
and add that bin directory to GITHUB_PATH.
USAGE
}

version="${1:-}"
if [[ -z "$version" ]]; then
  usage
  exit 2
fi
shift || true

use_cache=false
while (($#)); do
  case "$1" in
    --cache) use_cache=true ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
  shift
done

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "Unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_no_v="${version#v}"
platform_tuple="${os}_${arch}"
expected_sha=""
case "${version}:${platform_tuple}" in
  v1.0.3:linux_amd64) expected_sha="1ef5dca818d7e81574df9e9f9fc2a16ab711da09b0fa7b822ae162d9a81c8912" ;;
  v1.0.3:linux_arm64) expected_sha="243a9c75012e794888fcafb957e7624b8fefdfef033d14cd03ebc9831c3bc12f" ;;
  v1.0.3:darwin_amd64) expected_sha="6bd75ac056288a5e8bbb203750e95af5a441d5ad1d20ca5511e60cd6c813e54b" ;;
  v1.0.3:darwin_arm64) expected_sha="fe6e4465751f46d9f3a670c3cf656714a171e44c8bc318fe19054f513b8306ed" ;;
  v1.0.0:linux_amd64) expected_sha="7057db1e92428fcf5c08d5dc6b07ead57e588b262cba78b9a26893d55bd29fdb" ;;
  v1.0.0:linux_arm64) expected_sha="9bb30413041e50dac945a0f8aa64011e4b345ebfd0a3f9b5fccd646c6dca61a7" ;;
  v1.0.0:darwin_amd64) expected_sha="9a3d5bca07c9ce809c205ef9a20f73de6503ab3714655239ce306d862ceeb0d0" ;;
  v1.0.0:darwin_arm64) expected_sha="b8763b428e6b68550eb2b2505483797794b49ae497a2e265ed3c60f0f0a0bcd2" ;;
esac

github_release_asset_sha() {
  local owner_repo="$1"
  local tag="$2"
  local asset="$3"
  if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required to resolve GitHub release asset checksums" >&2
    exit 1
  fi
  local auth_header=()
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    auth_header=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi
  curl -fsSL "${auth_header[@]}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${owner_repo}/releases/tags/${tag}" \
    | jq -r --arg asset "$asset" '.assets[] | select(.name == $asset) | .digest // empty' \
    | sed 's/^sha256://'
}

archive="beads_${version_no_v}_${platform_tuple}.tar.gz"
if [[ -z "$expected_sha" ]]; then
  expected_sha="$(github_release_asset_sha "gastownhall/beads" "$version" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No bd checksum found for ${version}/${platform_tuple}" >&2
    exit 1
  fi
fi

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

install_binary() {
  local src="$1"
  local dst="$2"
  mkdir -p "$(dirname "$dst")"
  install -m 0755 "$src" "$dst"
}

install_binary_with_sudo_fallback() {
  local src="$1"
  local dst="$2"
  local dst_dir
  dst_dir="$(dirname "$dst")"
  mkdir -p "$dst_dir"
  if [[ -w "$dst_dir" ]]; then
    install_binary "$src" "$dst"
  elif command -v sudo >/dev/null 2>&1; then
    sudo install -m 0755 "$src" "$dst"
  else
    echo "Cannot write $dst and sudo is unavailable" >&2
    exit 1
  fi
}

if $use_cache; then
  cache_root="${RUNNER_TOOL_CACHE:-$HOME/.local}"
  bin_dir="${cache_root}/gascity-bd/${version}/${platform_tuple}/bin"
else
  bin_dir="${BD_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/bd"
if [[ -x "$target" ]]; then
  echo "Reusing cached bd ${version} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "${tmp}/${archive}" \
    "https://github.com/gastownhall/beads/releases/download/${version}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "bd checksum mismatch for ${version}/${platform_tuple}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  src="${tmp}/bd"
  if [[ ! -x "$src" ]]; then
    src="${tmp}/beads_${version_no_v}_${platform_tuple}/bd"
  fi
  if $use_cache; then
    install_binary "$src" "$target"
  else
    install_binary_with_sudo_fallback "$src" "$target"
  fi
fi

if $use_cache && [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" version
