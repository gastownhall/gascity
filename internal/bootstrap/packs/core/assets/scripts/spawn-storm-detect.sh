#!/usr/bin/env bash
# spawn-storm-detect — find beads stuck in a recovery loop.
#
# Each sweep reads the claimable, unassigned beads of every store (gc ready
# --unassigned) and counts a bead each time it comes back to that pool
# carrying recovery or rejection metadata (metadata.recovered or
# metadata.rejection_reason). When any bead's count reaches the threshold,
# escalates via mail to the configured escalation target
# ($GC_ESCALATION_TARGET, default: the reserved "human" alias, which every
# city resolves).
#
# The ledger keeps each bead's count across runs. A second file keeps the
# updated_at each bead had when a sweep last found it in the pool, so a bead
# that waits there counts once, not once per sweep. Closed beads are pruned
# from both; a suspended rig's beads keep their counts, unread, until the
# rig resumes.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "spawn-storm-detect"

CITY="${GC_CITY:-.}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
LEDGER="$PACK_STATE_DIR/spawn-storm-counts.json"
THRESHOLD="${SPAWN_STORM_THRESHOLD:-2}"
# Where storm alerts go. Cities that staff a coordinator role (e.g. the gastown
# pack's mayor) can point this at it; "human" is the reserved recipient alias
# that resolves in every city, including core-only ones.
ESCALATION_TARGET="${GC_ESCALATION_TARGET:-human}"
# Count of undeliverable alerts. Load-bearing for the exit code below.
FAILED=0
# Set when the rig list could not be read and the ledger went unpruned.
# Load-bearing for the exit code below, like FAILED.
UNPRUNED=0

if [ ! -e "$LEDGER" ] && [ -e "$CITY/.gc/spawn-storm-counts.json" ]; then
    LEDGER="$CITY/.gc/spawn-storm-counts.json"
fi
# The ledger stays a plain {id: count} map, which every older copy of this
# script still reads; the timestamps live beside it.
SEEN_FILE="$(dirname "$LEDGER")/spawn-storm-seen.json"
mkdir -p "$(dirname "$LEDGER")"

# save_json FILE JSON writes JSON to FILE through a temp file and a rename, so
# a sweep killed mid-write leaves the previous copy, not a truncated file that
# every later sweep fails to parse.
save_json() {
    local tmp
    tmp=$(mktemp "$1.XXXXXX")
    printf '%s\n' "$2" > "$tmp"
    mv -f "$tmp" "$1"
}

# Initialize ledger if missing.
if [ ! -f "$LEDGER" ]; then
    echo '{}' > "$LEDGER"
fi

# Step 1: Read the pool, the beads a worker can claim. A recovery resets a
# bead to open and unassigned, which puts it back there.
# Read through gc ready, not gc bd list. gc bd list answers from one store (the
# city store, for this city-scoped order) and bd leaves the wisp plane out of a
# plain list, so a bead reset in a rig store, in the graph store a split city
# keeps workflow steps in, or on the wisp plane was never counted. gc ready
# reads the city store, every rig store that is not suspended and every
# [storage.classes] binding, across both storage tiers, and fails on a store it
# cannot read. So does this sweep, before it touches the ledger: an exit 0
# would read as "no storms", and the controller logs an exec order's output
# only on a non-zero exit. It reads ready work, not --status open, which adds
# beads no worker can claim (deferred, blocked, or parked in a custom status)
# and so are not in the pool. --unassigned keeps beads being worked out of the
# count.
READ_ERR=$(mktemp)
trap 'rm -f "$READ_ERR"' EXIT
READ_RC=0
OPEN_BEADS=$(gc ready --unassigned --json 2>"$READ_ERR") || READ_RC=$?
if [ "$READ_RC" -ne 0 ]; then
    WHY=$(<"$READ_ERR")
    echo "spawn-storm-detect: gc ready --unassigned failed (exit $READ_RC): ${WHY//$'\n'/ }" >&2
    exit 1
fi
if ! jq -e 'type == "array"' <<<"$OPEN_BEADS" >/dev/null 2>&1; then
    echo "spawn-storm-detect: gc ready --unassigned did not answer a JSON array: ${OPEN_BEADS:0:200}" >&2
    exit 1
fi

# Step 2: Load the ledger, and the updated_at each bead had when a sweep last
# found it in the pool.
COUNTS=$(cat "$LEDGER")
SEEN='{}'
if [ -f "$SEEN_FILE" ]; then
    SEEN=$(cat "$SEEN_FILE")
fi

# Step 3: Count the beads that came back to the pool with rejection metadata
# (returned for rework, or recovered by a work-health patrol) since the last
# sweep. An empty string is not a rejection: jq reads "" as not null, so a key
# left set to "" would count on every sweep.
#
# A bead counts when its updated_at differs from the one it had the last time a
# sweep found it in the pool, or no sweep has found it there yet. Every reset is
# a write, so a bead claimed and reset again between two sweeps counts. A bead
# that waits in the pool does not, nor does one that leaves it and comes back
# unwritten (a blocker closing, its rig suspended and resumed). A row with no
# updated_at, which a bd store leaves off many rows, counts when the last sweep
# that could read it did not find it in the pool (Step 5). A ledger count kept
# from before the timestamps existed reads as not found.
#
# Pairs of JSON documents go to jq on stdin, never as --argjson: one argument
# is capped (128KiB on Linux), and the pool and the ledger can be larger.
STORMS=0
RESET=$(jq -c '[.[] | select((.metadata.rejection_reason // "") != "" or (.metadata.recovered // "") != "") | {key: .id, value: (.updated_at // "")}] | from_entries' <<<"$OPEN_BEADS")
RESET_IDS=$(printf '%s\n%s\n' "$SEEN" "$RESET" | jq -r -n 'input as $seen | input | to_entries[] | select($seen[.key] != .value) | .key')
SEEN_NEXT=$(printf '%s\n%s\n' "$SEEN" "$RESET" | jq -c -n 'input as $seen | input as $reset | $seen + $reset')
while IFS= read -r bead_id; do
    [ -z "$bead_id" ] && continue

    # Increment count for this bead.
    PREV=$(echo "$COUNTS" | jq -r --arg id "$bead_id" '.[$id] // 0')
    NEW=$((PREV + 1))
    COUNTS=$(echo "$COUNTS" | jq --arg id "$bead_id" --argjson n "$NEW" '.[$id] = $n')

    # Edge-triggered (-eq, not -ge): mail once when the count CROSSES the
    # threshold, not on every 5m sweep for as long as the storm persists.
    # A new storm on the same bead re-alerts because the ledger prunes the
    # count once the bead closes. --dedup guards the repeat-crossing case
    # (ledger reset while the previous mail is still live in the inbox).
    if [ "$NEW" -eq "$THRESHOLD" ]; then
        TITLE_JSON=$(gc bd show "$bead_id" --json 2>/dev/null || true)
        TITLE=$(echo "$TITLE_JSON" | jq -r 'if type == "array" then (.[0].title // "unknown") else "unknown" end' 2>/dev/null || echo "unknown")
        if ! gc mail send "$ESCALATION_TARGET" \
            -s "SPAWN_STORM: bead $bead_id reset ${NEW}x" \
            -m "Bead $bead_id ($TITLE) has been reset to pool $NEW times (threshold: $THRESHOLD).
This likely indicates a polecat crash loop on this specific work.

Recommended actions:
- Inspect the bead: gc bd show $bead_id --json
- Check rejection history: metadata.rejection_reason
- Consider quarantining the bead or investigating the root cause." \
            --dedup "spawn-storm:$bead_id" \
            2>/dev/null; then
            # Do not swallow an undeliverable alert — a vanished storm alert
            # is invisible. Surfacing it requires a non-zero exit (see below),
            # so record the failure and keep sweeping the remaining beads.
            echo "spawn-storm-detect: could not mail escalation target '$ESCALATION_TARGET' about $bead_id" >&2
            FAILED=$((FAILED + 1))
            # Roll this bead's count back so the next sweep crosses the
            # threshold again. The ledger is saved either way, and with an edge
            # trigger a count left AT the threshold never equals it again, so a
            # transient mail failure would lose the alert outright. --dedup
            # covers the other half: if the mail did land and only the report
            # failed, the retry is suppressed instead of duplicated. Forget its
            # timestamp too: a bead still waiting in the pool counts only when
            # no timestamp matches, and the retry has to count it again.
            COUNTS=$(echo "$COUNTS" | jq --arg id "$bead_id" --argjson n "$PREV" '.[$id] = $n')
            SEEN_NEXT=$(jq -c --arg id "$bead_id" 'del(.[$id])' <<<"$SEEN_NEXT")
        fi
        STORMS=$((STORMS + 1))
    fi
done <<< "$RESET_IDS"

# Step 4: Prune closed beads from ledger.
# Only check beads actually tracked in the ledger (avoids expensive full scan
# of all closed beads via gc bd list --status=closed --limit=0), and not the
# ones this sweep found in the pool, which are open.
TRACKED_IDS=$(printf '%s\n%s\n' "$RESET" "$COUNTS" | jq -c -n 'input as $reset | input | keys - ($reset | keys)')
# The tracked beads this sweep leaves unread. They keep their counts, and Step
# 5 keeps their records.
UNREAD_IDS='[]'
if [ "$TRACKED_IDS" != "[]" ]; then
    # A suspended rig is left cold: any bd read restarts its retired proxy, so
    # its beads stay unread until it resumes. A bead belongs to the HQ or the
    # rig whose prefix, followed by "-", is the longest its id starts with,
    # ignoring case: the rule gc bd routes an id by (sling.BeadPrefixForCity).
    # A prefix can hold a hyphen, so agent-diagnostics-hnn belongs to rig
    # agent-diagnostics, not to rig agent.
    # gc rig list is a separate read that can fail where gc ready did not (it
    # builds a session provider). If it fails, no tracked bead is read and
    # nothing is pruned this sweep, and the exit below reports it: a suspended
    # set read as empty would wake every cold rig the ledger tracks.
    RIGS_RC=0
    RIGS=$(gc rig list --json 2>"$READ_ERR") || RIGS_RC=$?
    if [ "$RIGS_RC" -ne 0 ]; then
        WHY=$(<"$READ_ERR")
        echo "spawn-storm-detect: gc rig list --json failed (exit $RIGS_RC), so the ledger was not pruned: ${WHY//$'\n'/ }" >&2
        UNPRUNED=1
        UNREAD_IDS="$TRACKED_IDS"
    elif ! jq -e '(.rigs | type) == "array" and all(.rigs[]; (.prefix | type) == "string")' <<<"$RIGS" >/dev/null 2>&1; then
        echo "spawn-storm-detect: gc rig list --json did not answer a rig list, so the ledger was not pruned: ${RIGS:0:200}" >&2
        UNPRUNED=1
        UNREAD_IDS="$TRACKED_IDS"
    else
        UNREAD_IDS=$(printf '%s\n%s\n' "$RIGS" "$TRACKED_IDS" | jq -c -n '
            [input.rigs[] | select(.prefix != "") | {p: ((.prefix | ascii_downcase) + "-"), cold: (.hq != true and .suspended == true)}] as $owners
            | [input[] as $id
               | (reduce $owners[] as $o (null;
                   if ($id | ascii_downcase | startswith($o.p)) and ($o.p | length) > ((.p // "") | length) then $o else . end)) as $owner
               | select($owner.cold == true)
               | $id]')
    fi
fi
PRUNE_IDS=$(printf '%s\n%s\n' "$TRACKED_IDS" "$UNREAD_IDS" | jq -r -n 'input as $tracked | input as $unread | ($tracked - $unread)[]')
while IFS= read -r tid; do
    [ -z "$tid" ] && continue
    # stdout only: gc writes diagnostics to stderr (scope disclosure, doctor
    # warnings), and merging them into the JSON makes every jq parse fail and
    # every bead read as "unknown", so nothing is ever pruned. The error path
    # re-reads with stderr merged, since that is where the not-found text is.
    if BEAD_OUTPUT=$(gc bd show "$tid" --json 2>/dev/null); then
        BEAD_STATUS=$(echo "$BEAD_OUTPUT" | jq -r 'if type == "array" then (.[0].status // "deleted") elif type == "object" and ((.error // "") | test("not found|no issue found"; "i")) then "deleted" else "unknown" end' 2>/dev/null || echo "unknown")
    elif BEAD_OUTPUT=$(gc bd show "$tid" --json 2>&1 || true); echo "$BEAD_OUTPUT" | grep -qiE 'not found|no issue found'; then
        BEAD_STATUS="deleted"
    else
        BEAD_STATUS="unknown"
    fi
    if [ "$BEAD_STATUS" = "closed" ] || [ "$BEAD_STATUS" = "deleted" ]; then
        COUNTS=$(echo "$COUNTS" | jq --arg id "$tid" 'del(.[$id])' 2>/dev/null) || true
    fi
done <<< "$PRUNE_IDS"

# Step 5: Save the ledger, then the records of the beads it still tracks. A
# timestamp is kept until its bead leaves the ledger. A record with no
# timestamp says only that the last sweep found the bead in the pool, so it
# lasts while sweeps keep finding it there or leave it unread. The ledger goes
# first: a sweep that dies between the two writes counts this sweep's comebacks
# again rather than losing them.
SEEN_NEXT=$(printf '%s\n%s\n%s\n%s\n' "$COUNTS" "$RESET" "$UNREAD_IDS" "$SEEN_NEXT" | jq -c -n '
    input as $counts | input as $reset | input as $unread
    | input
    | with_entries(select(.key as $id
        | ($counts | has($id)) and (.value != "" or ($reset | has($id)) or any($unread[]; . == $id))))')
save_json "$LEDGER" "$COUNTS"
save_json "$SEEN_FILE" "$SEEN_NEXT"

if [ "$STORMS" -gt 0 ]; then
    echo "spawn-storm-detect: found $STORMS beads exceeding reset threshold"
fi

# Loud-fail: the ledger has been written above, so a non-zero exit now surfaces
# the failure lines above (an undeliverable alert, an unread rig list) to the
# controller log without losing the recorded counts. The controller captures an
# exec order's combined output but logs it only on a non-zero exit
# (order_dispatch.go), so exit 0 would swallow them (gastownhall/gascity#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "spawn-storm-detect: $FAILED storm alert(s) could not be delivered to '$ESCALATION_TARGET' (see above)" >&2
    exit 1
fi
if [ "$UNPRUNED" -ne 0 ]; then
    exit 1
fi
