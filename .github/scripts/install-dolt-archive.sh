#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-dolt-archive.sh VERSION [--cache]

Downloads a Dolt release tarball, verifies its pinned SHA-256, and installs
dolt. Use --cache on self-hosted runners to install under RUNNER_TOOL_CACHE/HOME
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

platform_tuple="${os}-${arch}"
expected_sha=""
case "${version}:${platform_tuple}" in
  1.86.6:linux-amd64) expected_sha="1f78bdc39edf4d4e731a53131b17d455fa0d1e2e872c0f5f8daaa44d07753a8b" ;;
  1.86.6:linux-arm64) expected_sha="1caa0aedc562ca63cfc24ee4b91287e5be7446aaeddc294f199f7515e5cfdc1f" ;;
  1.86.6:darwin-amd64) expected_sha="7ac44944c068c0bbb31ef91b032826f2e1aa0d5f5e4847e6c69bd31ea6d88dc5" ;;
  1.86.6:darwin-arm64) expected_sha="d27bb39ec5b86e425d06844e7f7e5495758adc41719a4fba99b842b89c8d68fc" ;;
  1.86.1:linux-amd64) expected_sha="37b4bd73b4c44fd1779115b35ab3e046a332ed99e563cf562882eb4fdb8bde86" ;;
  1.86.1:linux-arm64) expected_sha="5dc46c9db3cb2e8a3b5154ef972e502671520efdcdcdce0df644b67bab27d958" ;;
  1.86.1:darwin-amd64) expected_sha="563c9bae968e9d3dfa935eff36b06e91c16eed8b11d6a9c0d08e2b4629cdc458" ;;
  1.86.1:darwin-arm64) expected_sha="2e92b6aed60b2b02c4defc97fb48ca8b1c79d6994c645f690944c4c39a00d3a5" ;;
  1.85.0:linux-amd64) expected_sha="58e1462ddfbd59b2ccd707a12f70aa7597f1590745b546502049a03cb52e1aa2" ;;
  1.85.0:linux-arm64) expected_sha="f668c8e0d0276f684741ee66cd0dd18f2be8bf628a92982e8c7f20d1aef7b390" ;;
  1.85.0:darwin-amd64) expected_sha="7514c125cfb40f8a377e697a88535e21aa2e354f4bb62b7cabd6994604cb4af2" ;;
  1.85.0:darwin-arm64) expected_sha="67c5848ca13290722e8f49ec32cfa01140c4c64a3f55da3a5454aecbb59fc90d" ;;
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

archive="dolt-${platform_tuple}.tar.gz"
if [[ -z "$expected_sha" ]]; then
  expected_sha="$(github_release_asset_sha "dolthub/dolt" "v${version}" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No Dolt checksum found for ${version}/${platform_tuple}" >&2
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
  bin_dir="${cache_root}/gascity-dolt/${version}/${platform_tuple}/bin"
else
  bin_dir="${DOLT_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/dolt"
if [[ -x "$target" ]]; then
  echo "Reusing cached Dolt ${version} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "${tmp}/${archive}" \
    "https://github.com/dolthub/dolt/releases/download/v${version}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "Dolt checksum mismatch for ${version}/${platform_tuple}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  src="${tmp}/dolt-${platform_tuple}/bin/dolt"
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
