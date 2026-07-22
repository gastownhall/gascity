#!/usr/bin/env bash
# Behavioral proof for the review-loop fixes to notify-on-human-gate-creation.sh:
#   Fix A (loud-fail): the script must exit NON-ZERO when a send fails (so the
#          controller — which logs an exec order's output only on non-zero exit —
#          actually surfaces the failure), and must NOT record the failed gate.
#   Fix D (event-shape): the gate filter must detect a gate in BOTH the API
#          envelope (.payload.bead.*) and the local-fallback raw form (.payload.*).
#
# Driven with a fake `gc` on PATH (the script's sourced _bd_trace.sh runs
# `command gc`, so a PATH shim is honored). No real town state is touched.
set -u
SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh"
H="$(mktemp -d)"; trap 'rm -rf "$H"' EXIT
mkdir -p "$H/bin" "$H/state"

cat > "$H/bin/gc" <<'FAKE'
#!/usr/bin/env bash
case "$1 $2" in
  "events "*) cat "$FAKE_EVENTS_FILE" ;;                      # gc events --type ...
  "rig list") echo '{"rigs":[]}' ;;                          # single-city, no rigs
  "bd show")  cat "$FAKE_GATE_FILE" ;;                        # gc bd show <id> --json
  "mail send")
     if [ "${FAKE_MAIL_RC:-0}" -eq 0 ]; then echo "Sent message m-1 to $3"; fi
     exit "${FAKE_MAIL_RC:-0}" ;;
  *) exit 0 ;;
esac
FAKE
chmod +x "$H/bin/gc"
export PATH="$H/bin:$PATH" GC_CITY="$H" GC_PACK_STATE_DIR="$H/state"
STATE="$H/state/notify-on-human-gate-creation-state.json"

# An OPEN human gate record returned by `gc bd show ... --json` (array form).
cat > "$H/gate.json" <<'JSON'
[{"id":"sc-gate01","await_type":"human","status":"open","assignee":"mayor","title":"T","description":"D","metadata":{}}]
JSON
export FAKE_GATE_FILE="$H/gate.json"

# Event shapes.
API_EVENT='{"type":"bead.created","payload":{"bead":{"id":"sc-gate01","issue_type":"gate"}}}'
RAW_EVENT='{"type":"bead.created","payload":{"id":"sc-gate01","issue_type":"gate"}}'
NONGATE_EVENT='{"type":"bead.created","payload":{"bead":{"id":"sc-msg01","issue_type":"message"}}}'

pass=0; fail=0
check() { # desc, expected_rc, actual_rc, expected_state_has, state_file
  local d="$1" erc="$2" arc="$3" ehas="$4" sf="$5" has="no"
  [ -f "$sf" ] && grep -q "sc-gate01" "$sf" && has="yes"
  if [ "$erc" = "$arc" ] && [ "$ehas" = "$has" ]; then
    echo "  PASS: $d (rc=$arc, state-has-gate=$has)"; pass=$((pass+1))
  else
    echo "  FAIL: $d (rc=$arc want $erc; state-has-gate=$has want $ehas)"; fail=$((fail+1))
  fi
}

echo "=== Scenario 1: API-envelope event, mail SUCCEEDS -> exit 0, gate recorded ==="
rm -f "$STATE"; printf '%s\n' "$API_EVENT" > "$H/ev"; export FAKE_EVENTS_FILE="$H/ev" FAKE_MAIL_RC=0
out="$(bash "$SCRIPT" 2>"$H/err")"; rc=$?
echo "  stdout: $out"; check "success path" 0 "$rc" yes "$STATE"

echo "=== Scenario 2 (Fix A): mail FAILS -> exit 1 (loud), gate NOT recorded ==="
rm -f "$STATE"; export FAKE_MAIL_RC=1
out="$(bash "$SCRIPT" 2>"$H/err")"; rc=$?
echo "  stderr: $(cat "$H/err")"; check "loud-fail nonzero + no dedup" 1 "$rc" no "$STATE"

echo "=== Scenario 3 (Fix D): RAW local-fallback event shape, mail SUCCEEDS -> gate detected ==="
rm -f "$STATE"; printf '%s\n' "$RAW_EVENT" > "$H/ev"; export FAKE_MAIL_RC=0
out="$(bash "$SCRIPT" 2>"$H/err")"; rc=$?
echo "  stdout: $out"; check "fallback-shape gate detected+notified" 0 "$rc" yes "$STATE"

echo "=== Scenario 4: non-gate creation -> exit 0, nothing notified ==="
rm -f "$STATE"; printf '%s\n' "$NONGATE_EVENT" > "$H/ev"
out="$(bash "$SCRIPT" 2>"$H/err")"; rc=$?
echo "  stdout: '${out}' (expect empty)"; check "non-gate ignored" 0 "$rc" no "$STATE"

echo ""
echo "=== renudge iso_to_epoch portability chain (GNU path + graceful-empty) ==="
gnu="$(date -u -d "2026-07-22T13:54:16Z" +%s 2>/dev/null || echo FAIL)"
bad="$(date -u -d "not-a-date" +%s 2>/dev/null || date -ju -f "%Y-%m-%dT%H:%M:%SZ" "not-a-date" +%s 2>/dev/null || echo "")"
echo "  GNU parse 2026-07-22T13:54:16Z -> $gnu (expect 1784728456)"
[ "$gnu" = "1784728456" ] && { echo "  PASS: GNU date path"; pass=$((pass+1)); } || { echo "  FAIL: GNU date path"; fail=$((fail+1)); }
echo "  garbage input -> '${bad}' (expect empty)"
[ -z "$bad" ] && { echo "  PASS: garbage -> empty (gate skipped, not misaged)"; pass=$((pass+1)); } || { echo "  FAIL"; fail=$((fail+1)); }

echo ""
echo "RESULT: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
