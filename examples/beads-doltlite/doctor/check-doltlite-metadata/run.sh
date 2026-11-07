#!/usr/bin/env bash
set -euo pipefail

city_root="${GC_CITY:-${GC_CITY_PATH:-}}"
if [[ -z "$city_root" ]]; then
  city_root="$(pwd)"
fi

metadata="$city_root/.beads/metadata.json"
city_config="$city_root/city.toml"

if [[ ! -f "$city_config" ]]; then
  echo "city.toml not found at $city_config"
  exit 1
fi

if [[ ! -f "$metadata" ]]; then
  echo "beads metadata not found at $metadata"
  exit 1
fi

if ! grep -Eq '^[[:space:]]*backend[[:space:]]*=[[:space:]]*"doltlite"' "$city_config"; then
  echo "doltlite backend is not configured in city.toml; skipping metadata backend check"
  exit 0
fi

backend="$(sed -nE 's/^[[:space:]]*"backend"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/p' "$metadata" | head -1)"

if [[ "$backend" != "doltlite" ]]; then
  echo "configured [beads] backend is doltlite, but $metadata has backend=$backend"
  echo "repair: set .beads/metadata.json backend to \"doltlite\" or run the doltlite bootstrap/migration for this city"
  exit 1
fi

echo "doltlite metadata backend OK: $metadata"
