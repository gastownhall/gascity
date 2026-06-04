#!/bin/sh
# compact-gain-drift-proof.sh — Option A row-preservation proof for the
# post-flatten gain+drift case (gastownhall/gascity#2846).
#
# When verify_counts sees a table gain rows AND its value hash drift, the
# safety property at stake ("pre-flight rows remain reachable") cannot be
# inferred from HEAD movement alone: a concurrent writer whose commit is
# ABSORBED into the flatten commit moves no HEAD, so the HEAD-proven gate
# misses it and a benign race is hard-quarantined — which then blocks all
# future GC of a busy DB (the memory-exhaustion failure the code calls out).
#
# This proves preservation DIRECTLY: for each gained+drifted table, diff the
# pre-flight snapshot HEAD against the flatten commit. If the only change is
# `added` rows (no `removed`/`modified`), every pre-flight row survived and the
# gain is concurrent-writer data — defer, exactly as the HEAD-proven path does.
# It is strictly more rigorous than the HEAD proxy: it proves reachability
# instead of inferring it. Any removed/modified row, or any probe failure,
# fails closed and falls through to quarantine.
#
# Depends on `query_single_cell` and `valid_table_name` from run.sh.

# gain_drift_is_additive_only <db> <from_head> <to_head> <space-separated tables>
# Returns 0 iff every listed table's <from>..<to> content diff contains only
# `added` rows. Returns non-zero (fail closed) if either commit endpoint is
# missing, the table list is empty, a table name is invalid, a diff probe
# fails or returns a non-numeric result, or any table shows removed/modified
# rows.
gain_drift_is_additive_only() {
  _gd_db="$1"
  _gd_from="$2"
  _gd_to="$3"
  _gd_tables="$4"
  # Without both commit endpoints there is nothing to diff against — fail closed.
  [ -n "$_gd_from" ] && [ -n "$_gd_to" ] || return 1
  _gd_seen=0
  for _gd_t in $_gd_tables; do
    _gd_seen=1
    valid_table_name "$_gd_t" || return 1
    # Count rows that are NOT purely additive between the pre-flight snapshot
    # and the flatten commit. Zero means every pre-flight row is reachable
    # unchanged and only concurrent-writer rows were added.
    if ! _gd_nonadded=$(query_single_cell "$_gd_db" \
      "gain+drift preservation diff probe failed for table=$_gd_t" \
      "SELECT COUNT(*) FROM DOLT_DIFF('$_gd_from', '$_gd_to', '$_gd_t') WHERE diff_type <> 'added'"); then
      return 1
    fi
    case "$_gd_nonadded" in
      0) ;;                    # only added rows — this table's pre-flight rows preserved
      ''|*[!0-9]*) return 1 ;; # empty/non-numeric probe result — fail closed
      *) return 1 ;;           # one or more removed/modified rows — not preservable
    esac
  done
  # An empty table list is not a proof of preservation.
  [ "$_gd_seen" = "1" ] || return 1
  return 0
}
