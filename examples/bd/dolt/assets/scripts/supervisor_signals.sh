#!/bin/sh
# supervisor_signals.sh — read the supervisor's own recorded health signals
# from the session-reconciler trace, without touching dolt.
# Sourced by mol-dog-doctor.sh; unit-tested by test/dolt/supervisor_signals_test.sh.
#
# The controller records a `cycle_input_snapshot` trace record each reconcile
# tick. Its `fields` carry booleans that flag when a reconcile query could not
# fully read the beads store:
#
#   scale_check_query_partial — the pool scale-check query returned partial, so
#       supervisor pools may not scale up and queued work stalls (a scaleCheck
#       PARTIAL occurrence).
#   store_query_partial       — the store read returned partial; this is what a
#       tripped "dolt circuit breaker is open" fail-fast looks like from the
#       supervisor's side (store / circuit-breaker instability).
#   session_query_partial     — the session read returned partial (also store
#       instability; folded into the store signal).
#
# Reading these is a *local, dolt-free* probe: `gc trace show` reads the trace
# store the controller already wrote, so it adds no query load to the dolt
# server the watchdog is protecting — the watchdog's "SOFT" contract.
#
# The parser is deliberately split from the fetch (the doctor runs `gc trace
# show ... --json` and passes the text in) so it is unit-testable against canned
# JSON with no controller or dolt running.

# supervisor_flag_true FIELD JSON — exit 0 when the trace JSON contains at least
# one `"FIELD":true`. Blanks between the key and value are normalized so both
# `"f":true` and `"f": true` match; newlines are preserved so a key at the end
# of one line cannot join a value on the next. The values are JSON booleans, so
# matching `:true` (not `:false`) is unambiguous. Never trips the caller's
# `set -e`: it is only ever evaluated in an `if`/`||` condition.
supervisor_flag_true() {
  _sf_field="${1:-}"
  _sf_json="${2:-}"
  [ -n "$_sf_field" ] || return 1
  [ -n "$_sf_json" ] || return 1
  printf '%s' "$_sf_json" \
    | tr -d '[:blank:]' \
    | grep -q "\"${_sf_field}\":true"
}

# supervisor_signals JSON — echo a stable, space-separated set of active signal
# tokens found in the trace JSON:
#   "scalecheck"  when scale_check_query_partial is true anywhere in the window.
#   "store"       when store_query_partial OR session_query_partial is true.
# Empty output means no partial signals (the healthy case). The fixed order
# (scalecheck before store) keeps any signature built from it deterministic.
# Always succeeds.
supervisor_signals() {
  _ss_json="${1:-}"
  _ss_out=""
  if supervisor_flag_true "scale_check_query_partial" "$_ss_json"; then
    _ss_out="${_ss_out}scalecheck "
  fi
  if supervisor_flag_true "store_query_partial" "$_ss_json" \
     || supervisor_flag_true "session_query_partial" "$_ss_json"; then
    _ss_out="${_ss_out}store "
  fi
  printf '%s\n' "${_ss_out% }"
}
