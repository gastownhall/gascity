#!/bin/bash
# Bash agent: graph workflow worker.
# Executes assigned graph.v2 step beads in sequence, simulating worktree
# setup/implementation/cleanup so integration tests can validate controller
# behavior through the real reconciler path.

set -euo pipefail

cd "$GC_CITY"
export BEADS_DIR="$GC_CITY/.beads"

MODE="${GC_GRAPH_MODE:-success}"
REPORT_FILE="$GC_CITY/graph-workflow-steps.log"
TRACE_FILE="$GC_CITY/graph-workflow-trace.log"
ASSIGNEE="${GC_SESSION_NAME:-${GC_AGENT:-}}"
HARNESS_STATE_DIR="$GC_CITY/.gc/test-harness"

# Keep each worker/pool slot distinct at the beads actor layer.
# `bd update --claim` claims "to you", so sharing one actor across sessions
# defeats the CAS and makes duplicate logical claims possible.
export BEADS_ACTOR="${BEADS_ACTOR:-${ASSIGNEE:-worker}}"
mkdir -p "$HARNESS_STATE_DIR"

echo "graph-worker startup: GC_CITY=${GC_CITY:-} GC_CITY_PATH=${GC_CITY_PATH:-} GC_DOLT_PORT=${GC_DOLT_PORT:-} GC_AGENT=${GC_AGENT:-} GC_SESSION_NAME=${GC_SESSION_NAME:-} PWD=$(pwd)"

trace() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "$TRACE_FILE"
}

current_port_file() {
    if [ -f "$GC_CITY/.beads/dolt-server.port" ]; then
        tr -d '\n' < "$GC_CITY/.beads/dolt-server.port"
        return 0
    fi
    return 1
}

current_runtime_port() {
    local state
    state=$(find "$GC_CITY/.gc/runtime/packs" -name dolt-state.json -print -quit 2>/dev/null || true)
    if [ -z "$state" ] || [ ! -f "$state" ]; then
        return 1
    fi
    jq -r '.port // empty' "$state" 2>/dev/null
}

trace_store() {
    local port_file runtime_port
    port_file=$(current_port_file 2>/dev/null || true)
    runtime_port=$(current_runtime_port 2>/dev/null || true)
    trace "store gc_dolt_port=${GC_DOLT_PORT:-} port_file=${port_file:-} runtime_port=${runtime_port:-} pwd=$(pwd)"
}

sanitize_key() {
    printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_'
}

trim_spaces() {
    printf '%s' "$1" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

ref_matches_suffix_list() {
    local ref="$1"
    local suffix_list="$2"
    local suffix

    [ -n "$suffix_list" ] || return 1
    IFS=',' read -r -a suffixes <<< "$suffix_list"
    for suffix in "${suffixes[@]}"; do
        suffix=$(trim_spaces "$suffix")
        [ -n "$suffix" ] || continue
        if [[ "$ref" == *"$suffix"* ]]; then
            return 0
        fi
    done
    return 1
}

transient_reason_for_ref() {
    local ref="$1"
    if [[ "$ref" == *"gemini"* ]]; then
        printf 'rate_limited'
        return 0
    fi
    printf 'transient_test_failure'
}

close_with_result() {
    local bead_id="$1"
    local outcome="$2"
    local failure_class="${3:-}"
    local failure_reason="${4:-}"

    # Swallow any bd failure — the worker runs under `set -e`, so an
    # error from a transient bd issue here (flaky socket, concurrent
    # modification, etc.) would kill the whole worker mid-loop and take
    # down the test session. Callers already tolerate unknown final
    # outcomes (they read it back via show_outcome), so failing silently
    # is safer than failing loudly.
    if [ "$outcome" = "pass" ]; then
        if ! bd update "$bead_id" --set-metadata "gc.outcome=pass" --status closed 2>/dev/null; then
            trace "close-rejected bead=$bead_id outcome=$outcome (likely already closed by controller)"
        fi
        return 0
    fi

    if ! bd update "$bead_id" \
        --set-metadata "gc.outcome=fail" \
        --set-metadata "gc.failure_class=$failure_class" \
        --set-metadata "gc.failure_reason=$failure_reason" \
        --status closed 2>/dev/null; then
        trace "close-rejected bead=$bead_id outcome=$outcome (likely already closed by controller)"
    fi
    return 0
}

should_fail_transient_once() {
    local ref="$1"
    local marker=""
    if ! ref_matches_suffix_list "$ref" "${GC_GRAPH_TRANSIENT_ONCE_SUFFIXES:-}"; then
        return 1
    fi
    marker="$HARNESS_STATE_DIR/transient-once.$(sanitize_key "$ref")"
    if [ -f "$marker" ]; then
        return 1
    fi
    : > "$marker"
    return 0
}

should_fail_transient_always() {
    local ref="$1"
    ref_matches_suffix_list "$ref" "${GC_GRAPH_ALWAYS_TRANSIENT_SUFFIXES:-}"
}

should_exit_after_claim_once() {
    local ref="$1"
    local marker=""
    if ! ref_matches_suffix_list "$ref" "${GC_GRAPH_EXIT_AFTER_CLAIM_ONCE_SUFFIXES:-}"; then
        return 1
    fi
    marker="$HARNESS_STATE_DIR/exit-after-claim-once.$(sanitize_key "$ref")"
    if [ -f "$marker" ]; then
        return 1
    fi
    : > "$marker"
    return 0
}

set_formula_verdict() {
    local bead_id="$1"
    local ref="$2"

    case "$ref" in
        *.apply-fixes*)
            bd update "$bead_id" --set-metadata "review.verdict=done" >/dev/null
            trace "set-verdict bead=$bead_id key=review.verdict value=done"
            ;;
        *.apply-design-changes*)
            bd update "$bead_id" --set-metadata "design_review.verdict=done" >/dev/null
            trace "set-verdict bead=$bead_id key=design_review.verdict value=done"
            ;;
        *.apply-code-fixes*)
            bd update "$bead_id" --set-metadata "code_review.verdict=done" >/dev/null
            trace "set-verdict bead=$bead_id key=code_review.verdict value=done"
            ;;
    esac
}

show_status() {
    timeout 10 bd show --json "$1" | json_payload | jq_bead '.status'
}

show_outcome() {
    timeout 10 bd show --json "$1" | json_payload | jq_bead '.metadata["gc.outcome"]'
}

show_assignee() {
    timeout 10 bd show --json "$1" | json_payload | jq_bead '.assignee'
}

refresh_bead_json() {
    timeout 10 bd show --json "$1" 2>/dev/null
}

owned_status_ok() {
    local status="$1"
    local assignee="$2"

    [ "$assignee" = "$BEADS_ACTOR" ] || return 1
    [ "$status" = "open" ] || [ "$status" = "in_progress" ]
}

ack_drain_if_idle() {
    if [ -z "$ASSIGNEE" ]; then
        return 1
    fi
    if ! gc runtime drain-check 2>/dev/null; then
        return 1
    fi
    trace "drain-requested assignee=$ASSIGNEE"
    gc runtime drain-ack 2>/dev/null || true
    trace "drain-acked assignee=$ASSIGNEE"
    exit 0
}

trace "startup pid=$$ assignee=${ASSIGNEE:-}"
trace_store
cleanup() {
    local rc=$?
    trace "exit pid=$rc shell=$$"
    trap - EXIT INT TERM
    pkill -TERM -P $$ >/dev/null 2>&1 || true
    sleep 0.05
    pkill -KILL -P $$ >/dev/null 2>&1 || true
    exit "$rc"
}
trap cleanup EXIT INT TERM
misses=0

jq_bead() {
    local filter="$1"
    jq -r "if type == \"array\" then (.[0] | ($filter)) else ($filter) end // \"\""
}

json_payload() {
    awk 'found || /^[[:space:]]*[[{]/{ found=1; print }'
}

is_currently_blocked() {
    local bead_id="$1"
    local root_id="${2:-}"
    local blocked_json=""

    if [ -n "$root_id" ]; then
        blocked_json=$(timeout 10 bd blocked --json --parent "$root_id" 2>/dev/null || true)
    else
        blocked_json=$(timeout 10 bd blocked --json 2>/dev/null || true)
    fi

    printf '%s\n' "$blocked_json" | json_payload | jq -e --arg id "$bead_id" '
        if type == "array" then any(.[]?; .id == $id) else false end
    ' >/dev/null 2>&1
}

fetch_ready_queue() {
    if [ -z "$ASSIGNEE" ]; then
        return 1
    fi
    # gc hook resolves the current session via GC_ALIAS/GC_AGENT and uses the
    # session model work query tiers, so it can see both directly assigned work
    # and generic routed work for controller-materialized sessions.
    timeout 10 gc hook 2>/dev/null
}

fetch_in_progress_queue() {
    if [ -z "$ASSIGNEE" ]; then
        return 1
    fi
    timeout 10 bd list --assignee "$ASSIGNEE" --status=in_progress --json 2>/dev/null
}

select_candidate_from_queue() {
    local ready_json="$1"
    local allow_in_progress="${2:-false}"
    local candidate=""
    local bead_id=""
    local ref=""
    local kind=""
    local root_id=""
    local status_before=""
    local outcome_before=""

    while IFS= read -r candidate; do
        [ -n "$candidate" ] || continue
        bead_id=$(printf '%s\n' "$candidate" | jq -r '.id // ""' 2>/dev/null || true)
        [ -n "$bead_id" ] || continue
        ref=$(printf '%s\n' "$candidate" | jq -r '.ref // .metadata["gc.step_ref"] // ""' 2>/dev/null || true)
        kind=$(printf '%s\n' "$candidate" | jq -r '.metadata["gc.kind"] // ""' 2>/dev/null || true)
        root_id=$(printf '%s\n' "$candidate" | jq -r '.metadata["gc.root_bead_id"] // ""' 2>/dev/null || true)

        if is_currently_blocked "$bead_id" "$root_id"; then
            trace "skip-blocked bead=$bead_id ref=$ref assignee=$ASSIGNEE"
            continue
        fi

        case "$kind" in
            check|fanout|retry-eval|scope-check|workflow-finalize)
                trace "unexpected-control bead=$bead_id kind=$kind ref=$ref"
                trace_store
                exit 1
                ;;
            workflow|scope|ralph|retry)
                trace "skip-latch bead=$bead_id kind=$kind ref=$ref"
                continue
                ;;
        esac

        status_before=$(show_status "$bead_id" 2>/dev/null || true)
        outcome_before=$(show_outcome "$bead_id" 2>/dev/null || true)
        if [ "$outcome_before" = "skipped" ]; then
            trace "skip-terminal bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
            continue
        fi
        if [ "$allow_in_progress" = "true" ]; then
            if [ "$status_before" != "in_progress" ]; then
                trace "skip-terminal bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
                continue
            fi
        elif [ "$status_before" != "open" ]; then
            trace "skip-terminal bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
            continue
        fi

        printf '%s\n' "$candidate"
        return 0
    done < <(printf '%s\n' "$ready_json" | json_payload | jq -c 'if type == "array" then .[]? else . end' 2>/dev/null || true)

    return 1
}

while true; do
    owned=""
    bead_json=""
    owns_bead="false"

    if [ -n "$ASSIGNEE" ]; then
        owned=$(fetch_in_progress_queue || true)
    fi
    bead_json=$(select_candidate_from_queue "$owned" "true" || true)
    bead_id=$(printf '%s\n' "$bead_json" | jq -r '.id // ""' 2>/dev/null || true)
    if [ -n "$bead_id" ]; then
        owns_bead="true"
        trace "resume bead=$bead_id assignee=$ASSIGNEE"
    fi

    if [ "$owns_bead" != "true" ]; then
        ack_drain_if_idle || true
    fi

    ready=""
    ready_rc=0
    if [ -z "$bead_id" ]; then
        if [ -n "$ASSIGNEE" ]; then
            if ready=$(fetch_ready_queue); then
                :
            else
                ready_rc=$?
            fi
        fi
        bead_json=$(select_candidate_from_queue "$ready" || true)
        bead_id=$(printf '%s\n' "$bead_json" | jq -r '.id // ""' 2>/dev/null || true)
    fi
    if [ -z "$bead_id" ]; then
        misses=$((misses + 1))
        if [ "$ready_rc" -ne 0 ]; then
            trace "ready-error rc=$ready_rc assignee=$ASSIGNEE"
        elif [ $((misses % 25)) -eq 0 ]; then
            trace "idle misses=$misses assignee=$ASSIGNEE"
        fi
        sleep 0.2
        continue
    fi
    misses=0

    is_claimable_work=$(printf '%s\n' "$bead_json" | jq -r '(.assignee // "" | length == 0) and ((((.metadata // {})["gc.routed_to"] // "" | length > 0) or ((.labels // []) | any(startswith("pool:")))))' 2>/dev/null || echo "false")
    claimed_here="false"
    if [ "$is_claimable_work" = "true" ] && [ "$owns_bead" != "true" ]; then
        ack_drain_if_idle || true
        if ! claimed=$(timeout 10 bd update "$bead_id" --claim --json 2>/dev/null); then
            trace "claim-miss bead=$bead_id assignee=$ASSIGNEE"
            sleep 0.2
            continue
        fi
        bead_json="$claimed"
        bead_id=$(printf '%s\n' "$bead_json" | json_payload | jq -r 'if type == "array" then (.[0].id // "") else (.id // "") end' 2>/dev/null || true)
        claimed_here="true"
        owns_bead="true"
        trace "claim bead=$bead_id assignee=$ASSIGNEE"
    fi

    ref=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.ref // .metadata["gc.step_ref"] // ""')
    kind=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.metadata["gc.kind"] // ""')
    root_id=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.metadata["gc.root_bead_id"] // ""')
    source_id=""
    work_dir=""
    if [ -n "$root_id" ]; then
        if ! root_json=$(timeout 10 bd show --json "$root_id" 2>/dev/null); then
            trace "root-show-failed bead=$bead_id root=$root_id"
            sleep 1
            continue
        fi
        source_id=$(printf '%s\n' "$root_json" | json_payload | jq_bead '.metadata["gc.source_bead_id"]')
    fi
    if [ -n "$source_id" ]; then
        if ! source_json=$(timeout 10 bd show --json "$source_id" 2>/dev/null); then
            trace "source-show-failed bead=$bead_id source=$source_id"
            sleep 1
            continue
        fi
        work_dir=$(printf '%s\n' "$source_json" | json_payload | jq_bead '.metadata.work_dir')
    fi

    if is_currently_blocked "$bead_id" "$root_id"; then
        trace "skip-blocked bead=$bead_id ref=$ref assignee=$ASSIGNEE"
        if [ "$is_claimable_work" = "true" ]; then
            if ! timeout 10 bd update "$bead_id" --assignee "" --status open >/dev/null 2>&1; then
                trace "release-failed bead=$bead_id ref=$ref"
            else
                trace "released bead=$bead_id ref=$ref"
            fi
        fi
        sleep 0.2
        continue
    fi

    case "$kind" in
        check|fanout|retry-eval|scope-check|workflow-finalize)
            trace "unexpected-control bead=$bead_id kind=$kind ref=$ref"
            trace_store
            exit 1
            ;;
        workflow|scope|ralph|retry)
            trace "skip-latch bead=$bead_id kind=$kind ref=$ref"
            sleep 0.2
            continue
            ;;
    esac

    if ! bead_json=$(refresh_bead_json "$bead_id"); then
        trace "state-refresh-failed bead=$bead_id ref=$ref"
        sleep 0.2
        continue
    fi
    bead_payload=$(printf '%s\n' "$bead_json" | json_payload)
    if [ -z "$bead_payload" ] || ! printf '%s\n' "$bead_payload" | jq -e . >/dev/null 2>&1; then
        trace "state-refresh-failed bead=$bead_id ref=$ref reason=invalid-json"
        sleep 0.2
        continue
    fi
    status_before=$(printf '%s\n' "$bead_payload" | jq_bead '.status')
    outcome_before=$(printf '%s\n' "$bead_payload" | jq_bead '.metadata["gc.outcome"]')
    assignee_before=$(printf '%s\n' "$bead_payload" | jq_bead '.assignee')
    if [ "$outcome_before" = "skipped" ]; then
        trace "skip-before-action bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi
    if [ "$owns_bead" = "true" ]; then
        if ! owned_status_ok "$status_before" "$assignee_before"; then
            trace "skip-before-action bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before assignee=$assignee_before"
            sleep 0.2
            continue
        fi
    elif [ "$status_before" != "open" ]; then
        trace "skip-before-action bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi

    if should_exit_after_claim_once "$ref"; then
        trace "exit-after-claim bead=$bead_id ref=$ref assignee=$ASSIGNEE"
        exit 1
    fi

    # Report emission is deferred until just before close_with_result so a
    # step that gets skipped by the controller mid-processing (after we
    # fetched it from the ready queue but before we committed to closing)
    # does not appear in the report. Writing here would violate
    # TestGraphWorkflowFailureRunsCleanup's "report should not include
    # .implement after abort" invariant.
    trace "run bead=$bead_id ref=$ref kind=$kind source=$source_id work_dir=$work_dir"
    trace_store

    case "$ref" in
        *.workspace-setup*)
            if [ -z "$work_dir" ]; then
                work_dir="$GC_CITY/worktrees/$source_id"
                mkdir -p "$work_dir"
                bd update "$source_id" --set-metadata "work_dir=$work_dir"
                trace "workspace-setup source=$source_id work_dir=$work_dir"
            fi
            ;;
        *.preflight-tests*)
            if [ "$MODE" = "fail-preflight" ]; then
                printf '%s\n' "$ref" >> "$REPORT_FILE"
                trace "close-fail bead=$bead_id ref=$ref class=hard reason=preflight_failed"
                close_with_result "$bead_id" "fail" "hard" "preflight_failed"
                trace "close-returned bead=$bead_id"
                status_after=$(show_status "$bead_id" 2>/dev/null || true)
                outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
                trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
                continue
            fi
            ;;
        *.implement*)
            if [ -z "$work_dir" ]; then
                echo "missing work_dir during implement" >&2
                exit 1
            fi
            mkdir -p "$work_dir"
            printf 'implemented\n' > "$work_dir/implemented.txt"
            ;;
        *.submit*)
            bd update "$source_id" --set-metadata "submitted=true"
            trace "submitted source=$source_id"
            ;;
        *.cleanup-worktree*)
            if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
                rm -rf "$work_dir"
                trace "cleanup removed work_dir=$work_dir"
            fi
            bd update "$source_id" --unset-metadata work_dir
            trace "cleanup unset work_dir source=$source_id"
            ;;
    esac

    set_formula_verdict "$bead_id" "$ref"

    if should_fail_transient_once "$ref"; then
        reason=$(transient_reason_for_ref "$ref")
        printf '%s\n' "$ref" >> "$REPORT_FILE"
        trace "close-fail bead=$bead_id ref=$ref class=transient reason=$reason mode=once"
        close_with_result "$bead_id" "fail" "transient" "$reason"
        trace "close-returned bead=$bead_id"
        status_after=$(show_status "$bead_id" 2>/dev/null || true)
        outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
        trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
        continue
    fi
    if should_fail_transient_always "$ref"; then
        reason=$(transient_reason_for_ref "$ref")
        printf '%s\n' "$ref" >> "$REPORT_FILE"
        trace "close-fail bead=$bead_id ref=$ref class=transient reason=$reason mode=always"
        close_with_result "$bead_id" "fail" "transient" "$reason"
        trace "close-returned bead=$bead_id"
        status_after=$(show_status "$bead_id" 2>/dev/null || true)
        outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
        trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
        continue
    fi

    status_before=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_before=$(show_outcome "$bead_id" 2>/dev/null || true)
    assignee_before=$(show_assignee "$bead_id" 2>/dev/null || true)
    if [ "$outcome_before" = "skipped" ]; then
        trace "skip-before-close bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi
    if [ "$owns_bead" = "true" ]; then
        if ! owned_status_ok "$status_before" "$assignee_before"; then
            trace "skip-before-close bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before assignee=$assignee_before"
            sleep 0.2
            continue
        fi
    elif [ "$status_before" != "open" ]; then
        trace "skip-before-close bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi

    # Abort-propagation defense for TestGraphWorkflowFailureRunsCleanup.
    # Without this, a worker that picked up .implement after preflight
    # closed with fail — but before the controller's skipOpenScopeMembers
    # pass ran — could race the controller in two ways:
    #   1. Outcome race: worker closes .implement with pass before the
    #      controller's skip lands. bd allows overwrites on closed beads,
    #      so whoever writes last wins.
    #   2. Report race: worker writes .implement to REPORT_FILE between
    #      its own outcome check and the close.
    # Defense: detect any sibling hard failure in the same workflow and
    # close this bead as skipped ourselves instead of closing with pass.
    # Scoped to gc.failure_class="hard" so retry-flavor transient
    # failures (which the controller DOESN'T use to skip siblings) don't
    # trigger false positives.
    # Teardown beads must still run after body failure; only body members get
    # the sibling-hard-fail short-circuit.
    sibling_hard_fail="false"
    if [ "$kind" != "cleanup" ] && [ -n "$root_id" ]; then
        if sibling_json=$(timeout 10 bd list --all --limit=0 --json 2>/dev/null); then
            if printf '%s\n' "$sibling_json" | json_payload | jq -e \
                --arg root "$root_id" --arg self "$bead_id" '
                    if type == "array" then
                        any(.[]?; (.metadata // {})["gc.root_bead_id"] == $root
                                    and .id != $self
                                    and .status == "closed"
                                    and (.metadata // {})["gc.outcome"] == "fail"
                                    and (.metadata // {})["gc.failure_class"] == "hard")
                    else false
                    end' >/dev/null 2>&1; then
                sibling_hard_fail="true"
            fi
        fi
    fi
    if [ "$sibling_hard_fail" = "true" ] && [[ "$ref" != *.cleanup-worktree* ]]; then
        trace "skip-sibling-hard-fail bead=$bead_id ref=$ref root=$root_id (sibling has outcome=fail class=hard; closing self as skipped)"
        bd update "$bead_id" --set-metadata "gc.outcome=skipped" --status closed 2>/dev/null || \
            trace "skip-close-failed bead=$bead_id ref=$ref"
        continue
    fi

    printf '%s\n' "$ref" >> "$REPORT_FILE"
    trace "close bead=$bead_id ref=$ref"
    trace_store
    close_with_result "$bead_id" "pass"
    trace "close-returned bead=$bead_id"
    trace_store
    status_after=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
    trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
done
