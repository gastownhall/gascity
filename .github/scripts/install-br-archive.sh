#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-br-archive.sh VERSION [--cache]

Downloads a br release tarball, verifies its pinned SHA-256, and installs br.
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
tag="v${version_no_v}"
platform_tuple="${os}_${arch}"
expected_sha=""
case "${tag}:${platform_tuple}" in
  v0.1.20:linux_amd64) expected_sha="aefc2ef6b16c7b275f6890636c110540c7bc081e203a1e8a706a376207d1f9dd" ;;
  v0.1.20:linux_arm64) expected_sha="20899316274b7ac40de477f3318a3d6391f7885c6cd1bec7ba10e828360207fb" ;;
  v0.1.20:darwin_amd64) expected_sha="b53f109e3f288d23d2918bc9dcf7fa9997351d79bfab6be54ca18bc41d504d58" ;;
  v0.1.20:darwin_arm64) expected_sha="705a13ab7c972bff97440656633210ca2c88cd49c1094a6007a98983d73fbb1d" ;;
esac

github_release_asset_sha() {
  local owner_repo="$1"
  local release_tag="$2"
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
    "https://api.github.com/repos/${owner_repo}/releases/tags/${release_tag}" \
    | jq -r --arg asset "$asset" '.assets[] | select(.name == $asset) | .digest // empty' \
    | sed 's/^sha256://'
}

archive="br-v${version_no_v}-${platform_tuple}.tar.gz"
if [[ -z "$expected_sha" ]]; then
  expected_sha="$(github_release_asset_sha "Dicklesworthstone/beads_rust" "$tag" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No br checksum found for ${tag}/${platform_tuple}" >&2
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
  bin_dir="${cache_root}/gascity-br/${tag}/${platform_tuple}/bin"
else
  bin_dir="${BR_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/br"
if [[ -x "$target" ]]; then
  echo "Reusing cached br ${tag} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "${tmp}/${archive}" \
    "https://github.com/Dicklesworthstone/beads_rust/releases/download/${tag}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "br checksum mismatch for ${tag}/${platform_tuple}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  src="${tmp}/br"
  if [[ ! -x "$src" ]]; then
    src="$(find "$tmp" -type f -name br -perm -111 | head -n 1)"
  fi
  if [[ -z "${src:-}" || ! -x "$src" ]]; then
    echo "br binary not found in ${archive}" >&2
    exit 1
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

"$target" --version
