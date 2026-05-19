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
set -euo pipefail

CITY="${GC_CITY:-.}"

# Get all beads.
ALL=$(bd list --json --all -n 0 2>/dev/null) || exit 0
EPHEMERALS=$(echo "$ALL" | jq '[.[] | select(.ephemeral == true)]' 2>/dev/null) || EPHEMERALS="[]"

NOW=$(date +%s)
PROMOTED=0
DELETED=0
SKIPPED=0

# Pass 1 (ephemerals): apply wisp_type TTL retention.
#
# Capturing jq output into BEADS first (instead of piping into the loop)
# preserves the original pipefail fail-loud on jq error AND keeps
# PROMOTED/DELETED/SKIPPED in the parent shell so they survive to the
# summary echo below.
if [ -n "$EPHEMERALS" ] && [ "$EPHEMERALS" != "[]" ]; then
BEADS=$(echo "$EPHEMERALS" | jq -c '.[]' 2>/dev/null)
while IFS= read -r bead; do
    id=$(echo "$bead" | jq -r '.id')
    status=$(echo "$bead" | jq -r '.status')
    updated_at=$(echo "$bead" | jq -r '.updated_at // .created_at')
    comment_count=$(echo "$bead" | jq -r '.comment_count // 0')
    labels=$(echo "$bead" | jq -r '.labels // [] | .[]' 2>/dev/null)

    # Determine TTL from wisp_type label.
    TTL_SECONDS=$((24 * 3600))  # default: 24h
    for label in $labels; do
        case "$label" in
            wisp_type:heartbeat|wisp_type:ping) TTL_SECONDS=$((6 * 3600)) ;;
            wisp_type:patrol|wisp_type:gc_report) TTL_SECONDS=$((24 * 3600)) ;;
            wisp_type:recovery|wisp_type:error|wisp_type:escalation) TTL_SECONDS=$((7 * 24 * 3600)) ;;
            keep) TTL_SECONDS=0 ;;  # force promote
        esac
    done

    # Calculate age. bd emits RFC3339 timestamps with a trailing 'Z'; the
    # second BSD `date -ju -f` fallback handles that explicitly and forces
    # UTC semantics to match GNU `date -d`. The third layout supports older
    # no-Z timestamps without interpreting them in the local timezone.
    BEAD_TS=$(date -d "$updated_at" +%s 2>/dev/null || \
              date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$updated_at" +%s 2>/dev/null || \
              date -ju -f "%Y-%m-%dT%H:%M:%S" "$updated_at" +%s 2>/dev/null) || continue
    AGE=$((NOW - BEAD_TS))

    # Skip if within TTL (unless force-promote via keep label).
    if [ "$TTL_SECONDS" -gt 0 ] && [ "$AGE" -lt "$TTL_SECONDS" ]; then
        SKIPPED=$((SKIPPED + 1))
        continue
    fi

    # Promote if has comments, keep label, or non-closed.
    if [ "$comment_count" -gt 0 ] || echo "$labels" | grep -q '^keep$' || [ "$status" != "closed" ]; then
        REASON="proven value"
        [ "$status" != "closed" ] && REASON="open past TTL (stuck detection)"
        bd update "$id" --persistent 2>/dev/null || true
        bd comment "$id" "Promoted from wisp: $REASON" 2>/dev/null || true
        PROMOTED=$((PROMOTED + 1))
        continue
    fi

    # Closed + past TTL + no special attributes → delete.
    bd delete "$id" --force 2>/dev/null || true
    DELETED=$((DELETED + 1))
done <<< "$BEADS"
fi

# Pass 2: archive aged read mail messages.
#
# Mail messages aren't ephemeral wisps, so they bypass Pass 1's
# ephemeral-only filter. They need their own retention rule, keyed off the
# 'read' label (set by beadmail.Read / MarkRead when a recipient consumes
# the message). Unread mail is never archived here — only the recipient's
# explicit acknowledgement triggers the TTL countdown.
MAIL_ARCHIVE_AGE_H="${GC_MAIL_ARCHIVE_AGE_HOURS:-24}"
ARCHIVED=0

MESSAGES=$(echo "$ALL" | jq -c '.[] | select(.issue_type == "message" and .status == "open")' 2>/dev/null) || MESSAGES=""

if [ -n "$MESSAGES" ]; then
    while IFS= read -r msg; do
        [ -z "$msg" ] && continue
        id=$(echo "$msg" | jq -r '.id')
        updated_at=$(echo "$msg" | jq -r '.updated_at // .created_at')
        comment_count=$(echo "$msg" | jq -r '.comment_count // 0')
        labels=$(echo "$msg" | jq -r '.labels // [] | .[]' 2>/dev/null)

        # Skip messages the recipient hasn't consumed yet.
        if ! echo "$labels" | grep -q '^read$'; then
            continue
        fi

        # 'keep' overrides the auto-archive, same opt-out as Pass 1.
        if echo "$labels" | grep -q '^keep$'; then
            continue
        fi

        # Active discussion = proven value, leave it alone.
        if [ "$comment_count" -gt 0 ]; then
            continue
        fi

        # Same date fallback chain as Pass 1 (GNU → BSD with Z → BSD without Z).
        MSG_TS=$(date -d "$updated_at" +%s 2>/dev/null || \
                 date -ju -f "%Y-%m-%dT%H:%M:%SZ" "$updated_at" +%s 2>/dev/null || \
                 date -ju -f "%Y-%m-%dT%H:%M:%S" "$updated_at" +%s 2>/dev/null) || continue
        AGE_H=$(( (NOW - MSG_TS) / 3600 ))

        if [ "$AGE_H" -lt "$MAIL_ARCHIVE_AGE_H" ]; then
            continue
        fi

        # bd validation.on-close=error requires a reason >= 20 chars.
        bd close "$id" --reason "wisp-compact: archived aged read mail past TTL" 2>/dev/null || true
        ARCHIVED=$((ARCHIVED + 1))
    done <<< "$MESSAGES"
fi

TOTAL=$((PROMOTED + DELETED + ARCHIVED))
if [ "$TOTAL" -gt 0 ]; then
    echo "wisp-compact: promoted=$PROMOTED deleted=$DELETED skipped=$SKIPPED archived=$ARCHIVED"
fi
