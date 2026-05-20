#!/usr/bin/env bash
# wisp-compact — TTL-based cleanup of expired ephemeral beads and aged read mail.
#
# Wisps are short-lived work items (heartbeats, pings, patrols) that
# accumulate and bloat the database. This script applies retention policy:
# - Closed wisps past TTL → deleted (Dolt AS OF preserves history)
# - Non-closed wisps past TTL → promoted to permanent (stuck detection)
# - Wisps with comments or "keep" label → promoted (proven value)
#
# TTL by wisp_type label:
#   heartbeat, ping: 6h
#   patrol, gc_report: 24h
#   recovery, error, escalation: 7d
#   default (untyped): 24h
#
# Pass 2 archives aged read mail messages (issue_type=message). Mail beads
# are not ephemeral — they live in the issues tier and are git-synced — but
# once the recipient has marked one "read" it has served its purpose, and
# without active cleanup the read backlog drives the open-bead count past
# the reaper alert threshold (see ga-l17).
#
# Rule (Pass 2): issue_type=message AND status=open AND label=read AND
# updated_at past TTL AND no 'keep' label AND comment_count=0 → archive
# (bd close with reason). The 'keep' label and any active discussion
# (comment_count>0) opt the message out, mirroring Pass 1's "proven value"
# escape hatches. TTL defaults to 24h; GC_MAIL_ARCHIVE_AGE_HOURS overrides.
#
# Runs as an exec order (no LLM, no agent, no wisp).
#
# Batched bd contract (ga-blt): a single jq pass classifies every bead;
# eligible IDs are collected into bash arrays and applied with one bd call
# per category (promote-proven, promote-stuck, delete, archive). This
# replaces the per-bead loop that spawned 5,000–32,000 bd subprocesses per
# cooldown and pegged dolt's thread pool (pe-t07v).
set -euo pipefail

CITY="${GC_CITY:-.}"

# Get all beads.
ALL=$(bd list --json --all -n 0 2>/dev/null) || exit 0

NOW=$(date +%s)
MAIL_ARCHIVE_AGE_H="${GC_MAIL_ARCHIVE_AGE_HOURS:-24}"
MAIL_ARCHIVE_AGE_S=$((MAIL_ARCHIVE_AGE_H * 3600))

# Single jq pass over all beads. Emits tab-separated "<action>\t<id>" lines
# for each actionable bead and "skip\t<id>" for ephemerals still within TTL
# (used only to maintain the SKIPPED counter). Date math is done inside jq
# via fromdateiso8601 (always-UTC), so we no longer call `date` per bead.
#
# Action values:
#   promote_proven   — closed past TTL with comments or 'keep' label
#   promote_stuck    — non-closed past TTL (stuck detection)
#   delete_wisp      — closed past TTL, no comments, no 'keep'
#   archive_mail     — Pass 2: read mail past mail-archive TTL
#   skip             — ephemeral within TTL (skipped counter only)
CLASSIFIED=$(echo "$ALL" | jq -r \
    --argjson now "$NOW" \
    --argjson mail_age "$MAIL_ARCHIVE_AGE_S" '
  def wisp_ttl(labels):
    if any(labels[]; . == "keep") then 0
    elif any(labels[]; . == "wisp_type:heartbeat" or . == "wisp_type:ping") then 6 * 3600
    elif any(labels[]; . == "wisp_type:patrol" or . == "wisp_type:gc_report") then 24 * 3600
    elif any(labels[]; . == "wisp_type:recovery" or . == "wisp_type:error" or . == "wisp_type:escalation") then 7 * 24 * 3600
    else 24 * 3600
    end;

  def parse_ts:
    # Try both RFC3339-with-Z (jq native) and no-Z (append Z and retry).
    (fromdateiso8601? // ((. + "Z") | fromdateiso8601?));

  .[]
  | (.labels // []) as $labels
  | (.updated_at // .created_at // "") as $ts_str
  | ($ts_str | parse_ts) as $ts
  | select($ts != null)
  | ($now - $ts) as $age
  | (.comment_count // 0) as $cc
  | (.status // "open") as $status

  | if .ephemeral == true then
      wisp_ttl($labels) as $ttl
      | if $ttl > 0 and $age < $ttl then
          "skip\t\(.id)"
        elif $cc > 0 or any($labels[]; . == "keep") or $status != "closed" then
          (if $status != "closed" then "promote_stuck\t\(.id)"
           else "promote_proven\t\(.id)" end)
        else
          "delete_wisp\t\(.id)"
        end
    elif .issue_type == "message" and $status == "open" then
      if any($labels[]; . == "read")
         and (any($labels[]; . == "keep") | not)
         and $cc == 0
         and $age >= $mail_age then
        "archive_mail\t\(.id)"
      else empty end
    else empty end
' 2>/dev/null) || CLASSIFIED=""

PROMOTE_PROVEN=()
PROMOTE_STUCK=()
DELETE_IDS=()
ARCHIVE_IDS=()
SKIPPED=0

while IFS=$'\t' read -r action id; do
    [ -z "$action" ] && continue
    case "$action" in
        skip) SKIPPED=$((SKIPPED + 1)) ;;
        promote_proven) PROMOTE_PROVEN+=("$id") ;;
        promote_stuck) PROMOTE_STUCK+=("$id") ;;
        delete_wisp) DELETE_IDS+=("$id") ;;
        archive_mail) ARCHIVE_IDS+=("$id") ;;
    esac
done <<< "$CLASSIFIED"

PROMOTED=0
DELETED=0
ARCHIVED=0

# Promotion: one bd update call per audit-reason group. --append-notes
# captures the reason on each promoted bead's notes field, replacing the
# old per-bead `bd comment` audit trail.
if [ ${#PROMOTE_PROVEN[@]} -gt 0 ]; then
    bd update "${PROMOTE_PROVEN[@]}" --persistent --append-notes "Promoted from wisp: proven value" 2>/dev/null || true
    PROMOTED=$((PROMOTED + ${#PROMOTE_PROVEN[@]}))
fi
if [ ${#PROMOTE_STUCK[@]} -gt 0 ]; then
    bd update "${PROMOTE_STUCK[@]}" --persistent --append-notes "Promoted from wisp: open past TTL (stuck detection)" 2>/dev/null || true
    PROMOTED=$((PROMOTED + ${#PROMOTE_STUCK[@]}))
fi
if [ ${#DELETE_IDS[@]} -gt 0 ]; then
    bd delete "${DELETE_IDS[@]}" --force 2>/dev/null || true
    DELETED=${#DELETE_IDS[@]}
fi
if [ ${#ARCHIVE_IDS[@]} -gt 0 ]; then
    # bd validation.on-close=error requires a reason >= 20 chars.
    bd close "${ARCHIVE_IDS[@]}" --reason "wisp-compact: archived aged read mail past TTL" 2>/dev/null || true
    ARCHIVED=${#ARCHIVE_IDS[@]}
fi

TOTAL=$((PROMOTED + DELETED + ARCHIVED))
if [ "$TOTAL" -gt 0 ]; then
    echo "wisp-compact: promoted=$PROMOTED deleted=$DELETED skipped=$SKIPPED archived=$ARCHIVED"
fi
