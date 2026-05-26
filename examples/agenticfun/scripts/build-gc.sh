#!/usr/bin/env bash
set -euo pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
city_dir="$(CDPATH= cd -- "${script_dir}/.." && pwd)"
repo_root="$(CDPATH= cd -- "${city_dir}/../.." && pwd)"
bin_dir="${GC_AGENTICFUN_BIN_DIR:-${city_dir}/.gc/bin}"
gc_bin="${bin_dir}/gc"

mkdir -p "${bin_dir}"

(
  cd "${repo_root}"
  go build -trimpath '-ldflags=-s -w' -o "${gc_bin}" ./cmd/gc
)

printf 'Built %s\n' "${gc_bin}"
printf 'Run this before starting AgenticFun:\n'
printf '  export PATH="%s:${PATH}"\n' "${bin_dir}"
printf '  gc start %s\n' "${city_dir}"
