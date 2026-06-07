#!/bin/sh
# Unit test for gain_drift_is_additive_only (Option A preservation proof, #2846).
# Lib under test: examples/dolt/assets/scripts/compact-gain-drift-proof.sh
#
# Stubs the run.sh-provided dependencies (query_single_cell, valid_table_name)
# so the additive-only classification is exercised without a live Dolt server.
# The full flatten path needs a concurrent-writer race that cannot be reproduced
# deterministically in a unit test (see #2846); this covers the decision logic.
set -u

HERE=$(unset CDPATH; cd -- "$(dirname "$0")" && pwd)
LIB="$HERE/../../examples/dolt/assets/scripts/compact-gain-drift-proof.sh"
[ -f "$LIB" ] || { echo "FAIL: lib not found at $LIB"; exit 1; }

# --- stubs for run.sh-provided helpers --------------------------------------
# query_single_cell <db> <msg> <query>: extract the table from the DOLT_DIFF
# query and echo the canned non-added count stub_count_<table> (default 0).
# STUB_FAIL_TABLE forces a probe failure; STUB_EMPTY_TABLE forces an empty result.
query_single_cell() {
  _t=$(printf '%s\n' "$3" | sed -n "s/.*DOLT_DIFF('[^']*', *'[^']*', *'\([^']*\)').*/\1/p")
  if [ -n "${STUB_FAIL_TABLE:-}" ] && [ "$_t" = "$STUB_FAIL_TABLE" ]; then
    return 1
  fi
  if [ -n "${STUB_EMPTY_TABLE:-}" ] && [ "$_t" = "$STUB_EMPTY_TABLE" ]; then
    printf ''
    return 0
  fi
  case "$_t" in
    issues) printf '%s' "${stub_count_issues:-0}" ;;
    mail) printf '%s' "${stub_count_mail:-0}" ;;
    *) printf '0' ;;
  esac
  return 0
}
valid_table_name() {
  if [ -n "${STUB_INVALID_TABLE:-}" ] && [ "$1" = "$STUB_INVALID_TABLE" ]; then
    return 1
  fi
  return 0
}

# shellcheck disable=SC1090  # $LIB path is computed from the test's own location
. "$LIB"

# --- harness ----------------------------------------------------------------
pass=0
fail=0
ok() { pass=$((pass + 1)); printf 'ok   - %s\n' "$1"; }
no() { fail=$((fail + 1)); printf 'FAIL - %s\n' "$1"; }
reset() {
  unset STUB_FAIL_TABLE STUB_EMPTY_TABLE STUB_INVALID_TABLE \
    stub_count_issues stub_count_mail 2>/dev/null || true
}

# 1. single table, purely additive -> preserved (defer)
reset; stub_count_issues=0
if gain_drift_is_additive_only db H1 H2 "issues"; then ok "additive-only single table -> defer"; else no "additive-only single table -> defer"; fi

# 2. single table with removed/modified rows -> not preservable (quarantine)
reset; stub_count_issues=2
if gain_drift_is_additive_only db H1 H2 "issues"; then no "removed/modified -> quarantine"; else ok "removed/modified -> quarantine"; fi

# 3. two tables, both additive -> preserved
reset; stub_count_issues=0; stub_count_mail=0
if gain_drift_is_additive_only db H1 H2 "issues mail"; then ok "two additive tables -> defer"; else no "two additive tables -> defer"; fi

# 4. two tables, one not additive -> not preservable
reset; stub_count_issues=0; stub_count_mail=5
if gain_drift_is_additive_only db H1 H2 "issues mail"; then no "mixed tables -> quarantine"; else ok "mixed tables -> quarantine"; fi

# 5. diff probe failure on a table -> fail closed
reset; stub_count_issues=0; STUB_FAIL_TABLE=mail
if gain_drift_is_additive_only db H1 H2 "issues mail"; then no "probe failure -> quarantine"; else ok "probe failure -> quarantine"; fi

# 6. empty / non-numeric probe result -> fail closed
reset; STUB_EMPTY_TABLE=issues
if gain_drift_is_additive_only db H1 H2 "issues"; then no "empty probe result -> quarantine"; else ok "empty probe result -> quarantine"; fi

# 7. empty table list -> not a proof
reset
if gain_drift_is_additive_only db H1 H2 ""; then no "empty table list -> quarantine"; else ok "empty table list -> quarantine"; fi

# 8. missing from-head -> fail closed
reset; stub_count_issues=0
if gain_drift_is_additive_only db "" H2 "issues"; then no "missing from-head -> quarantine"; else ok "missing from-head -> quarantine"; fi

# 9. missing to-head -> fail closed
reset; stub_count_issues=0
if gain_drift_is_additive_only db H1 "" "issues"; then no "missing to-head -> quarantine"; else ok "missing to-head -> quarantine"; fi

# 10. invalid table name -> fail closed
reset; stub_count_issues=0; STUB_INVALID_TABLE=issues
if gain_drift_is_additive_only db H1 H2 "issues"; then no "invalid table name -> quarantine"; else ok "invalid table name -> quarantine"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
