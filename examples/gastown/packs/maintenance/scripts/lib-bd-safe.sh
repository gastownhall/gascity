#!/usr/bin/env bash
# lib-bd-safe.sh — shared safety primitives for maintenance scripts.
#
# Sourced by scripts in packs/maintenance/scripts/ to prevent two
# failure modes that hit the fleet during incident lx-wisp-ece:
#
#   * lx-qfq5r1 — unbounded `bd list` queries in spawn-storm-detect.sh
#     and wisp-compact.sh ran longer than their cooldown, stacked up,
#     and drove Dolt to 250% CPU.
#   * lx-f2z2ph — a bd child inside `$(bd list ...)` command substitution
#     kept running after its parent bash script died, reparenting to
#     systemd and continuing to hammer Dolt.
#
# This library provides three primitives every maintenance script should
# call at the top of its body:
#
#   install_trap  — propagate EXIT/INT/TERM to background jobs so bd
#                   children can't outlive the parent script.
#   acquire_lock  — non-blocking per-script flock; if another instance is
#                   already running, exit 0 silently (cooldown dedup).
#   bd_safe       — bd wrapper that runs every call under `timeout`
#                   (default 30s, override with BD_SAFE_TIMEOUT). On
#                   deadline miss, signals the parent script so the
#                   trap tears down any pending children.
#
# Usage (at the top of a script, after `set -euo pipefail`):
#
#     SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#     # shellcheck source=lib-bd-safe.sh
#     . "$SCRIPT_DIR/lib-bd-safe.sh"
#     install_trap
#     acquire_lock
#
# Then replace every `bd ...` call with `bd_safe ...`.
#
# Pure bash + POSIX utilities. Scripts that also need jq require it
# directly — this library deliberately has no jq dependency.

# bd_safe: run bd under `timeout` so a hung query can't block forever.
# Usage is identical to bd itself: `bd_safe list --status=open --json`.
# On deadline miss (timeout returns 124) this emits a log line and
# signals the parent script with SIGTERM, which the installed trap
# catches to tear down any pending children. Other non-zero exits
# (bd errors, empty results, etc.) are returned to the caller unchanged
# so existing `|| exit 0` and `|| true` patterns keep working.
bd_safe() {
    local timeout_sec="${BD_SAFE_TIMEOUT:-30}"
    local rc=0
    timeout "$timeout_sec" bd "$@" || rc=$?
    if [ "$rc" -eq 124 ]; then
        printf '%s: bd_safe deadline miss after %ss: bd %s\n' \
            "${BD_SAFE_SCRIPT_NAME:-${0##*/}}" "$timeout_sec" "$*" >&2
        # $$ expands to the invoking shell PID even inside $(...) command
        # substitution, so this signals the script tree even if bd_safe
        # was called in a subshell.
        kill -TERM "$$" 2>/dev/null || true
        exit 124
    fi
    return "$rc"
}

# acquire_lock: non-blocking per-script lock. If another instance of the
# same script is already running, exit 0 silently so stacked cooldowns
# no-op instead of piling up.
#
# Lock file location:
#   * $GC_PACK_STATE_DIR if set (per-pack/per-city state directory)
#   * /tmp/gc-maintenance/ otherwise (machine-wide fallback)
#
# The lock is released automatically when the script exits — bash
# closes FD 200 on exit. `release_lock` is provided for scripts that
# want to release early; most don't need it.
#
# Uses FD 200 by convention. Scripts that also want to use FD 200 for
# their own purposes should call release_lock first.
acquire_lock() {
    local script_name="${1:-${0##*/}}"
    local lockdir="${GC_PACK_STATE_DIR:-/tmp/gc-maintenance}"
    if ! mkdir -p "$lockdir" 2>/dev/null; then
        # Can't create lockdir — fall through without locking rather
        # than fail the whole run. Best-effort semantics.
        return 0
    fi
    local lockfile="$lockdir/${script_name%.sh}.lock"
    if ! exec 200>"$lockfile" 2>/dev/null; then
        return 0
    fi
    if ! flock -n 200; then
        # Another instance holds the lock. Silent cooldown dedup.
        exit 0
    fi
}

# release_lock: close FD 200 to release the flock early. Bash closes
# the FD automatically on script exit, so this is only needed by
# long-running scripts that want to yield the lock partway through.
release_lock() {
    exec 200>&- 2>/dev/null || true
}

# install_trap: propagate EXIT/INT/TERM to background jobs. Scripts
# that spawn bd or other children in the background (including inside
# `$(...)` command substitutions) rely on this trap to avoid orphaning
# children on signal — see lx-f2z2ph.
#
# Two traps are installed:
#   * EXIT captures the real exit code via `$?` and re-exits with it,
#     so scripts under `set -euo pipefail` propagate failure cleanly.
#   * INT/TERM forces a non-zero exit (143) regardless of `$?`. This
#     matters because bd_safe sends SIGTERM to `$$` on deadline miss.
#     If the signal trap captured `$?` it would grab 0 from the kill
#     command itself, masking the failure.
#
# Both traps wrap `kill $(jobs -p)` in `|| true` because there are
# usually no background jobs, so the expansion becomes bare `kill`
# which errors out — without `|| true` that would trip `set -e`
# inside the trap body.
install_trap() {
    trap '__rc=$?; kill $(jobs -p) 2>/dev/null || true; exit "$__rc"' EXIT
    trap 'kill $(jobs -p) 2>/dev/null || true; exit 143' INT TERM
}
