#!/usr/bin/env bash
# dead-run-detect — escalate in-flight workflow runs whose driving session died.
#
# A formulas-v2 (graph.v2) run is driven by the session that claimed its
# workflow root (the run-operator). When that session dies between steps —
# after a decomposition step closed and before the drain was routed — nothing
# in the city notices: the root stays in_progress, the step beads stay open and
# unclaimed, and the run reports as active indefinitely. Every existing sweep
# misses this shape:
#   - reaper.sh's stale-root close needs an EMPTY assignee, >24h of silence and
#     NO descendant in a live status ('open' is live), so open step beads make
#     the root permanently ineligible — and that path closes silently anyway.
#   - orphan-sweep.sh resets in_progress beads whose assignee is unknown; a
#     root assigned to a CONFIGURED agent name counts as known even when no
#     session for it is running.
#   - the dashboard's run staleness (internal/runproj/enrich.go) is display-only.
#   - the execution / idle-claim backstops (cmd/gc/execution_backstop.go,
#     cmd/gc/idle_nudge.go) act on pool-slot trigger beads, not workflow roots.
#
# Runs as a cooldown sweep. Each run:
#   1. Collects the live session identities from `gc session list --json` for
#      HQ and every non-HQ rig (the liveness source orphan-sweep.sh uses). If
#      any scope's session list fails, liveness is unverifiable and the whole
#      sweep is skipped — never alert on a partial picture.
#   2. Enumerates in_progress and open beads per scope via `gc bd list`.
#   3. Selects workflow ROOTS: status in_progress AND
#      (metadata gc.kind == "workflow" OR gc.formula_contract == "graph.v2")
#      AND gc.root_bead_id empty or self (sourceworkflow.IsWorkflowRoot).
#   4. Resolves each root's DRIVING SESSION from the fields the run projection
#      reads (internal/runproj/detail_sessionlink.go): metadata session_name /
#      gc.session_name / gc.sessionName / session_id / gc.session_id /
#      gc.sessionId, then the assignee, then gc.routed_to. The reconciler
#      back-fills gc.session_name onto the root from its worked steps
#      (cmd/gc/build_desired_state.go stampRunRootFromStep).
#   5. A root is DEAD when none of those identities matches a live session
#      (exact id / session_name / alias / agent_name / name / template, or the
#      rig-stripped and pool-slot-stripped forms orphan-sweep.sh accepts), it
#      has at least one open UNCLAIMED step (gc.root_bead_id == root, status
#      open, empty assignee), no in_progress step held by a live session, and
#      the run has been silent — no step bead created or updated — for longer
#      than GC_DEAD_RUN_THRESHOLD.
#   6. Escalates ONCE per root by mail to GC_DEAD_RUN_RECIPIENT (default
#      mayor), falling back to GC_ESCALATION_RECIPIENT (default human) when the
#      primary address is undeliverable. The mail carries the recovery recipe.
#
# Dedup marker: metadata gc.dead_run_escalated_at=<ISO timestamp> on the root,
# written only after a delivered send. A marked root is never re-mailed; the
# marker is removed when the condition clears (driver alive again, steps
# claimed, or the run progressed), so a recurrence re-alerts. Nothing else on
# any work bead is mutated — no closes, no resets, no assignee changes.
#
# Loud-fail (gastownhall/gascity#4543): an undeliverable escalation surfaces on
# stderr, is NOT marked, and the script exits non-zero so the controller logs
# it; the next sweep retries.
#
# Runs as an exec order (no LLM, no agent, no wisp).
set -euo pipefail

# Trace bd invocations to $GC_BD_TRACE when set (no-op otherwise).
__SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$__SCRIPT_DIR/_bd_trace.sh" "dead-run-detect"

# jq is a hard dependency: it decodes the session list and every bead record.
# Without it nothing could be evaluated. Fail loud.
if ! command -v jq >/dev/null 2>&1; then
    echo "dead-run-detect: jq is required but not found in PATH" >&2
    exit 1
fi

# A root must have been silent (no step bead created or updated) at least this
# long, with its driver gone, before it is escalated. Debounces the window in
# which a session is being replaced or a step is between hand-offs.
THRESHOLD="${GC_DEAD_RUN_THRESHOLD:-2h}"
# Primary escalation address. The mayor coordinates runs, so it is the address
# that can act on the recovery recipe.
RECIPIENT="${GC_DEAD_RUN_RECIPIENT:-mayor}"
# Fallback when the primary address is undeliverable (a city with no mayor).
# escalate.sh and the human-gate notify orders use the same default.
FALLBACK_RECIPIENT="${GC_ESCALATION_RECIPIENT:-human}"
# Re-entry formula named in the recovery recipe.
REENTRY_FORMULA="${GC_DEAD_RUN_REENTRY_FORMULA:-build-from-convoy}"
# Dedup marker written on the root after a delivered escalation.
MARKER_KEY="gc.dead_run_escalated_at"

# Convert a simple Go-style duration (Ns/Nm/Nh/Nd) to whole seconds.
duration_to_seconds() {
    case "$1" in
        *d) echo $(( ${1%d} * 86400 )) ;;
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}

THRESHOLD_S="$(duration_to_seconds "$THRESHOLD")"
NOW_EPOCH="$(date -u +%s)"
NOW_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

WORK_DIR="$(mktemp -d)" || exit 0
trap 'rm -rf "$WORK_DIR"' EXIT
SESSIONS_FILE="$WORK_DIR/sessions.jsonl"
BEADS_FILE="$WORK_DIR/beads.jsonl"
SCOPES_FILE="$WORK_DIR/scopes"
: > "$SESSIONS_FILE"
: > "$BEADS_FILE"

# Scopes to sweep: HQ (empty scope, bare gc) plus every non-HQ rig. `gc bd
# list` and `gc session list` without --rig are HQ-scoped from the city cwd,
# so per-rig beads and sessions are invisible to a bare query — walk each rig
# explicitly. The HQ entry is excluded (gc rig list reports the city root as an
# hq=true pseudo-rig that `gc --rig <cityName>` cannot resolve), matching the
# sibling scripts' cross-rig convention.
printf '\n' > "$SCOPES_FILE"
RIGS_JSON="$(gc rig list --json 2>/dev/null || true)"
if [ -n "$RIGS_JSON" ]; then
    printf '%s' "$RIGS_JSON" \
        | jq -r '(.rigs // [])[] | select(.hq != true) | .name' 2>/dev/null \
        >> "$SCOPES_FILE" || true
fi
RIG_COUNT=$(( $(grep -c . "$SCOPES_FILE" || true) ))

# Step 1: live session identities across every scope. A session-list failure
# in ANY scope makes liveness unverifiable city-wide (a root's driver may live
# in a scope other than the root's), so the sweep stops before it can produce
# a false "dead" verdict. Skipping is silent-by-design like orphan-sweep's
# `|| exit 0`; nothing is mutated on this path.
while IFS= read -r scope; do
    if [ -n "$scope" ]; then
        gc --rig "$scope" session list --json >"$WORK_DIR/sessions.raw" 2>/dev/null || {
            echo "dead-run-detect: session list failed for rig $scope; liveness unverifiable, skipping sweep" >&2
            exit 0
        }
    else
        gc session list --json >"$WORK_DIR/sessions.raw" 2>/dev/null || {
            echo "dead-run-detect: HQ session list failed; liveness unverifiable, skipping sweep" >&2
            exit 0
        }
    fi
    cat "$WORK_DIR/sessions.raw" >> "$SESSIONS_FILE"
done < "$SCOPES_FILE"

# Every identity a non-closed session row exposes, plus the rig-stripped short
# form of each, as one JSON array. The JSON shape is
# {"sessions":[...], "summary":..., "filters":..., "schema_version":...} with
# snake_case fields; PascalCase is accepted as forward-compatible hardening so
# a casing change cannot empty the live set and turn every run "dead"
# (orphan-sweep.sh carries the same guard).
LIVE_JSON="$(jq -c -s '
    def pick($snake; $pascal):
      if has($snake) and .[$snake] != null then .[$snake]
      elif has($pascal) and .[$pascal] != null then .[$pascal]
      else null end;
    [ .[] | .sessions[]?
      | select(
          (pick("closed"; "Closed") // false) == false
          and (((pick("state"; "State") // "") | tostring | ascii_downcase) != "closed")
        )
      | pick("id"; "ID"), pick("session_name"; "SessionName"), pick("alias"; "Alias"),
        pick("agent_name"; "AgentName"), pick("template"; "Template"), pick("name"; "Name")
    ]
    | map(select(. != null and . != "") | tostring)
    | map(., (split("/") | last))
    | unique
' "$SESSIONS_FILE" 2>/dev/null)" || exit 0
[ -n "$LIVE_JSON" ] || LIVE_JSON='[]'

# Step 2: in_progress and open beads per scope, annotated with their scope so
# a root's marker write can be routed back to the store that owns it. A scope
# is staged and committed only when BOTH lists succeed and parse; a scope with
# a failed list is dropped whole (its roots are simply not evaluated this
# sweep), so a root can never be judged against a partial view of its steps.
# Nothing is mutated for beads that were not seen.
list_beads() {
    local scope="$1"
    local status="$2"
    if [ -n "$scope" ]; then
        gc bd list --rig "$scope" --status="$status" --json --limit=0 2>/dev/null
    else
        gc bd list --status="$status" --json --limit=0 2>/dev/null
    fi
}

SCOPE_STAGE="$WORK_DIR/scope.jsonl"
while IFS= read -r scope; do
    : > "$SCOPE_STAGE"
    for status in in_progress open; do
        LISTED="$(list_beads "$scope" "$status")" || {
            echo "dead-run-detect: gc bd list --status=$status failed for scope '${scope:-hq}'; skipping scope" >&2
            continue 2
        }
        [ -n "$LISTED" ] || continue
        printf '%s' "$LISTED" \
            | jq -c --arg scope "$scope" '(if type == "array" then . else [] end)[] | {scope: $scope, bead: .}' \
            >> "$SCOPE_STAGE" 2>/dev/null || {
            echo "dead-run-detect: unparseable gc bd list output for scope '${scope:-hq}'; skipping scope" >&2
            continue 2
        }
    done
    cat "$SCOPE_STAGE" >> "$BEADS_FILE"
done < "$SCOPES_FILE"

[ -s "$BEADS_FILE" ] || exit 0

# Step 3: workflow roots. Mirrors sourceworkflow.IsWorkflowRoot (gc.kind ==
# workflow OR gc.formula_contract == graph.v2) plus the reaper's root test
# (gc.root_bead_id empty or self), so graph.v2-only roots are not missed.
ROOTS="$(jq -r '
    select(.bead.status == "in_progress")
    | .bead as $b
    | ($b.metadata // {}) as $m
    | select(
        (($m["gc.kind"] // "") | tostring | ascii_downcase) == "workflow"
        or (($m["gc.formula_contract"] // "") | tostring | ascii_downcase) == "graph.v2"
      )
    | select((($m["gc.root_bead_id"] // "") | tostring) as $r | $r == "" or $r == $b.id)
    | [$b.id, .scope] | @tsv
' "$BEADS_FILE" 2>/dev/null)" || exit 0
[ -n "$ROOTS" ] || exit 0

# Step 4: per-root verdict. One jq pass over the collected beads yields the
# driver identities, which of them are live, the unclaimed open steps, the
# in_progress steps held by live sessions, the last step activity, and the
# marker. Timestamps are parsed in jq (portable across GNU and BSD date);
# fractional seconds are dropped and numeric UTC offsets folded in.
root_verdict() {
    local scope="$1"
    local id="$2"
    jq -c -s --arg scope "$scope" --arg id "$id" --arg marker "$MARKER_KEY" \
        --argjson live "$LIVE_JSON" --argjson now "$NOW_EPOCH" '
        def short: if type == "string" and contains("/") then (split("/") | last) else . end;
        def slotbase: if type == "string" then sub("-[0-9]+$"; "") else . end;
        def islive($ids):
          (tostring) as $s
          | any($ids[]; . == $s or . == ($s | short) or . == ($s | slotbase) or . == ($s | short | slotbase));
        def epoch:
          if . == null or . == "" then null else
            (tostring | sub("\\.[0-9]+"; "")) as $s
            | if ($s | endswith("Z")) then (try ($s | fromdateiso8601) catch null)
              else ($s | capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<sign>[+-])(?<hh>[0-9]{2}):(?<mm>[0-9]{2})$") // null) as $c
                | if $c == null then null
                  else (try (($c.base + "Z") | fromdateiso8601) catch null) as $b
                    | if $b == null then null
                      else $b - (if $c.sign == "+" then 1 else -1 end) * ((($c.hh | tonumber) * 3600) + (($c.mm | tonumber) * 60))
                      end
                  end
              end
          end;
        (map(select(.scope == $scope and .bead.id == $id)) | first | .bead) as $root
        | ($root.metadata // {}) as $rm
        | (map(.bead) | map(select(.id != $id and (((.metadata // {})["gc.root_bead_id"] // "") | tostring) == $id))) as $desc
        | ($desc | map(select(.status == "open" and ((.assignee // "") == ""))) | map(.id)) as $unclaimed
        | ($desc | map(select(.status == "in_progress" and ((.assignee // "") != "") and ((.assignee) | islive($live)))) | map(.id)) as $claimed_live
        | ([ $rm.session_name, $rm["gc.session_name"], $rm["gc.sessionName"],
             $rm.session_id, $rm["gc.session_id"], $rm["gc.sessionId"],
             $root.assignee, $rm["gc.routed_to"] ]
           | map(select(. != null and . != "") | tostring) | unique) as $drivers
        | ($drivers | map(select(islive($live)))) as $alive
        | ([ $root.created_at ] + ($desc | map(.updated_at // .created_at)) | map(epoch) | map(select(. != null)) | max) as $last
        | {
            unclaimed: $unclaimed,
            claimed_live: $claimed_live,
            drivers: $drivers,
            alive: $alive,
            silence: (if $last == null then null else ($now - $last) end),
            marker: (($rm[$marker] // "") | tostring),
            title: ($root.title // ""),
            formula: (($rm["gc.formula_name"] // $rm["gc.formula"] // "") | tostring),
            convoy: (($rm["gc.input_convoy_id"] // "") | tostring),
            routed_to: (($rm["gc.routed_to"] // "") | tostring)
          }
    ' "$BEADS_FILE" 2>/dev/null
}

verdict_field() {
    printf '%s' "$1" | jq -r "$2" 2>/dev/null
}

ESCALATED=0
DEDUPED=0
CLEARED=0
FAILED=0
# The id comes first: the HQ scope is an empty field, and `read` with a tab
# IFS strips a LEADING empty field (tab is IFS whitespace), which would shift
# the id into $scope and silently skip every HQ-scoped root.
while IFS="$(printf '\t')" read -r root_id scope; do
    [ -n "$root_id" ] || continue

    RIG_ARG1=""
    RIG_ARG2=""
    if [ -n "$scope" ]; then
        RIG_ARG1="--rig"
        RIG_ARG2="$scope"
    fi

    VERDICT="$(root_verdict "$scope" "$root_id")" || continue
    [ -n "$VERDICT" ] && [ "$VERDICT" != "null" ] || continue

    DRIVER_COUNT="$(verdict_field "$VERDICT" '.drivers | length')"
    ALIVE_COUNT="$(verdict_field "$VERDICT" '.alive | length')"
    UNCLAIMED_COUNT="$(verdict_field "$VERDICT" '.unclaimed | length')"
    CLAIMED_LIVE_COUNT="$(verdict_field "$VERDICT" '.claimed_live | length')"
    SILENCE="$(verdict_field "$VERDICT" '.silence // empty')"
    MARKER="$(verdict_field "$VERDICT" '.marker')"

    # Dead = a driver is recorded and none of its identities is live, the run
    # has an open unclaimed step, no step is being worked by a live session,
    # and the silence exceeds the threshold. A root with NO recorded driver at
    # all is unverifiable and left alone rather than guessed at.
    DEAD=0
    if [ "${DRIVER_COUNT:-0}" -gt 0 ] && [ "${ALIVE_COUNT:-0}" -eq 0 ] \
        && [ "${UNCLAIMED_COUNT:-0}" -gt 0 ] && [ "${CLAIMED_LIVE_COUNT:-0}" -eq 0 ] \
        && [ -n "$SILENCE" ] && [ "$SILENCE" -ge "$THRESHOLD_S" ]; then
        DEAD=1
    fi

    if [ "$DEAD" -eq 0 ]; then
        # Condition absent or cleared. Drop a stale marker so a recurrence on
        # this root re-alerts; a root that was never marked is untouched.
        if [ -n "$MARKER" ]; then
            if gc bd update "$root_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --unset-metadata "$MARKER_KEY" >/dev/null 2>&1; then
                CLEARED=$((CLEARED + 1))
            else
                echo "dead-run-detect: failed to clear $MARKER_KEY on $root_id (will retry next sweep)" >&2
            fi
        fi
        continue
    fi

    if [ -n "$MARKER" ]; then
        # Already escalated for this occurrence; never re-mail every cycle.
        DEDUPED=$((DEDUPED + 1))
        continue
    fi

    TITLE="$(verdict_field "$VERDICT" '.title')"
    DRIVERS="$(verdict_field "$VERDICT" '.drivers | join(", ")')"
    UNCLAIMED_IDS="$(verdict_field "$VERDICT" '.unclaimed | join(" ")')"
    FORMULA="$(verdict_field "$VERDICT" '.formula')"
    CONVOY="$(verdict_field "$VERDICT" '.convoy')"
    ROUTED_TO="$(verdict_field "$VERDICT" '.routed_to')"
    age_h=$(( SILENCE / 3600 ))
    age_m=$(( (SILENCE % 3600) / 60 ))

    SUBJECT="DEAD_RUN: workflow root $root_id has no live driving session"
    BODY="Workflow root $root_id${TITLE:+ ($TITLE)} is in_progress but the session driving it ($DRIVERS) is not in gc session list (HQ + $RIG_COUNT rig(s)), and the run has been silent for ${age_h}h${age_m}m (threshold: $THRESHOLD).
It has $UNCLAIMED_COUNT open, unclaimed step bead(s): $UNCLAIMED_IDS
Scope: ${scope:-hq}${FORMULA:+; formula: $FORMULA}${CONVOY:+; input convoy: $CONVOY}${ROUTED_TO:+; routed to: $ROUTED_TO}

No sweep will recover this on its own: the reaper skips roots with open descendants, orphan-sweep only resets beads whose assignee is unknown, and the dashboard staleness flag is display-only.

Recovery recipe (validated in production):
1. Re-enter the build with the latest re-entry formula. $REENTRY_FORMULA adopts the existing implementation convoy; --force replaces the dead workflow's source attachment; pass the artifact-path vars the formula requires (requirements_path, plan_path, decomposition_path):
   gc sling ${ROUTED_TO:-gc.run-operator} ${CONVOY:-<convoy-id>} --on $REENTRY_FORMULA --force --var requirements_path=<requirements.md> --var plan_path=<plan.md> --var decomposition_path=<decomposition.json>
2. Then close the dead root and its step beads in ONE gc bd close invocation (per-bead close loops time out):
   gc bd close $root_id $UNCLAIMED_IDS --reason \"dead workflow run superseded by re-entry\"

Inspect: gc bd show $root_id --json
This alert is sent once per root (marker $MARKER_KEY) and re-fires only if the condition clears and recurs."

    # Loud-fail: mark the root only on a delivered send, so an undeliverable
    # escalation surfaces and retries next sweep. The fallback address covers a
    # city with no mayor session to address.
    if gc mail send "$RECIPIENT" -s "$SUBJECT" -m "$BODY" --notify >/dev/null 2>&1 \
        || { [ "$FALLBACK_RECIPIENT" != "$RECIPIENT" ] && gc mail send "$FALLBACK_RECIPIENT" -s "$SUBJECT" -m "$BODY" --notify >/dev/null 2>&1; }; then
        if ! gc bd update "$root_id" ${RIG_ARG1:+"$RIG_ARG1" "$RIG_ARG2"} --set-metadata "$MARKER_KEY=$NOW_ISO" >/dev/null 2>&1; then
            echo "dead-run-detect: escalated $root_id but failed to write $MARKER_KEY; it may re-escalate next sweep" >&2
        fi
        ESCALATED=$((ESCALATED + 1))
    else
        echo "dead-run-detect: FAILED to escalate dead workflow run $root_id to '$RECIPIENT' (will retry next sweep)" >&2
        FAILED=$((FAILED + 1))
    fi
done <<<"$ROOTS"

if [ "$ESCALATED" -gt 0 ] || [ "$DEDUPED" -gt 0 ] || [ "$CLEARED" -gt 0 ]; then
    echo "dead-run-detect: escalated=$ESCALATED already-escalated=$DEDUPED cleared=$CLEARED"
fi

# Loud-fail: delivered escalations are already marked, so a non-zero exit now
# surfaces the per-root failure lines above to the controller log without
# losing them. exit 0 would swallow the failures (#4543).
if [ "$FAILED" -gt 0 ]; then
    echo "dead-run-detect: $FAILED dead workflow run(s) failed to escalate (see above; will retry next sweep)" >&2
    exit 1
fi
