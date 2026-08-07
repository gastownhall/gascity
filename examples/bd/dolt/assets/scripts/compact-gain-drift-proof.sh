#!/bin/sh
# compact-gain-drift-proof.sh — Option A row-preservation proof for post-flatten
# table value-hash drift (gastownhall/gascity#2846, widened for #3341).
#
# When verify_counts sees a table's value hash drift, the safety property at
# stake ("pre-flight rows remain reachable") cannot be inferred from HEAD
# movement alone: a concurrent writer whose commit is ABSORBED into the flatten
# commit moves no HEAD, so the HEAD-proven gate misses it and a benign race is
# hard-quarantined — which then blocks all future GC of a busy DB (the
# memory-exhaustion failure the code calls out).
#
# This proves preservation DIRECTLY: for each drifted table, diff the pre-flight
# snapshot HEAD against the flatten commit. `added` rows are a concurrent
# INSERT and `modified` rows a concurrent UPDATE; neither can drop a pre-flight
# row, so a diff carrying no `removed` row proves every pre-flight row survived
# and the drift is concurrent-writer data — defer, exactly as the HEAD-proven
# path does. It is strictly more rigorous than the HEAD proxy: it proves
# reachability instead of inferring it. Any removed row, or any probe failure,
# fails closed and falls through to quarantine.
#
# Depends on `query_single_cell` and `valid_table_name` from run.sh.

# diff_is_additive_only <db> <from_head> <to_head> <table>
# Returns 0 iff the table's <from>..<to> content diff contains only `added`
# rows. Returns non-zero (fail closed) if either commit endpoint is missing,
# the table name is missing or invalid, the diff probe fails or returns a
# non-numeric result, or the table shows removed/modified rows. Used by the
# committed-root drift proof's first-committed table case (run.sh
# db_root_drift_within_verified_tables), where the table exists in no
# pre-flatten commit root and so must be wholly new to be provable — a
# strictly stronger claim than the row-preservation proof below.
diff_is_additive_only() {
  _da_db="$1"
  _da_from="$2"
  _da_to="$3"
  _da_t="$4"
  # Without both commit endpoints there is nothing to diff against — fail closed.
  [ -n "$_da_from" ] && [ -n "$_da_to" ] && [ -n "$_da_t" ] || return 1
  valid_table_name "$_da_t" || return 1
  # Count rows that are NOT purely additive between the two commits. Zero means
  # every row present at <from> is reachable unchanged at <to> and the only
  # change was added rows.
  if ! _da_nonadded=$(query_single_cell "$_da_db" \
    "preservation diff probe failed for table=$_da_t" \
    "SELECT COUNT(*) FROM DOLT_DIFF('$_da_from', '$_da_to', '$_da_t') WHERE diff_type <> 'added'"); then
    return 1
  fi
  case "$_da_nonadded" in
    0) return 0 ;;             # only added rows — this table's <from> rows preserved
    ''|*[!0-9]*) return 1 ;;   # empty/non-numeric probe result — fail closed
    *) return 1 ;;             # one or more removed/modified rows — not preservable
  esac
}

# diff_has_no_removed_rows <db> <from_head> <to_head> <table>
# Returns 0 iff the table's <from>..<to> content diff contains no `removed`
# rows. Returns non-zero (fail closed) if either commit endpoint is missing,
# the table name is missing or invalid, the diff probe fails or returns a
# non-numeric result, or the table shows removed rows.
#
# This is the preservation claim itself rather than a proxy for it, and it
# subsumes the additive-only case: `added` rows are a concurrent INSERT and
# `modified` rows a concurrent UPDATE — an UPDATE rewrites a row's non-key
# values but keeps the row reachable, so only a `removed` row is loss. A
# concurrent DELETE does show as `removed` here and correctly fails this proof;
# it is carried by the row-decrease writer-race branch in run.sh instead.
diff_has_no_removed_rows() {
  _dr_db="$1"
  _dr_from="$2"
  _dr_to="$3"
  _dr_t="$4"
  # Without both commit endpoints there is nothing to diff against — fail closed.
  [ -n "$_dr_from" ] && [ -n "$_dr_to" ] && [ -n "$_dr_t" ] || return 1
  valid_table_name "$_dr_t" || return 1
  # Count rows dropped between the two commits. Zero means every row present at
  # <from> is still reachable at <to>, whatever else changed.
  if ! _dr_removed=$(query_single_cell "$_dr_db" \
    "preservation diff probe failed for table=$_dr_t" \
    "SELECT COUNT(*) FROM DOLT_DIFF('$_dr_from', '$_dr_to', '$_dr_t') WHERE diff_type = 'removed'"); then
    return 1
  fi
  case "$_dr_removed" in
    0) return 0 ;;             # no row dropped — this table's <from> rows preserved
    ''|*[!0-9]*) return 1 ;;   # empty/non-numeric probe result — fail closed
    *) return 1 ;;             # one or more removed rows — not preservable
  esac
}

# drift_preserves_preflight_rows <db> <from_head> <to_head> <space-separated tables>
# Returns 0 iff every listed table's <from>..<to> content diff drops no row.
# Returns non-zero (fail closed) if the table list is empty or any table fails
# diff_has_no_removed_rows.
drift_preserves_preflight_rows() {
  _dp_db="$1"
  _dp_from="$2"
  _dp_to="$3"
  _dp_tables="$4"
  _dp_seen=0
  for _dp_t in $_dp_tables; do
    _dp_seen=1
    diff_has_no_removed_rows "$_dp_db" "$_dp_from" "$_dp_to" "$_dp_t" || return 1
  done
  # An empty table list is not a proof of preservation.
  [ "$_dp_seen" = "1" ] || return 1
  return 0
}
