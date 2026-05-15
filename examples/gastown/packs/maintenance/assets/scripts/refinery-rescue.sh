#!/usr/bin/env bash
# refinery-rescue — wake refineries whose merge queue has work but
# whose patrol wisp is absent.
#
# Symptom this catches: refinery agent processes its queue, decides
# "done", stops pouring next wisp. Later, polecats finish more work and
# route beads to the same refinery — but no in-progress patrol wisp
# exists, so the agent never wakes to process them. The merge queue
# stalls silently until external intervention.
#
# This order is the safety net. The primary fix is the refinery's own
# Patrol Lifecycle Discipline (see gastown/agents/refinery/prompt.template.md):
#   1. Always pour next wisp before burning current
#   2. Request restart on heavy context
# This script catches the case where (1) or (2) fail.
#
# Logic: for each rig with a default-named gastown refinery, count open
# beads routed to that refinery. If > 0 and no in-progress patrol wisp,
# nudge the refinery. Idempotent — re-nudging an already-awake agent
# is cheap (one tmux keystroke, no state change).
#
# Runs as an exec order (no LLM, no agent, no wisp).

set -euo pipefail

CITY="${GC_CITY:-.}"

# Enumerate every rig in this city. Each rig that imports the default
# gastown pack exposes a canonical refinery at <rig>/gastown.refinery.
# Rigs without a refinery return an empty queue and are silently skipped.
RIGS=$(gc --city "$CITY" rig list --json 2>/dev/null | jq -r '.rigs[]?.name // empty')
if [ -z "$RIGS" ]; then
    exit 0
fi

NUDGED=0
SCANNED=0
while IFS= read -r rig; do
    [ -z "$rig" ] && continue
    SCANNED=$((SCANNED + 1))
    refinery="${rig}/gastown.refinery"

    # Open beads queued to this refinery? Use gc.routed_to as the
    # canonical signal; an empty result here is the common path
    # (refinery has no work, nothing to do).
    queued=$(gc --city "$CITY" bd list --rig "$rig" --status=open \
        --metadata-field "gc.routed_to=${refinery}" \
        --json --limit=1 2>/dev/null || echo "[]")
    [ "$queued" = "[]" ] && continue

    # In-progress patrol wisp for this refinery? If yes, the refinery
    # is mid-cycle and will pick up the queued bead on its next step
    # or wake — no rescue needed.
    patrol=$(gc --city "$CITY" bd list --rig "$rig" --status=in_progress \
        --assignee "$refinery" \
        --label "molecule:mol-refinery-patrol" \
        --json --limit=1 2>/dev/null || echo "[]")
    [ "$patrol" != "[]" ] && continue

    # Gap: queued work + no in-progress patrol. Nudge the refinery to
    # wake and pour a patrol wisp. Failure to nudge (e.g. refinery
    # session doesn't exist yet) is non-fatal — the next cycle will
    # retry.
    count=$(printf '%s' "$queued" | jq 'length' 2>/dev/null || echo "?")
    if gc --city "$CITY" session nudge "$refinery" \
        "Rescue: ${count} bead(s) queued, no patrol wisp in flight. Pour mol-refinery-patrol and process the queue." \
        >/dev/null 2>&1; then
        NUDGED=$((NUDGED + 1))
    fi
done <<<"$RIGS"

if [ "$NUDGED" -gt 0 ]; then
    printf 'refinery-rescue: nudged %d refinery agent(s) across %d rig(s)\n' \
        "$NUDGED" "$SCANNED"
fi
