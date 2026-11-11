#!/usr/bin/env bash
# Verify the Go module cache against go.sum after go-mod-download-retry.sh,
# so a tampered or corrupt Actions cache entry can never reach a build.
#
# Why this is needed: once a module's .zip and .ziphash are in the download
# cache, the go command trusts the .ziphash text instead of re-hashing the
# zip (cmd/go/internal/modfetch DownloadZip returns early; Download's
# checkMod compares only the .ziphash text with go.sum). A cache entry
# holding a modified zip next to its original .ziphash would therefore pass
# go.sum checking and be extracted and built. `go mod verify` closes that
# gap: for every module in the build list it re-hashes the downloaded zip
# (dirhash.HashZip) and any extracted directory (dirhash.HashDir) and
# compares them with the .ziphash, which `go mod download` has just checked
# against go.sum. Cached .mod files need no extra step: the go command hashes
# them against go.sum on every read.
#
# On a verification failure this script reports an error annotation, purges
# the module cache, downloads everything again through the module proxy, and
# verifies once more, so a poisoned entry heals in this job; a second failure
# fails the step, and the cache save after it never runs.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

verify() {
  # Verification must judge what is on disk, never fetch a replacement.
  GOPROXY=off go mod verify
}

if verify; then
  exit 0
fi

modcache="$(go env GOMODCACHE)"
echo "::error title=Go module cache failed verification::go mod verify found module content that does not match go.sum; purging $modcache and downloading every module again" >&2
# Extracted module directories are read-only; make them removable first.
chmod -R u+w "$modcache"
rm -rf "$modcache"
"$BASH" "$script_dir/go-mod-download-retry.sh"

if verify; then
  echo "go-mod-verify-cache: module cache re-downloaded and verified against go.sum"
  exit 0
fi
echo "::error title=Go module verification failed::modules downloaded fresh from the module proxy still fail go mod verify" >&2
exit 1
