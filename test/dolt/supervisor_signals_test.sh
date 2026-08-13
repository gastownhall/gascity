#!/bin/sh
# Unit test for examples/bd/dolt/assets/scripts/supervisor_signals.sh.
#
# The dolt-health watchdog reads the supervisor's own reconcile trace to catch
# the beads store destabilizing — scale-check PARTIAL (pools may not scale, work
# stalls) and store/session PARTIAL (the supervisor-side shadow of a tripped
# "dolt circuit breaker is open"). The parse is split from the `gc trace show`
# fetch precisely so it can be proven here against canned JSON with no
# controller and no dolt running — which is also what keeps the signal SOFT.
#
# Run: sh test/dolt/supervisor_signals_test.sh
set -u
HERE=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
SUP_LIB="${SUP_LIB:-$HERE/../../examples/bd/dolt/assets/scripts/supervisor_signals.sh}"

if [ ! -f "$SUP_LIB" ]; then
  echo "FAIL: supervisor_signals helper not found at $SUP_LIB"
  exit 1
fi
# shellcheck disable=SC1090
. "$SUP_LIB"

fail=0
pass() { echo "PASS: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

for fn in supervisor_flag_true supervisor_signals; do
  command -v "$fn" >/dev/null 2>&1 || { echo "FAIL: $fn not defined"; exit 1; }
done

# eq NAME GOT WANT — assert string equality (shows the JSON case name).
eq() {
  if [ "$2" = "$3" ]; then pass "$1 -> [$2]"; else bad "$1: got '$2', want '$3'"; fi
}

# --- healthy / empty inputs yield no signals -------------------------------
eq "empty string"      "$(supervisor_signals '')"    ""
eq "empty array"       "$(supervisor_signals '[]')"  ""
eq "empty object"      "$(supervisor_signals '{}')"  ""
eq "all-false flags"   "$(supervisor_signals '{"fields":{"scale_check_query_partial":false,"store_query_partial":false,"session_query_partial":false}}')" ""

# --- individual signals ----------------------------------------------------
eq "scale-check PARTIAL" \
  "$(supervisor_signals '{"fields":{"scale_check_query_partial":true}}')" "scalecheck"
eq "store PARTIAL (store_query_partial)" \
  "$(supervisor_signals '{"fields":{"store_query_partial":true}}')" "store"
eq "store PARTIAL (session_query_partial folds into store)" \
  "$(supervisor_signals '{"fields":{"session_query_partial":true}}')" "store"

# --- both signals, in the fixed (deterministic) order ----------------------
eq "scale-check + store" \
  "$(supervisor_signals '{"fields":{"scale_check_query_partial":true,"store_query_partial":true}}')" "scalecheck store"

# --- whitespace tolerance: `"f": true` (space after colon) still matches ----
eq "space after colon" \
  "$(supervisor_signals '{ "fields": { "scale_check_query_partial": true } }')" "scalecheck"

# --- signal seen anywhere in the window (multi-record array) ----------------
eq "signal in a later record" \
  "$(supervisor_signals '[{"fields":{"scale_check_query_partial":false}},{"fields":{"store_query_partial":true}}]')" "store"

# --- no false positives -----------------------------------------------------
# A field whose name merely *extends* a signal key must not match: the parser
# keys on the exact `"<field>":true`, so `..._partial_ext":true` is not a hit.
eq "extended-key name is not a match" \
  "$(supervisor_signals '{"fields":{"scale_check_query_partial_ext":true}}')" ""
# Newlines are preserved, so a key at the end of one line cannot join a value on
# the next — this is deliberate; `gc trace show --json` emits each record's
# `:true` contiguously, and matching across a newline would be a false positive.
eq "key and value split across a newline is not a match" \
  "$(supervisor_signals "$(printf '{"scale_check_query_partial":\ntrue}')")" ""

# --- supervisor_flag_true direct contract ----------------------------------
if supervisor_flag_true "store_query_partial" '{"store_query_partial":true}'; then pass "flag_true true-case"; else bad "flag_true missed a true flag"; fi
if supervisor_flag_true "store_query_partial" '{"store_query_partial":false}'; then bad "flag_true matched a false flag"; else pass "flag_true false-case"; fi
if supervisor_flag_true "" '{"store_query_partial":true}'; then bad "flag_true matched an empty field name"; else pass "flag_true empty-field guarded"; fi

# --- SOFT contract: never abort the caller under set -e --------------------
# The doctor calls these inside command substitution under `set -e`; the
# no-signal (empty output) path must still exit 0, not abort the tick.
if ( set -e; . "$SUP_LIB"; supervisor_signals '' >/dev/null; supervisor_signals '{}' >/dev/null; true ); then
  pass "supervisor_signals does not abort under set -e"
else
  bad "supervisor_signals aborted a set -e caller"
fi

echo "----"
if [ "$fail" -eq 0 ]; then echo "ALL PASS"; else echo "FAILURES PRESENT"; fi
exit "$fail"
