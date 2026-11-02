#!/usr/bin/env bash
# Regenerates internal/bdflags/inventory/*.json from bd's own cobra command
# tree — every command, alias and flag, hidden ones included, with flag types,
# defaults and NoOptDefVal — for each bd build gc bd's split-city allowlist is
# pinned to. It exports each bd ref into a temp dir, drops dump.go.tmpl into
# cmd/bd, and runs bd once with GC_BD_INVENTORY_DUMP set; nothing in the bd
# checkout is modified.
#
# usage: generate.sh <name>=<bd repo>@<ref>#<build tags> ...
#   The build tags are the ones the shipped binary is built with (beads
#   .goreleaser.yml: gms_pure_go; bd-enterprise Makefile's enterprise build:
#   enterprise,nometrics,gms_pure_go). Build tags add and remove commands.
#   e.g. generate.sh bd-1.3.2=/src/beads@v1.3.2#gms_pure_go \
#     bd-1.3.0-enterprise-77612097cec0=/src/bd-enterprise@77612097cec0#enterprise,nometrics,gms_pure_go
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out_dir="$here/../inventory"
mkdir -p "$out_dir"
for spec in "$@"; do
  name="${spec%%=*}"
  rest="${spec#*=}"
  tags="${rest##*#}"
  rest="${rest%#*}"
  repo="${rest%@*}"
  ref="${rest##*@}"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  git -C "$repo" archive "$ref" | tar -x -C "$tmp"
  cp "$here/dump.go.tmpl" "$tmp/cmd/bd/zz_gc_inventory_dump.go"
  commit="$(git -C "$repo" rev-parse "$ref^{commit}")"
  (cd "$tmp" && GC_BD_INVENTORY_DUMP="$out_dir/$name.json" GC_BD_INVENTORY_COMMIT="$commit" GC_BD_INVENTORY_TAGS="$tags" \
    go run -tags "$tags" ./cmd/bd </dev/null)
  # The binary embeds the gzipped copy; the JSON is what reviewers diff.
  gzip -n -9 -c "$out_dir/$name.json" >"$out_dir/$name.json.gz"
  echo "wrote $out_dir/$name.json and $name.json.gz"
  rm -rf "$tmp"
  trap - EXIT
done
