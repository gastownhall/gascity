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

show_status() {
    timeout 10 bd show --json "$1" | json_payload | jq_bead '.status'
}

show_outcome() {
    timeout 10 bd show --json "$1" | json_payload | jq_bead '.metadata["gc.outcome"]'
}

trace "startup pid=$$ assignee=${ASSIGNEE:-}"
trace_store
trap 'trace "exit pid=$? shell=$$"' EXIT
misses=0

jq_bead() {
    local filter="$1"
    jq -r "if type == \"array\" then (.[0] | ($filter)) else ($filter) end // \"\""
}

json_payload() {
    awk 'found || /^[[:space:]]*[[{]/{ found=1; print }'
}

while true; do
    ready=""
    ready_rc=0
    if [ -n "$ASSIGNEE" ]; then
        if ! ready=$(timeout 10 gc hook "$ASSIGNEE" 2>/dev/null); then
            ready_rc=$?
        fi
    fi
    bead_id=$(printf '%s\n' "$ready" | json_payload | jq -r 'if type == "array" then (.[0].id // "") else (.id // "") end' 2>/dev/null || true)
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

    bead_json="$ready"
    ref=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.ref')
    if [ -z "$ref" ]; then
        ref=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.metadata["gc.step_ref"]')
    fi
    kind=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.metadata["gc.kind"]')
    root_id=$(printf '%s\n' "$bead_json" | json_payload | jq_bead '.metadata["gc.root_bead_id"]')
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

    case "$kind" in
        check|fanout|scope-check|workflow-finalize)
            trace "unexpected-control bead=$bead_id kind=$kind ref=$ref"
            trace_store
            exit 1
            ;;
        workflow|scope)
            trace "skip-latch bead=$bead_id kind=$kind ref=$ref"
            sleep 0.2
            continue
            ;;
    esac

    status_before=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_before=$(show_outcome "$bead_id" 2>/dev/null || true)
    if [ "$status_before" != "open" ] || [ "$outcome_before" = "skipped" ]; then
        trace "skip-terminal bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi

    status_before=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_before=$(show_outcome "$bead_id" 2>/dev/null || true)
    if [ "$status_before" != "open" ] || [ "$outcome_before" = "skipped" ]; then
        trace "skip-before-action bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi

    printf '%s\n' "$ref" >> "$REPORT_FILE"
    trace "run bead=$bead_id ref=$ref kind=$kind source=$source_id work_dir=$work_dir"
    trace_store

    case "$ref" in
        *.workspace-setup)
            if [ -z "$work_dir" ]; then
                work_dir="$GC_CITY/worktrees/$source_id"
                mkdir -p "$work_dir"
                bd update "$source_id" --set-metadata "work_dir=$work_dir"
                trace "workspace-setup source=$source_id work_dir=$work_dir"
            fi
            ;;
        *.preflight-tests)
            if [ "$MODE" = "fail-preflight" ]; then
                trace "close-fail bead=$bead_id ref=$ref"
                bd update "$bead_id" --set-metadata "gc.outcome=fail" --status closed
                trace "close-returned bead=$bead_id"
                status_after=$(show_status "$bead_id" 2>/dev/null || true)
                outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
                trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
                continue
            fi
            ;;
        *.implement)
            if [ -z "$work_dir" ]; then
                echo "missing work_dir during implement" >&2
                exit 1
            fi
            mkdir -p "$work_dir"
            printf 'implemented\n' > "$work_dir/implemented.txt"
            ;;
        *.submit)
            bd update "$source_id" --set-metadata "submitted=true"
            trace "submitted source=$source_id"
            ;;
        *.cleanup-worktree)
            if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
                rm -rf "$work_dir"
                trace "cleanup removed work_dir=$work_dir"
            fi
            bd update "$source_id" --unset-metadata work_dir
            trace "cleanup unset work_dir source=$source_id"
            ;;
    esac

    status_before=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_before=$(show_outcome "$bead_id" 2>/dev/null || true)
    if [ "$status_before" != "open" ] || [ "$outcome_before" = "skipped" ]; then
        trace "skip-before-close bead=$bead_id ref=$ref status=$status_before outcome=$outcome_before"
        sleep 0.2
        continue
    fi

    trace "close bead=$bead_id ref=$ref"
    trace_store
    bd update "$bead_id" --status closed
    trace "close-returned bead=$bead_id"
    trace_store
    status_after=$(show_status "$bead_id" 2>/dev/null || true)
    outcome_after=$(show_outcome "$bead_id" 2>/dev/null || true)
    trace "closed bead=$bead_id status=$status_after outcome=$outcome_after"
done
