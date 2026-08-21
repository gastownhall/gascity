#!/bin/sh
# compact-same-count-drift-proof.sh — in-place-modification proof for the
# post-flatten same-count table value-hash drift case (ga-ewo6j).
#
# verify_counts flags same_count_hash_drift when a table's value hash changes
# but its row count does NOT — an in-place row modification. On a high-write DB
# the beads/mail workload commits such UPDATEs (mail read/archive, bead
# close/status change) every few seconds, so one lands inside the flatten verify
# window on ~every run. Hard-quarantining it blocks ALL future GC of the DB and
# is the production memory-exhaustion failure the compactor header warns about.
#
# Unlike the gain+drift case, a same-count in-place modification cannot be proven
# safe by "additive only": the pre-flight row's value legitimately changed, so
# there is no unchanged-row anchor and no absorbed-writer DOLT_DIFF proof
# analogous to Option A (compact-gain-drift-proof.sh). This proof is therefore
# only ever a SECOND gate, applied by the caller AFTER a concurrent writer is
# HEAD-proven (writer_race_detected=1): it confirms the drift has the SHAPE of a
# benign in-place UPDATE — every same-count-drifted table's pre-flight..flatten
# content diff modifies rows and removes NONE of them. Any removed pre-flight
# row, a diff that fails to explain the drift, or a probe failure fails closed
# and falls through to quarantine.
#
# Depends on `query_single_cell` and `valid_table_name` from run.sh.

# same_count_drift_is_writer_explained <db> <from_head> <to_head> <tables>
# Returns 0 iff every listed table's <from>..<to> content diff removes zero rows
# AND modifies at least one row (the drift is an explained in-place UPDATE).
# Returns non-zero (fail closed) if either commit endpoint is missing, the table
# list is empty, a table name is invalid, a diff probe fails or returns a
# non-numeric result, any pre-flight row was removed, or no row was modified.
same_count_drift_is_writer_explained() {
  _sc_db="$1"
  _sc_from="$2"
  _sc_to="$3"
  _sc_tables="$4"
  # Without both commit endpoints there is nothing to diff against — fail closed.
  [ -n "$_sc_from" ] && [ -n "$_sc_to" ] || return 1
  _sc_seen=0
  for _sc_t in $_sc_tables; do
    _sc_seen=1
    valid_table_name "$_sc_t" || return 1
    # No pre-flight row may be removed. A removed row is data loss, not a benign
    # in-place UPDATE, regardless of the proven concurrent writer.
    if ! _sc_removed=$(query_single_cell "$_sc_db" \
      "same-count drift preservation diff probe failed for table=$_sc_t" \
      "SELECT COUNT(*) FROM DOLT_DIFF('$_sc_from', '$_sc_to', '$_sc_t') WHERE diff_type = 'removed'"); then
      return 1
    fi
    case "$_sc_removed" in
      0) ;;                    # no pre-flight row removed
      ''|*[!0-9]*) return 1 ;; # empty/non-numeric probe result — fail closed
      *) return 1 ;;           # one or more pre-flight rows removed — data loss
    esac
    # The drift must be explained by at least one modified row. A same-count hash
    # drift the diff cannot account for (zero modified rows) is unexplained and
    # fails closed.
    if ! _sc_modified=$(query_single_cell "$_sc_db" \
      "same-count drift modification diff probe failed for table=$_sc_t" \
      "SELECT COUNT(*) FROM DOLT_DIFF('$_sc_from', '$_sc_to', '$_sc_t') WHERE diff_type = 'modified'"); then
      return 1
    fi
    case "$_sc_modified" in
      ''|*[!0-9]*|0) return 1 ;; # empty/non-numeric/zero — drift unexplained
      *) ;;                       # at least one modified row explains the drift
    esac
  done
  # An empty table list is not a proof of preservation.
  [ "$_sc_seen" = "1" ] || return 1
  return 0
}
