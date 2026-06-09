#!/bin/sh
# gc dolt health-check — Parse `gc dolt health --json` for order outcomes.
#
# Reads a health JSON report from stdin, echoes it to stdout for diagnostics,
# and exits nonzero with a concise stderr message for critical data-plane
# failures. This lets the generic order runner record `order.failed` with a
# useful message without making `gc dolt health --json` itself fail before
# programmatic consumers can parse the report.
set -e

report=$(cat)
printf '%s\n' "$report"

json_field() {
  field="$1"
  if command -v jq >/dev/null 2>&1; then
    printf '%s\n' "$report" | jq -r "if $field == null then \"\" else $field end" 2>/dev/null || true
    return
  fi
  key=$(printf '%s' "$field" | sed 's/^\.server\.//')
  printf '%s\n' "$report" \
    | sed -n "/\"server\"[[:space:]]*:/,/}/p" \
    | sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}]*\\).*/\\1/p" \
    | head -1 \
    | tr -d ' "'
}

reachable=$(json_field ".server.reachable")
running=$(json_field ".server.running")
pid=$(json_field ".server.pid")
port=$(json_field ".server.port")
latency=$(json_field ".server.latency_ms")

case "$reachable" in
  true) exit 0 ;;
  false)
    echo "Dolt server unreachable: running=${running:-unknown} pid=${pid:-0} port=${port:-unknown} latency_ms=${latency:-0}" >&2
    exit 1
    ;;
  *)
    echo "Dolt health report missing server.reachable" >&2
    exit 1
    ;;
esac
