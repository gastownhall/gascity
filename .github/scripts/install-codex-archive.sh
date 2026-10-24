#!/usr/bin/env bash
# Native Linux installer for container builds; Docker owns the install cache.
set -euo pipefail

if [[ $# != 1 || ! "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Usage: install-codex-archive.sh VERSION" >&2
  exit 2
fi
version="$1"
[[ "$(uname -s)" == Linux ]] || { echo "Linux is required" >&2; exit 1; }
case "$(uname -m)" in
  aarch64|arm64) arch=aarch64 ;;
  x86_64|amd64) arch=x86_64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
triple="${arch}-unknown-linux-musl"
archive="codex-${triple}.tar.gz"
expected_sha=""
case "${version}:${arch}" in
  0.152.1:x86_64) expected_sha="a0ed1b40b1d597b340f09ae00ecebc46670b06cb52aac315b9dc84fed0289fd0" ;;
  0.152.1:aarch64) expected_sha="b65f964600972a948b898f4782e316a741a1b81c044622aa6bdf37ca4525debc" ;;
esac

# Renovate can advance VERSION before the checksum table is updated.
# Require the release asset's SHA-256 from GitHub for those versions.
if [[ -z "$expected_sha" ]]; then
  auth=()
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi
  digest="$(curl -fsSL --retry 3 "${auth[@]}" \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/openai/codex/releases/tags/rust-v${version}" \
    | jq -er --arg asset "$archive" '.assets[] | select(.name == $asset) | .digest')"
  [[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo "Missing SHA-256 for $archive" >&2; exit 1; }
  expected_sha="${digest#sha256:}"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp/$archive" \
  "https://github.com/openai/codex/releases/download/rust-v${version}/${archive}"
printf '%s  %s\n' "$expected_sha" "$tmp/$archive" | sha256sum --check --strict
# Extract only the expected binary, not arbitrary archive paths.
tar -xzOf "$tmp/$archive" "codex-${triple}" > "$tmp/codex"
bin_dir="${CODEX_INSTALL_BIN_DIR:-/usr/local/bin}"
install -d "$bin_dir"
install -m 0755 "$tmp/codex" "$bin_dir/codex"
"$bin_dir/codex" --version
