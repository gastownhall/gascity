#!/bin/sh
# _bd_trace.sh — shell-side bead call trace helper.
#
# Sourced by gas city application scripts (maintenance pack scripts, tmux
# status-line). Overrides `gc` in the calling shell so each `gc bd` invocation
# appends a JSONL record to $GC_BD_TRACE_JSON. When $GC_BD_TRACE_JSON is unset,
# calls pass through with no logging overhead.
#
# Source with a source tag identifying the calling script:
#
#   . "$(dirname "$0")/_bd_trace.sh" "gate-sweep"
#
# Then call `gc bd ...` normally — calls are traced.

__bd_trace_source="${1:-unknown}"

__bd_trace_emit() {
    # $1 = command name, $2 = exit code, $3 = start_ns, $4..N = args
    if [ -z "${GC_BD_TRACE_JSON:-}" ]; then
        return 0
    fi
    __bd_trace_cmd="$1"
    __bd_trace_exit="$2"
    __bd_trace_start="$3"
    shift 3
    __bd_trace_end="$(date +%s%N 2>/dev/null || printf '%s000000000' "$(date +%s)")"
    __bd_trace_dur_ms=$(( (__bd_trace_end - __bd_trace_start) / 1000000 ))
    __bd_trace_ts="$(date -u +%Y-%m-%dT%H:%M:%S)Z"
    # Build JSON args array. Each arg is JSON-escaped with python; fall back
    # to a single string if python isn't available.
    if command -v python3 >/dev/null 2>&1; then
        __bd_trace_args=$(printf '%s\n' "$@" | python3 -c 'import sys,json; print(json.dumps([l.rstrip("\n") for l in sys.stdin]))')
    else
        __bd_trace_args="[\"$(printf '%s' "$*" | sed 's/\\/\\\\/g; s/"/\\"/g')\"]"
    fi
    printf '{"ts":"%s","source":"sh:%s/%s","args":%s,"dir":"%s","dur_ms":%d,"exit_code":%d,"pid":%d,"ppid":%d}\n' \
        "$__bd_trace_ts" \
        "$__bd_trace_source" \
        "$__bd_trace_cmd" \
        "$__bd_trace_args" \
        "$(pwd)" \
        "$__bd_trace_dur_ms" \
        "$__bd_trace_exit" \
        "$$" \
        "${PPID:-0}" \
        >> "$GC_BD_TRACE_JSON" 2>/dev/null || true
}

gc() {
    __bd_start="$(date +%s%N 2>/dev/null || printf '%s000000000' "$(date +%s)")"
    command gc "$@"
    __bd_exit=$?
    # Only emit a trace entry for gc subcommands that internally fire bd
    # (bd passthrough, hook, mail). Others (gc session/runtime/etc) emit
    # their own bd-trace lines via the Go-side TraceBDCall.
    case "$1" in
        bd|hook|mail)
            __bd_trace_emit "gc-$1" "$__bd_exit" "$__bd_start" "$@"
            ;;
    esac
    return "$__bd_exit"
}
