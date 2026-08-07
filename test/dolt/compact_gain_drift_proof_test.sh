#!/bin/sh
# Unit test for the compact preservation proofs (Option A, #2846; widened for
# the concurrent-UPDATE case, #3341).
# Lib under test: examples/bd/dolt/assets/scripts/compact-gain-drift-proof.sh
#
# Stubs the run.sh-provided dependencies (query_single_cell, valid_table_name)
# so the classification is exercised without a live Dolt server. The full
# flatten path needs a concurrent-writer race that cannot be reproduced
# deterministically in a unit test (see #2846); this covers the decision logic.
#
# Two proofs live in the lib and they are deliberately different strengths:
#   * diff_is_additive_only      — added rows ONLY. Used for a table that
#     exists in no pre-flatten commit root, which must be wholly new.
#   * diff_has_no_removed_rows / drift_preserves_preflight_rows — no row
#     dropped. Used for the post-flatten drift proof, where a `modified` row is
#     a concurrent in-place UPDATE and keeps the row reachable.
# The stub models both DOLT_DIFF predicates separately so the gap between them
# (a modified-only diff) is pinned rather than assumed.
set -u

HERE=$(unset CDPATH; cd -- "$(dirname "$0")" && pwd)
LIB="$HERE/../../examples/bd/dolt/assets/scripts/compact-gain-drift-proof.sh"
[ -f "$LIB" ] || { echo "FAIL: lib not found at $LIB"; exit 1; }

# --- stubs for run.sh-provided helpers --------------------------------------
# query_single_cell <db> <msg> <query>: extract the table from the DOLT_DIFF
# query and echo the canned count for whichever diff_type predicate was asked
# for — stub_nonadded_<table> for `<> 'added'`, stub_removed_<table> for
# `= 'removed'` (both default 0).
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
  case "$3" in
    *"diff_type = 'removed'"*)
      case "$_t" in
        issues) printf '%s' "${stub_removed_issues:-0}" ;;
        mail) printf '%s' "${stub_removed_mail:-0}" ;;
        *) printf '0' ;;
      esac
      ;;
    *"diff_type <> 'added'"*)
      case "$_t" in
        issues) printf '%s' "${stub_nonadded_issues:-0}" ;;
        mail) printf '%s' "${stub_nonadded_mail:-0}" ;;
        *) printf '0' ;;
      esac
      ;;
    *)
      printf 'stub: unexpected diff predicate: %s\n' "$3" >&2
      return 1
      ;;
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
    stub_nonadded_issues stub_nonadded_mail \
    stub_removed_issues stub_removed_mail 2>/dev/null || true
}

# --- drift_preserves_preflight_rows (post-flatten drift proof) ---------------

# 1. single table, nothing dropped -> preserved (defer)
reset; stub_removed_issues=0
if drift_preserves_preflight_rows db H1 H2 "issues"; then ok "no removed rows single table -> defer"; else no "no removed rows single table -> defer"; fi

# 2. single table with a dropped row -> not preservable (quarantine)
reset; stub_removed_issues=2
if drift_preserves_preflight_rows db H1 H2 "issues"; then no "removed rows -> quarantine"; else ok "removed rows -> quarantine"; fi

# 3. THE #3341 CASE: concurrent in-place UPDATE — rows modified, none dropped.
# The additive-only proof refuses it; the preservation proof admits it. This is
# the whole point of the widening, so pin both verdicts on the same diff.
reset; stub_nonadded_issues=7; stub_removed_issues=0
if drift_preserves_preflight_rows db H1 H2 "issues"; then ok "modified-only (concurrent UPDATE) -> defer"; else no "modified-only (concurrent UPDATE) -> defer"; fi
if diff_is_additive_only db H1 H2 "issues"; then no "modified-only still fails the additive-only proof"; else ok "modified-only still fails the additive-only proof"; fi

# 4. two tables, neither drops a row -> preserved
reset; stub_removed_issues=0; stub_removed_mail=0
if drift_preserves_preflight_rows db H1 H2 "issues mail"; then ok "two preserving tables -> defer"; else no "two preserving tables -> defer"; fi

# 5. two tables, one drops a row -> not preservable
reset; stub_removed_issues=0; stub_removed_mail=5
if drift_preserves_preflight_rows db H1 H2 "issues mail"; then no "mixed tables -> quarantine"; else ok "mixed tables -> quarantine"; fi

# 6. diff probe failure on a table -> fail closed
reset; stub_removed_issues=0; STUB_FAIL_TABLE=mail
if drift_preserves_preflight_rows db H1 H2 "issues mail"; then no "probe failure -> quarantine"; else ok "probe failure -> quarantine"; fi

# 7. empty / non-numeric probe result -> fail closed
reset; STUB_EMPTY_TABLE=issues
if drift_preserves_preflight_rows db H1 H2 "issues"; then no "empty probe result -> quarantine"; else ok "empty probe result -> quarantine"; fi

# 8. empty table list -> not a proof
reset
if drift_preserves_preflight_rows db H1 H2 ""; then no "empty table list -> quarantine"; else ok "empty table list -> quarantine"; fi

# 9. missing from-head -> fail closed
reset; stub_removed_issues=0
if drift_preserves_preflight_rows db "" H2 "issues"; then no "missing from-head -> quarantine"; else ok "missing from-head -> quarantine"; fi

# 10. missing to-head -> fail closed
reset; stub_removed_issues=0
if drift_preserves_preflight_rows db H1 "" "issues"; then no "missing to-head -> quarantine"; else ok "missing to-head -> quarantine"; fi

# 11. invalid table name -> fail closed
reset; stub_removed_issues=0; STUB_INVALID_TABLE=issues
if drift_preserves_preflight_rows db H1 H2 "issues"; then no "invalid table name -> quarantine"; else ok "invalid table name -> quarantine"; fi

# --- diff_is_additive_only (first-committed table proof) ---------------------
# Still used by run.sh db_root_drift_within_verified_tables, where a table
# absent from the pre-flatten commit root must be wholly new to be provable.

# 12. purely additive -> proven
reset; stub_nonadded_issues=0
if diff_is_additive_only db H1 H2 "issues"; then ok "additive-only -> proven"; else no "additive-only -> proven"; fi

# 13. any non-added row -> fail closed
reset; stub_nonadded_issues=1
if diff_is_additive_only db H1 H2 "issues"; then no "non-added rows -> fail closed"; else ok "non-added rows -> fail closed"; fi

# 14. probe failure -> fail closed
reset; STUB_FAIL_TABLE=issues
if diff_is_additive_only db H1 H2 "issues"; then no "additive probe failure -> fail closed"; else ok "additive probe failure -> fail closed"; fi

# 15. invalid table name -> fail closed
reset; stub_nonadded_issues=0; STUB_INVALID_TABLE=issues
if diff_is_additive_only db H1 H2 "issues"; then no "additive invalid table name -> fail closed"; else ok "additive invalid table name -> fail closed"; fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
