#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work="$(mktemp -d "${TMPDIR:-/tmp}/gc-export-test.XXXXXX")"

cleanup() {
    rm -rf "$work"
}
trap cleanup EXIT INT TERM HUP

fake_gc="$work/fake-gc"
cp -f "$repo_root/scripts/testdata/fake-export-gc.sh" "$fake_gc"
chmod 0755 "$fake_gc"

"$repo_root/scripts/export-gc-bundle.sh" --binary "$fake_gc" --output "$work/out" >/dev/null
archive="$(find "$work/out" -maxdepth 1 -type f -name '*.tar.gz' -print -quit)"
[[ -n "$archive" ]]
(cd "$work/out" && shasum -a 256 -c "$(basename "$archive").sha256" >/dev/null)
mkdir -p "$work/unpacked"
tar -C "$work/unpacked" -xzf "$archive"
bundle="$(find "$work/unpacked" -mindepth 1 -maxdepth 1 -type d -print -quit)"
"$bundle/install.sh" "$work/prefix" >/dev/null
[[ -x "$work/prefix/bin/gc" ]]
[[ "$("$work"/prefix/bin/gc version --json)" == *'"version":"test"'* ]]
printf 'PASS: gc export bundle is checksummed, extractable, and installable\n'
