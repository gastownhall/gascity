#!/usr/bin/env bash
# Export a locally built gc as a self-verifying, platform-specific bundle.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
binary="$repo_root/bin/gc"
output_dir="$repo_root/dist"

usage() {
    printf '%s\n' \
        'Usage: scripts/export-gc-bundle.sh [--binary PATH] [--output DIR]' \
        '' \
        'Packages an existing gc binary with provenance, checksums, and an' \
        'atomic per-user installer. The archive is valid only for its recorded' \
        'operating system and architecture.'
}

while (($# > 0)); do
    case "$1" in
        --binary)
            [[ $# -ge 2 ]] || { printf 'ERROR: --binary requires a path\n' >&2; exit 2; }
            binary=$2
            shift 2
            ;;
        --output)
            [[ $# -ge 2 ]] || { printf 'ERROR: --output requires a directory\n' >&2; exit 2; }
            output_dir=$2
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            printf 'ERROR: unknown argument: %s\n' "$1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

[[ -x "$binary" ]] || {
    printf 'ERROR: gc binary is not executable: %s\n' "$binary" >&2
    printf 'Build it first with: make build\n' >&2
    exit 1
}
command -v jq >/dev/null 2>&1 || { printf 'ERROR: jq is required\n' >&2; exit 1; }

metadata="$("$binary" version --json)"
version="$(jq -er '.version | select(type == "string" and length > 0)' <<<"$metadata")"
commit="$(jq -er '.commit | select(type == "string" and length > 0)' <<<"$metadata")"
build_date="$(jq -er '.date | select(type == "string" and length > 0)' <<<"$metadata")"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) printf 'ERROR: unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

safe_version="$(tr -c 'A-Za-z0-9._-' '-' <<<"$version" | sed 's/-$//')"
safe_commit="$(tr -c 'A-Za-z0-9._-' '-' <<<"$commit" | sed 's/-$//')"
bundle_name="gascity_${safe_version}_${os}_${arch}_${safe_commit:0:16}"
archive="$output_dir/$bundle_name.tar.gz"
checksum_file="$archive.sha256"
work="$(mktemp -d "${TMPDIR:-/tmp}/gc-export.XXXXXX")"

cleanup() {
    rm -rf "$work"
}
trap cleanup EXIT INT TERM HUP

bundle="$work/$bundle_name"
mkdir -p "$bundle" "$output_dir"
cp -f "$binary" "$bundle/gc"
chmod 0755 "$bundle/gc"
cp -f "$repo_root/scripts/install-exported-gc.sh" "$bundle/install.sh"
chmod 0755 "$bundle/install.sh"

binary_sha="$(shasum -a 256 "$bundle/gc" | awk '{print $1}')"
printf '%s  %s\n' "$binary_sha" gc >"$bundle/gc.sha256"
jq -n \
    --arg schema_version '1' \
    --arg version "$version" \
    --arg commit "$commit" \
    --arg build_date "$build_date" \
    --arg os "$os" \
    --arg arch "$arch" \
    --arg binary_sha256 "$binary_sha" \
    '{schema_version: $schema_version, version: $version, commit: $commit, build_date: $build_date, os: $os, arch: $arch, binary_sha256: $binary_sha256}' \
    >"$bundle/manifest.json"

printf '%s\n' \
    "Gas City $version ($commit)" \
    "Platform: $os/$arch" \
    '' \
    'Verify the archive checksum beside the archive, extract it, then run:' \
    '  ./install.sh' \
    '' \
    'The default destination is ~/.local/bin/gc. Pass a prefix as the first' \
    'argument (for example ./install.sh /usr/local) to choose another location.' \
    'The installer does not remove Graphviz; put ~/.local/bin before Homebrew in' \
    'PATH when Graphviz also provides a command named gc.' \
    >"$bundle/README.txt"

tar -C "$work" -czf "$archive" "$bundle_name"
archive_sha="$(shasum -a 256 "$archive" | awk '{print $1}')"
printf '%s  %s\n' "$archive_sha" "$(basename "$archive")" >"$checksum_file"

printf 'Created %s\n' "$archive"
printf 'Checksum %s\n' "$checksum_file"
printf 'Binary   %s/%s · %s · %s\n' "$os" "$arch" "$version" "$commit"
