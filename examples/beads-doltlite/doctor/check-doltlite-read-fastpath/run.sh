#!/usr/bin/env bash
set -euo pipefail

city_root="${GC_CITY:-${GC_CITY_PATH:-}}"
if [[ -z "$city_root" ]]; then
  city_root="$(pwd)"
fi

if [[ ! -f "$city_root/city.toml" ]] || [[ ! -f "$city_root/.beads/metadata.json" ]]; then
  echo "city root is missing city.toml or .beads/metadata.json: $city_root"
  exit 1
fi

if ! grep -Eq '^[[:space:]]*backend[[:space:]]*=[[:space:]]*"doltlite"' "$city_root/city.toml"; then
  echo "doltlite backend is not configured in city.toml; skipping fast-path check"
  exit 0
fi

metadata_backend="$(sed -nE 's/^[[:space:]]*"backend"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "$city_root/.beads/metadata.json" | head -1)"
if [[ "$metadata_backend" != "doltlite" ]]; then
  echo "cannot check fast path: .beads/metadata.json backend=$metadata_backend"
  echo "repair: set .beads/metadata.json backend to \"doltlite\""
  exit 1
fi

candidate="${GC_DOLTLITE_DOCTOR_BEAD_ID:-}"
if [[ -z "$candidate" ]]; then
  candidate="$(bd list --json 2>/dev/null | sed -nE 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' | head -1 || true)"
fi

if [[ -z "$candidate" ]]; then
  echo "no bead id available; skipping doltlite read-path responsiveness check"
  exit 0
fi

out="$(mktemp)"
err="$(mktemp)"
trap 'rm -f "$out" "$err"' EXIT

if ! (
  cd "$city_root"
  GC_NATIVE_DOLTLITE_BEADS=1 timeout 10s gc beads show "$candidate" >"$out" 2>"$err"
); then
  echo "doltlite read path failed for bead $candidate"
  sed -n '1,10p' "$err"
  exit 1
fi

echo "doltlite read path OK with GC_NATIVE_DOLTLITE_BEADS=1: gc beads show $candidate"
