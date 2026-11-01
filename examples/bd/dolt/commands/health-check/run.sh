#!/bin/sh
# gc dolt health-check — Parse `gc dolt health --json` for order outcomes.
#
# Reads a health JSON report from stdin, echoes it to stdout for diagnostics,
# and exits nonzero with a concise stderr message for critical data-plane
# failures. This lets the generic order runner record `order.failed` with a
# useful message without making `gc dolt health --json` itself fail before
# programmatic consumers can parse the report.
set -e

PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)}"
. "$PACK_DIR/assets/scripts/proxied_scope.sh"

report=$(cat)
printf '%s\n' "$report"

# A bd-owned proxied scope has no gc-managed server, so `gc dolt health` sends
# a skip document instead of a server section. Recognize both the document and
# the scope itself: the order pipes the two commands together, and either end
# alone is enough to prove there is nothing to fail on.
case "$report" in
  *'"'"$GC_DOLT_PROXIED_SKIP_REASON"'"'*) exit 0 ;;
esac
if bd_owns_proxied_scope; then
  exit 0
fi

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

# server_string_field KEY — a string field of the server object. json_field's
# no-jq fallback strips spaces and quotes, which suits scalars but mangles free
# text; here the no-jq path keeps the value's JSON escapes (diagnostics only).
server_string_field() {
  if command -v jq >/dev/null 2>&1; then
    json_field ".server.$1"
    return
  fi
  printf '%s\n' "$report" \
    | sed -n "/\"server\"[[:space:]]*:/,/}/p" \
    | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\\(.*\\)\".*/\\1/p" \
    | head -1
}

reachable=$(json_field ".server.reachable")
running=$(json_field ".server.running")
pid=$(json_field ".server.pid")
port=$(json_field ".server.port")
latency=$(json_field ".server.latency_ms")
attempts=$(json_field ".server.probe_attempts")
probe_error=$(server_string_field probe_error)

case "$reachable" in
  true) exit 0 ;;
  false)
    msg="Dolt server unreachable: running=${running:-unknown} pid=${pid:-0} port=${port:-unknown} latency_ms=${latency:-0}"
    if [ -n "$attempts" ]; then msg="$msg attempts=$attempts"; fi
    if [ -n "$probe_error" ]; then msg="$msg error=$probe_error"; fi
    echo "$msg" >&2
    exit 1
    ;;
  *)
    echo "Dolt health report missing server.reachable" >&2
    exit 1
    ;;
esac
