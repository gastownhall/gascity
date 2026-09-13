#!/usr/bin/env bash
# Regenerate internal/api/genclient/client_gen.go from the live spec.
#
# Writing via a temp file avoids self-truncation: a direct
# `go run ./cmd/gen-client > client_gen.go` redirect zeroes
# client_gen.go before the compile step reads it, and the compile
# step depends on the genclient package — so the build fails before
# producing any output. Redirect to a temp file, then mv atomically.
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
target="$repo_root/internal/api/genclient/client_gen.go"
tmp=$(mktemp -t gc-client-gen.XXXXXX.go)
trap 'rm -f "$tmp"' EXIT

(cd "$repo_root" && go run ./cmd/gen-client) > "$tmp"
mv "$tmp" "$target"
