#!/bin/sh

# recovery_should_skip_due_to_fsync_stall returns 0 (true) when an fsync on the
# Dolt data directory's filesystem is stalled, and 1 (false) when recovery may
# go ahead.
#
# One stalled commit fsync freezes the whole Dolt server, and restarting Dolt
# does not fix storage: the restart adds recovery fsync load to a filesystem
# that is already stalled. Callers (gc-beads-bd's recover op, gc dolt restart)
# therefore refuse to restart a live Dolt while the probe reports a stall.
#
# Go measures, this script decides: `gc dolt-state fsync-probe` times one fsync
# (bounded for the caller even when the kernel never returns) and this file
# compares it with GC_DOLT_FSYNC_WARN_MS. A stall is an unfinished probe, or a
# finished one at or above that threshold.
#
# Environment:
#   DATA_DIR               the Dolt data directory to probe (set by the caller)
#   GC_BIN                 the gc binary that provides the probe
#   GC_DOLT_FSYNC_WARN_MS  stall threshold and probe bound in milliseconds
#                          (default 1000). Empty, non-numeric or non-positive
#                          values fall back to the default.
#   GC_DOLT_FSYNC_ERROR_MS the `gc doctor` error threshold (default 5000, same
#                          fallback rules; fsync_stall_error_ms); it plays no
#                          part in this guard.
#
# The probe worker is detached on purpose: a thread stuck in an uninterruptible
# fsync (D state) cannot be killed, so the bound is enforced by abandoning it
# and the worker may outlive the call until the kernel returns. A probe that
# is merely starved of CPU past the bound reports `unfinished` too, so a heavily
# loaded host can defer a recovery that was not blocked on storage; the
# deferral is retried by the next recover, and `gc dolt restart --force`
# overrides the restart refusal.
#
# Fails open: when the probe cannot run or its output cannot be read (GC_BIN
# or DATA_DIR unset, the helper exits non-zero or reports status=error, the
# output does not parse) the function prints one "fsync-stall guard skipped"
# line to stderr and returns 1, so a broken detector never blocks recovery.
# Callers stay silent when nothing is listening: there is nothing to protect.
#
# `gc dolt restart --force` skips the refusal for an operator who still needs a
# restart; see docs/troubleshooting/dolt-storage-stalls.md.

# fsync_stall_env_ms prints the positive decimal integer in the environment
# variable named $1, or the default $2 when it is empty, non-numeric or zero.
# Leading zeros are dropped: Go's flag parser reads 0500 as octal and rejects
# 0900, and the value is passed on as --timeout-ms.
fsync_stall_env_ms() {
    _fs_value=$(eval "printf '%s' \"\${$1:-}\"")
    case "$_fs_value" in
        ''|*[!0-9]*) printf '%s\n' "$2"; return 0 ;;
    esac
    while :; do
        case "$_fs_value" in
            0?*) _fs_value=${_fs_value#0} ;;
            *) break ;;
        esac
    done
    if [ "$_fs_value" = "0" ]; then
        printf '%s\n' "$2"
        return 0
    fi
    printf '%s\n' "$_fs_value"
}

# fsync_stall_error_ms prints the `gc doctor` error threshold, 5000 unless
# GC_DOLT_FSYNC_ERROR_MS is a positive integer. The recover guard does not
# read it; the doctor check that does is tracked in ga-dnqmzh.
fsync_stall_error_ms() {
    fsync_stall_env_ms GC_DOLT_FSYNC_ERROR_MS 5000
}

fsync_stall_gc_bin() {
    if command -v resolve_gc_helper_bin >/dev/null 2>&1; then
        resolve_gc_helper_bin
    else
        printf '%s\n' "${GC_BIN:-}"
    fi
}

# load_fsync_probe_from_gc sets GC_FSYNC_STATUS and GC_FSYNC_MS from the probe
# bounded at $1 milliseconds. It returns 1 and sets GC_FSYNC_FAIL_REASON when
# the probe could not produce a usable answer.
load_fsync_probe_from_gc() {
    GC_FSYNC_STATUS=""
    GC_FSYNC_MS=""
    GC_FSYNC_FAIL_REASON=""
    _fs_gc_bin=$(fsync_stall_gc_bin)
    if [ -z "$_fs_gc_bin" ]; then
        GC_FSYNC_FAIL_REASON="no gc binary to probe with (GC_BIN unset)"
        return 1
    fi
    if [ -z "${DATA_DIR:-}" ]; then
        GC_FSYNC_FAIL_REASON="data directory unknown"
        return 1
    fi
    _fs_output=$("$_fs_gc_bin" dolt-state fsync-probe --dir "$DATA_DIR" --timeout-ms "$1" </dev/null 2>/dev/null) || {
        GC_FSYNC_FAIL_REASON="probe helper failed or reported an error"
        return 1
    }
    while IFS="$(printf '\t')" read -r _fs_key _fs_val; do
        case "$_fs_key" in
            status) GC_FSYNC_STATUS="$_fs_val" ;;
            fsync_ms) GC_FSYNC_MS="$_fs_val" ;;
        esac
    done <<EOF_FSYNC_PROBE
$_fs_output
EOF_FSYNC_PROBE
    case "$GC_FSYNC_STATUS" in
        finished)
            case "$GC_FSYNC_MS" in
                ''|*[!0-9]*)
                    GC_FSYNC_FAIL_REASON="probe output did not parse"
                    return 1
                    ;;
            esac
            ;;
        unfinished) ;;
        *)
            GC_FSYNC_FAIL_REASON="probe output did not parse"
            return 1
            ;;
    esac
    return 0
}

recovery_should_skip_due_to_fsync_stall() {
    _fs_warn=$(fsync_stall_env_ms GC_DOLT_FSYNC_WARN_MS 1000)
    if ! load_fsync_probe_from_gc "$_fs_warn"; then
        echo "warning: fsync-stall guard skipped: $GC_FSYNC_FAIL_REASON" >&2
        return 1
    fi
    [ "$GC_FSYNC_STATUS" = "unfinished" ] && return 0
    [ "$GC_FSYNC_MS" -ge "$_fs_warn" ] && return 0
    return 1
}
