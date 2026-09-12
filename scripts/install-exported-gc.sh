#!/usr/bin/env bash
# Install a gc binary from an export-gc-bundle.sh archive.

set -euo pipefail

bundle_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
prefix="${1:-${GC_INSTALL_PREFIX:-$HOME/.local}}"

if [[ -z "$prefix" || "$prefix" == / ]]; then
    printf 'ERROR: refusing unsafe install prefix: %q\n' "$prefix" >&2
    exit 1
fi

source_binary="$bundle_dir/gc"
checksum_file="$bundle_dir/gc.sha256"
[[ -x "$source_binary" ]] || { printf 'ERROR: bundle has no executable gc\n' >&2; exit 1; }
[[ -f "$checksum_file" ]] || { printf 'ERROR: bundle has no gc.sha256\n' >&2; exit 1; }

expected="$(awk 'NR == 1 {print $1}' "$checksum_file")"
actual="$(shasum -a 256 "$source_binary" | awk '{print $1}')"
[[ -n "$expected" && "$actual" == "$expected" ]] || {
    printf 'ERROR: gc checksum mismatch; refusing install\n' >&2
    exit 1
}

install_dir="$prefix/bin"
target="$install_dir/gc"
mkdir -p "$install_dir"
temporary="$install_dir/.gc.tmp.$$"
backup=''

cleanup() {
    rm -f "$temporary"
}
trap cleanup EXIT INT TERM HUP

if [[ -e "$target" || -L "$target" ]]; then
    backup="$target.previous.$(date -u +%Y%m%dT%H%M%SZ)"
    mv -f "$target" "$backup"
fi

cp -f "$source_binary" "$temporary"
chmod 0755 "$temporary"
mv -f "$temporary" "$target"

if ! "$target" version --json >/dev/null 2>&1; then
    printf 'ERROR: installed gc cannot run on this machine; rolling back\n' >&2
    rm -f "$target"
    if [[ -n "$backup" ]]; then mv -f "$backup" "$target"; fi
    exit 1
fi

trap - EXIT INT TERM HUP
printf 'Installed gc to %s\n' "$target"
if [[ -n "$backup" ]]; then printf 'Previous command preserved at %s\n' "$backup"; fi

missing=()
for command_name in tmux jq git dolt bd flock; do
    if ! command -v "$command_name" >/dev/null 2>&1; then missing+=("$command_name"); fi
done
if ((${#missing[@]} > 0)); then
    printf 'Runtime dependencies still needed: %s\n' "${missing[*]}" >&2
fi
case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) printf 'Add %s to PATH before invoking gc.\n' "$install_dir" ;;
esac
