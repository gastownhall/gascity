#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-claude-native.sh VERSION [--cache]

Installs the native Claude Code binary after verifying its pinned SHA-256.
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
  x86_64|amd64) arch=x64 ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

platform="${os}-${arch}"
expected_sha=""
case "${version}:${platform}" in
  2.1.123:darwin-arm64) expected_sha="44597dff0f1c11e37c1954d4ac3965909be376e5961b558345723357253bcc90" ;;
  2.1.123:darwin-x64) expected_sha="ddea227d4c2b2602d650d2c5d5c812f7680701a1504bcaff81e42c165c583ef9" ;;
  2.1.123:linux-arm64) expected_sha="825c526035d1d75ff0bc1eebf18c887f98d07ea49ea80bd312ff416fe61a39b3" ;;
  2.1.123:linux-x64) expected_sha="5a78139b679a86a88a0ac5476c706a64c3105bf6a6d435ba10f3aa3fb635bdb2" ;;
esac

if [[ -z "$expected_sha" ]]; then
  if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required to resolve Claude Code checksums for ${version}/${platform}" >&2
    exit 1
  fi
  manifest_url="https://downloads.claude.ai/claude-code-releases/${version}/manifest.json"
  expected_sha="$(curl -fsSL "$manifest_url" | jq -r --arg platform "$platform" '.platforms[$platform].checksum // empty')"
  if [[ -z "$expected_sha" ]]; then
    echo "No Claude Code checksum found for ${version}/${platform}" >&2
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
  if [[ -w "$(dirname "$dst")" ]]; then
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
  bin_dir="${cache_root}/gascity-claude/${version}/${platform}/bin"
else
  bin_dir="${CLAUDE_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/claude"
if [[ -x "$target" ]]; then
  echo "Reusing cached Claude Code ${version} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  binary="${tmp}/claude"
  url="https://downloads.claude.ai/claude-code-releases/${version}/${platform}/claude"
  curl -fsSL -o "$binary" "$url"
  actual_sha="$(sha256_file "$binary")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "Claude Code checksum mismatch for ${version}/${platform}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi

  if $use_cache; then
    install_binary "$binary" "$target"
  else
    install_binary_with_sudo_fallback "$binary" "$target"
  fi
fi

if $use_cache && [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" --version
