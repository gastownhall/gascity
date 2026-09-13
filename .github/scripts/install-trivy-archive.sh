#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-trivy-archive.sh VERSION [--cache]

Downloads a Trivy release tarball, verifies its pinned SHA-256, and installs
trivy. Use --cache on self-hosted runners to install under RUNNER_TOOL_CACHE/HOME
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
  Darwin) os_asset=macOS ;;
  Linux) os_asset=Linux ;;
  *)
    echo "Unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch_asset=ARM64 ;;
  x86_64|amd64) arch_asset=64bit ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_no_v="${version#v}"
tag="v${version_no_v}"
asset_platform="${os_asset}-${arch_asset}"
expected_sha=""
case "${tag}:${asset_platform}" in
  v0.70.0:Linux-64bit) expected_sha="8b4376d5d6befe5c24d503f10ff136d9e0c49f9127a4279fd110b727929a5aa9" ;;
  v0.70.0:Linux-ARM64) expected_sha="2f6bb988b553a1bbac6bdd1ce890f5e412439564e17522b88a4541b4f364fc8d" ;;
  v0.70.0:macOS-64bit) expected_sha="52d531452b19e7593da29366007d02a810e1e0080d02f9cf6a1afb46c35aaa93" ;;
  v0.70.0:macOS-ARM64) expected_sha="68e543c51dcc96e1c344053a4fde9660cf602c25565d9f09dc17dd41e13b838a" ;;
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

archive="trivy_${version_no_v}_${asset_platform}.tar.gz"
if [[ -z "$expected_sha" ]]; then
  expected_sha="$(github_release_asset_sha "aquasecurity/trivy" "$tag" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No Trivy checksum found for ${tag}/${asset_platform}" >&2
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
  bin_dir="${cache_root}/gascity-trivy/${tag}/${asset_platform}/bin"
else
  bin_dir="${TRIVY_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/trivy"
if [[ -x "$target" ]]; then
  echo "Reusing cached Trivy ${tag} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "${tmp}/${archive}" \
    "https://github.com/aquasecurity/trivy/releases/download/${tag}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "Trivy checksum mismatch for ${tag}/${asset_platform}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp" trivy
  install_target="${tmp}/trivy"
  if [[ ! -x "$install_target" ]]; then
    echo "trivy binary not found in ${archive}" >&2
    exit 1
  fi
  if $use_cache; then
    install_binary "$install_target" "$target"
  else
    install_binary_with_sudo_fallback "$install_target" "$target"
  fi
fi

if $use_cache && [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" --version
