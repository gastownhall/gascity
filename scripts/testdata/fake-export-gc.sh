#!/usr/bin/env bash

set -euo pipefail

if [[ "${1:-}" == version && "${2:-}" == --json ]]; then
    printf '%s\n' '{"version":"test","commit":"0123456789abcdef","date":"2026-09-12T00:00:00Z"}'
    exit 0
fi
printf 'fake gc\n'
