#!/usr/bin/env bash
# Exec owns one atomic cursor/outbox, protected
# against concurrent manual runs as well as controller dispatch. A crash after
# nudge acceptance but before saving success may repeat a delivery.
set -euo pipefail

command -v jq >/dev/null || { echo "nudge-on-route: jq is required" >&2; exit 1; }
CITY="${GC_CITY:-.}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-${GC_CITY_RUNTIME_DIR:-$CITY/.gc/runtime}/packs/core}"
STATE_FILE="$PACK_STATE_DIR/nudge-on-route-delivery.json"
# An interrupted condition must schedule exec recovery on the next pass.
# This records an unfinished probe, not process liveness or lock ownership.
CHECK_PENDING="$PACK_STATE_DIR/nudge-on-route-check-pending"
NUDGE_MESSAGE="${GC_NUDGE_ON_ROUTE_MESSAGE:-Routed work is available. When idle, run gc hook --claim --json, execute the claimed work, and repeat until no work remains.}"
MODE="${1:-exec}"
case "$MODE" in exec|--check) ;; *) echo "usage: nudge-on-route.sh [--check]" >&2; exit 1 ;; esac

# Never leak the lock into gc's detached nudge poller or other descendants.
gc() { command gc "$@" 9>&-; }

if [ "$MODE" = exec ]; then
    mkdir -p "$PACK_STATE_DIR"
    if command -v flock >/dev/null 2>&1; then
        exec 9>"$PACK_STATE_DIR/nudge-on-route.lock"
        flock -n 9 || { echo "nudge-on-route: another delivery is running" >&2; exit 1; }
    elif command -v shlock >/dev/null 2>&1; then
        LOCK_FILE="$PACK_STATE_DIR/nudge-on-route.pid-lock"
        shlock -p $$ -f "$LOCK_FILE" || { echo "nudge-on-route: another delivery is running" >&2; exit 1; }
        trap 'rm -f "$LOCK_FILE"' EXIT
    else
        echo "nudge-on-route: flock or shlock is required" >&2
        exit 1
    fi
    rm -f "$CHECK_PENDING"
fi

STATE='{"version":1,"cursor":0,"pending":{},"notified":{},"retry":{},"observed":{}}'
if [ -f "$STATE_FILE" ]; then
    STATE="$(cat "$STATE_FILE")"
    if ! printf '%s\n' "$STATE" | jq -e '
        .version == 1 and (.cursor | type == "number" and . >= 0)
        and (.pending | type == "object") and (.notified | type == "object")
        and (.retry | type == "object")
        and ((.observed // {}) | type == "object")' >/dev/null; then
        [ "$MODE" != --check ] || exit 0
        echo "nudge-on-route: invalid delivery state: $STATE_FILE" >&2
        exit 1
    fi
elif [ -f "$PACK_STATE_DIR/nudge-on-route-state.json" ]; then
    # Import successes, never the old cursor: it could skip failed deliveries.
    if ! LEGACY="$(jq -ce 'to_entries | sort_by(.value) | reduce .[] as $e ({};
        ($e.key | split("|")) as $key | .[$key[0]] = $key[1])' \
        "$PACK_STATE_DIR/nudge-on-route-state.json")"; then
        [ "$MODE" != --check ] || exit 0
        echo "nudge-on-route: invalid legacy state; repair or move the file before retrying" >&2
        exit 1
    fi
    STATE="$(printf '%s\n%s\n' "$STATE" "$LEGACY" | jq -s '.[0].notified = .[1] | .[0]')"
fi

NOW="$(date +%s)"
LAST_SEQ="$(printf '%s\n' "$STATE" | jq -r .cursor)"
save_state() {
    local tmp
    tmp="$(mktemp "$PACK_STATE_DIR/.nudge-on-route.XXXXXX")"
    printf '%s\n' "$STATE" > "$tmp"
    mv -f "$tmp" "$STATE_FILE"
}
read_failed() {
    [ "$MODE" != --check ] || exit 0
    STATE="$(printf '%s\n' "$STATE" | jq --argjson now "$NOW" '
        ((.read_failures // 0) + 1) as $n | .read_failures = $n |
        .read_after = ($now + ([3600, (30 * pow(2; ([$n,7] | min)))] | min))')"
    save_state
    echo "nudge-on-route: incomplete read; retaining cursor and pending work for retry" >&2
    exit 1
}
has_due_delivery() {
    printf '%s\n' "$1" | jq -e --argjson now "$NOW" '
        . as $s | any(.pending | keys[]; ($s.retry[.].next_at // 0) <= $now)' >/dev/null
}
if [ "$MODE" = --check ]; then
    printf '%s\n' "$STATE" | jq -e --argjson now "$NOW" '(.read_after // 0) <= $now' >/dev/null || exit 1
    printf '%s\n' "$STATE" | jq -e '(.read_failures // 0) > 0' >/dev/null && exit 0
    has_due_delivery "$STATE" && exit 0
    [ -f "$STATE_FILE" ] || exit 0
    # The controller treats a timed-out condition as not due. Leave a recovery
    # intent before network I/O so repeated slow probes cannot starve exec.
    # Exec has the longer deadline and persists normal read-failure backoff.
    [ ! -f "$CHECK_PENDING" ] || exit 0
    mkdir -p "$PACK_STATE_DIR"
    : > "$CHECK_PENDING"
    trap 'exit 143' TERM INT HUP
    trap 'rc=$?; if [ "$rc" -lt 128 ]; then rm -f "$CHECK_PENDING"; fi' EXIT
fi

HEAD_SEQ="$(gc events --seq)" || read_failed
case "$HEAD_SEQ" in ''|*[!0-9]*) read_failed ;; esac
# The API can report zero on a failed head probe. Do not reset a valid cursor.
[ "$HEAD_SEQ" -ne 0 ] || [ "$LAST_SEQ" -eq 0 ] || read_failed

OPEN=''
read_open() {
    [ -n "$OPEN" ] && return 0
    # Federates HQ, rigs, graph bindings AND ephemeral storage. --status=open
    # includes blocked work so it can remain pending until it becomes ready.
    OPEN="$(gc ready --status=open --unassigned --metadata-field gc.routed_to --include-ephemeral --exclude-type=epic --json --limit=0)" || return 1
    printf '%s\n' "$OPEN" | jq -e 'type == "array"' >/dev/null
}
scan_routes() {
    read_open || return 1
    EVENTS="$(printf '%s\n' "$OPEN" | jq -c --argjson seq "$HEAD_SEQ" '.[] |
        {seq:$seq, type:"bead.updated", payload:.}')" || return 1
    # Only a complete authoritative snapshot may retire missing work.
    STATE="$(printf '%s\n%s\n' "$STATE" "$OPEN" | jq -s --argjson head "$HEAD_SEQ" '
        (.[1] | reduce .[] as $b ({}; .[$b.id] = true)) as $ids | .[0] | .cursor = $head |
        .notified |= with_entries(select($ids[.key] != null)) |
        .pending |= with_entries(select($ids[.key] != null)) |
        .retry |= with_entries(select($ids[.key] != null)) |
        .observed = ((.observed // {}) | with_entries(select($ids[.key] != null)))')"
}

if [ "$MODE" = --check ]; then
    [ "$HEAD_SEQ" -ne "$LAST_SEQ" ] || exit 1
    [ "$HEAD_SEQ" -ge "$LAST_SEQ" ] || exit 0
    [ "$LAST_SEQ" -gt 0 ] || exit 0
    # Advance irrelevant tails without continuously firing on our own tracking
    # events, and before every check has to replay an expensive history gap.
    [ $((HEAD_SEQ - LAST_SEQ)) -lt 500 ] || exit 0
fi

EVENTS=''
READ_TIMEOUT=20s
[ "$MODE" != --check ] || READ_TIMEOUT=2s
if [ "$LAST_SEQ" -eq 0 ] || [ "$HEAD_SEQ" -lt "$LAST_SEQ" ] || [ $((HEAD_SEQ - LAST_SEQ)) -gt 2000 ]; then
    scan_routes || read_failed
elif [ "$HEAD_SEQ" -gt "$LAST_SEQ" ]; then
    # Positive-sequence buffered watch returns the gap or an error. --after 0
    # starts at the current head; --since may return truncated pages with rc=0.
    if ! EVENTS="$(gc events --watch --after "$LAST_SEQ" --timeout "$READ_TIMEOUT")"; then
        [ "$MODE" != --check ] || exit 0
        scan_routes || read_failed
    elif ! printf '%s\n' "$EVENTS" | jq -se --argjson head "$HEAD_SEQ" --argjson last "$LAST_SEQ" '
        length > 0 and all(.[]; (.seq | type == "number"))
        and (map(.seq) | max) >= $head
        and (map(.seq) | unique | sort | .[0] == ($last + 1) and length == (.[-1] - $last))' >/dev/null; then
        # Missing sequence IDs are legal (the API omits undecodable rows).
        # They trigger a live snapshot, not a failed/retried history walk.
        [ "$MODE" != --check ] || exit 0
        scan_routes || read_failed
    fi
fi

# An incomplete create/update snapshot cannot prove that pending work became
# ineligible. Recover from the live ledger rather than deleting it on nulls.
if ! printf '%s\n' "$EVENTS" | jq -se '
    all(.[] | select(.type == "bead.created" or .type == "bead.updated");
        (.payload.bead // .payload) | (.id | type == "string" and length > 0)
        and (.status | type == "string" and length > 0))' >/dev/null; then
    [ "$MODE" != --check ] || exit 0
    scan_routes || read_failed
fi

# Event observations can lag the live route used for delivery. Track them
# separately: an observed transition invalidates an old success, but repeated
# stale snapshots must not erase a newer live-route notification.
if ! NEXT="$(printf '%s\n%s\n' "$STATE" "$EVENTS" | jq -s '
    .[0] as $state | .[1:] | sort_by(.seq) | reduce .[] as $e ($state;
        .cursor = ([.cursor, $e.seq] | max)
        | if ($e.type == "bead.created" or $e.type == "bead.updated" or $e.type == "bead.closed" or $e.type == "bead.deleted") then
            ($e | (.payload.bead // .payload)) as $b
            | ($b.id // $e.subject) as $id
            | ($b.metadata."gc.routed_to" // "") as $route
            | if ($id | type) != "string" or $id == "" then .
              elif $e.type == "bead.closed" or $e.type == "bead.deleted" or $b.status != "open"
                   or ($b.assignee // "") != "" or $route == "" then
                  del(.pending[$id], .notified[$id], .retry[$id], .observed[$id])
              else
                  (if .observed[$id] != $route and .notified[$id] != $route
                   then del(.notified[$id]) else . end)
                  | .observed[$id] = $route
                  | if .notified[$id] != $route then
                      (if .pending[$id] != $route or .retry[$id].reason == "blocked" then del(.retry[$id]) else . end)
                      | .pending[$id] = $route
                    else del(.pending[$id], .retry[$id]) end
              end
          else . end)
')"; then read_failed; fi

if [ "$MODE" = --check ]; then
    has_due_delivery "$NEXT" && exit 0
    printf '%s\n%s\n' "$STATE" "$NEXT" | jq -se '
        .[1].notified != .[0].notified or .[1].pending != .[0].pending' >/dev/null
    exit $?
fi

STATE="$(printf '%s\n' "$NEXT" | jq 'del(.read_after, .read_failures)')"
# Persist pending work WITH the cursor, before attempting delivery.
save_state

# Pool ownership comes from composed configuration, not current active count.
# An empty pool cannot resolve as a session; a pool's only live member may be
# busy while the controller starts a different slot for the routed work.
# The native idle-claim backstop owns pool-base reminders in every case.
# Named templates and explicit slots retain direct delivery.
AGENTS=''
nudge_routed_target() {
    _target="$1"
    if [ -z "$AGENTS" ]; then
        AGENTS="$(gc agent list --json)" || return 1
    fi
    printf '%s\n' "$AGENTS" | jq -e '(.agents | type == "array") and
        all(.agents[]; (.routes_to_pool | type == "boolean"))' >/dev/null || return 1
    if printf '%s\n' "$AGENTS" | jq -e --arg target "$_target" \
        'any(.agents[]; .qualified_name == $target and .routes_to_pool)' >/dev/null; then
        return 0
    fi
    _sessions="$(gc session list --json --state active --template "$_target")" || return 1
    _member_count="$(printf '%s\n' "$_sessions" | jq -er \
        'if (.sessions | type) == "array" then .sessions | length else error("missing sessions array") end')" || return 1
    if [ "$_member_count" -gt 1 ]; then
        return 0
    fi
    if [ "$_member_count" -eq 1 ]; then
        _members="$(printf '%s\n' "$_sessions" \
            | jq -er '.sessions[0] | .name // .id | select(type == "string" and length > 0)')" || return 1
        gc session nudge "$_members" "$NUDGE_MESSAGE"
        return $?
    fi
    gc session nudge "$_target" "$NUDGE_MESSAGE"
}

IDS="$(printf '%s\n' "$STATE" | jq -r --argjson now "$NOW" '
    . as $s | [.pending | keys[] | select(($s.retry[.].next_at // 0) <= $now)] | .[:20][]')"
[ -n "$IDS" ] || exit 0
read_open || read_failed
READY="$(gc ready --unassigned --metadata-field gc.routed_to --include-ephemeral \
    --exclude-type=epic --exclude-label hold:mayor --exclude-label hold:external --json --limit=0)" || read_failed
printf '%s\n' "$READY" | jq -e 'type == "array"' >/dev/null || read_failed
FAILED=0
defer_delivery() {
    STATE="$(printf '%s\n' "$STATE" | jq --arg id "$1" --arg reason "$2" --argjson now "$NOW" '
        ((.retry[$id].attempts // 0) + 1) as $n |
        .retry[$id] = {reason:$reason, attempts:$n, next_at:($now +
            (if $reason == "blocked" then 60 else ([3600, (30 * pow(2; ([$n,7] | min)))] | min) end))}')"
    save_state
}
while IFS= read -r id; do
    [ -n "$id" ] || continue
    printf '%s\n' "$STATE" | jq -e --arg id "$id" --argjson now "$NOW" \
        '(.retry[$id].next_at // 0) <= $now' >/dev/null || continue
    route="$(printf '%s\n' "$READY" | jq -r --arg id "$id" \
        'first(.[] | select(.id == $id and .status == "open" and (.assignee // "") == "") | .metadata."gc.routed_to") // ""')"
    if [ -z "$route" ]; then
        open_route="$(printf '%s\n' "$OPEN" | jq -r --arg id "$id" \
            'first(.[] | select(.id == $id) | .metadata."gc.routed_to") // ""')"
        if [ -n "$open_route" ]; then
            STATE="$(printf '%s\n' "$STATE" | jq --arg id "$id" --arg route "$open_route" \
                'if .notified[$id] != $route then del(.notified[$id]) else . end')"
            defer_delivery "$id" blocked
            continue
        fi
        STATE="$(printf '%s\n' "$STATE" | jq --arg id "$id" 'del(.pending[$id], .retry[$id], .notified[$id], .observed[$id])')"
    else
        # Each routing gets its own request. The queue may combine requests
        # into one turn, so the default message asks the worker to drain work.
        if ! printf '%s\n' "$STATE" | jq -e --arg id "$id" --arg route "$route" '.notified[$id] == $route' >/dev/null; then
            # A live reroute invalidates the old success before the new attempt.
            # A -> failed B -> A must notify A again, including after a crash.
            STATE="$(printf '%s\n' "$STATE" | jq --arg id "$id" 'del(.notified[$id])')"
            save_state
            if ! nudge_routed_target "$route"; then
                echo "nudge-on-route: failed notifying $route for $id; will retry" >&2
                FAILED=1
                defer_delivery "$id" delivery
                continue
            fi
        fi
        STATE="$(printf '%s\n' "$STATE" | jq --arg id "$id" --arg route "$route" \
            '.notified[$id] = $route | del(.pending[$id], .retry[$id])')"
    fi
    save_state
done <<IDS
$IDS
IDS
exit "$FAILED"
