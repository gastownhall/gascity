#!/usr/bin/env bash
#
# push-gate-lock-lib.sh — cross-invocation concurrency bound for the heavy
# local test suites (ga-owh20p).
#
# WHY THIS EXISTS
#   Two measured incidents (2026-07-14 load 88.07 with 5 concurrent
#   test-fast-parallel runs + 2 gates + 1 `make test`; 2026-07-2x load
#   53.6-82.1 with ~20 concurrent gate processes) show the pre-push gate has
#   zero cross-invocation concurrency control: any number of pushes, direct
#   `make` runs, and CI jobs can pile onto the same host at once, producing
#   false-red failures (timeouts, OOM-adjacent slowdowns) indistinguishable
#   from real regressions. This is the same recurring flake cluster
#   documented repeatedly since 2026-07-14 (see bd ga-owh20p notes).
#
#   This is one of three orthogonal axes: (1) within-run job sizing
#   (scripts/test-local-job-count, existing), (2) per-invocation resource
#   isolation (systemd-run --slice, existing), (3) cross-invocation
#   concurrency bound — this file. See TESTING.md for all three.
#
# MECHANISM
#   Adapted from packs/maintainer-pr-review/scripts/run-lock-lib.sh's
#   mpr_acquire_global_slot in the gc-management meta-repo (numbered
#   flock(1) slot files under a slot directory; the kernel releases the
#   lock when the last descriptor on the open-file-description is closed or
#   unlocked, so a normal exit, a test failure, and a crash all free the
#   slot alike — no PID-file liveness probing needed).
#
#   FD inheritance is deliberately severed at the fan-out boundary in
#   scripts/test-local-parallel: the slot FD is closed inside the subshell
#   that spawns the job fan-out, so no test job — and no daemon a test job
#   leaks (a tmux server, a dolt sql-server, an escaped `gc`) — ever holds a
#   copy of it. The consequence for operators is worth stating plainly: a
#   slot that is still locked when its gate process is gone is NOT a stale
#   file the kernel forgot to clean up. It means some descendant inherited
#   the descriptor anyway and outlived the gate. push_gate_describe_slots
#   flags exactly that case; the fix is to find and kill the leaked
#   descendant (`lsof <slot-file>`), never to delete the slot file.
#
#   One deliberate deviation from mpr: mpr's caller is an automatic
#   cooldown-retry dispatcher, so it fails fast (immediate EX_TEMPFAIL) when
#   all slots are busy. This gate's caller is a synchronous, human/agent-
#   facing command (a push, or a direct `make` invocation), so instead of
#   failing instantly it polls with a bounded wait, printing an immediate
#   diagnostic the moment it starts waiting (FR5) and naming current
#   holders. Only after the wait bound elapses does it report failure — the
#   caller is expected to map that to `exit 75` (EX_TEMPFAIL), never a bare
#   `exit 1`, so this is never confused with a real test failure or with
#   scripts/push-ownership-guard.sh's unrelated exit-1 contract.
#
# CONTRACT
#   - Slot content is diagnostic only: "<pid> <iso8601-utc> <label>
#     <hostname>" (mpr's own global slot only stamps pid+timestamp; this
#     adds label+hostname per FR8, since callers here span an entire city
#     of heterogeneous rigs/roles rather than one single-purpose script).
#     Slot files are never sourced or eval'd — display only.
#   - This library is entirely bd/beads/claim-lease-agnostic. It is a
#     generic, reusable mutex-semaphore primitive with no awareness of bead
#     claims, mail, or any other Gas City concept. Do not add any
#     bd-specific behavior here — layer that (if ever needed) in the
#     caller, not this library.
#   - GC_PUSH_GATE_NO_CAP=1 disables the cap entirely for one invocation
#     (escape hatch, FR9): acquire always succeeds immediately, nothing to
#     release. A missing flock(1) degrades the same way — a warning plus an
#     uncapped run — rather than blocking the caller for the full wait
#     bound, matching how nice/ionice and the systemd slice are treated as
#     optional in scripts/test-local-parallel.
#   - Malformed tunables fall back to their documented defaults with a
#     diagnostic naming the offending variable; they never reach arithmetic
#     or `sleep` unvalidated.
#   - A timed-out acquire returns 1 (shell-false), and ONLY a timed-out
#     acquire returns 1 — every degrade case (missing flock(1), a slot dir
#     that cannot be created) prints its own diagnostic and returns 0 with
#     an empty fd instead, so callers can trust that a 1 always means a
#     real wait-bound expiry, never an environment defect misreported as
#     fleet contention. This library never calls `exit` itself — mapping a
#     timeout to process exit code 75 is the caller's job
#     (scripts/test-local-parallel), keeping this file a pure, testable
#     function library.
#
# FUNCTIONS
#   push_gate_city_root
#       Print the resolved city root. Tries GC_CITY_PATH, GC_CITY,
#       GC_CITY_ROOT in turn — each is validated (must contain city.toml or
#       a legacy .gc/ runtime root) before being trusted, so a stray or
#       malicious env var can't redirect the lock directory anywhere
#       arbitrary (mirrors cmd/gc/main.go's validateCityPath intent). Falls
#       back to a directory walk-up from $PWD looking for city.toml (or,
#       failing that, the first ancestor with a legacy .gc/ root),
#       ceiling-bounded at $HOME. Best-effort bash port of
#       cmd/gc/city_discovery.go's findCityWithOptions — not a byte-for-
#       byte parity guarantee (this is NFR4's "degrade outside a city"
#       fallback path, not the primary resolution mechanism real `gc`
#       sessions rely on via env vars). Returns 1 if nothing is found.
#   push_gate_slots_dir
#       Print the slot directory to use: <city_root>/.gc/gate-slots when
#       push_gate_city_root resolves, else <git_common_dir>/gate-slots —
#       still <repo_root>/.git/gate-slots in a normal clone, but sibling
#       linked worktrees resolve to the one shared common dir instead of a
#       per-worktree `.git` file that cannot hold a directory
#       (NFR4 fallback, never /tmp — see AGENTS.md Build Cache Conventions).
#       Does not create the directory (push_gate_acquire_slot does).
#   push_gate_acquire_slot <slot_dir> <fd_out_var> [holder_label]
#       Reads tunables from env: PUSH_GATE_MAX_CONCURRENT (default 2),
#       PUSH_GATE_MAX_WAIT_SECONDS (default 600), PUSH_GATE_POLL_SECONDS
#       (default 15); each is validated and falls back to its default on a
#       malformed value. holder_label defaults to
#       ${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}. If the slot
#       dir cannot be created (e.g. an unwritable parent), degrades the same
#       way as a missing flock(1): diagnostic to stderr, empty fd, return 0
#       — never conflated with a timeout. Otherwise sweeps slots 0..N-1
#       non-blocking; acquires the first free one immediately (fd assigned
#       to the caller's <fd_out_var>, return 0). If all slots are busy:
#       prints an immediate unbuffered diagnostic naming current holders
#       (FR5), then re-sweeps every POLL_SECONDS until a slot frees or
#       MAX_WAIT_SECONDS elapses. Returns 0 (acquired) or 1 (timed out —
#       caller should `exit 75`).
#   push_gate_describe_slots <slot_dir> <max_concurrent>
#       Print one "slot-<i>: <holder line>" line per currently-occupied
#       slot, for the FR5 wait message and FR8 operator diagnostics. A slot
#       whose recorded PID no longer exists is flagged as a leaked
#       descendant (see MECHANISM), since that is the one case where the
#       holder line alone points at the wrong process.
#   push_gate_release_slot <fd>
#       Explicit release + close. Normally unnecessary (process exit
#       releases the flock) — provided for tests and tight loops, mirroring
#       mpr_release_run_lock.
#
# PORTABILITY
#   This file deliberately stays bash 3.2-compatible (macOS's stock
#   /bin/bash): no `local -n` namerefs (4.3) and no `exec {var}<>` dynamic
#   FD allocation (4.1). Sibling scripts under the same entrypoint hold the
#   same floor on purpose — see scripts/go-test-observable and
#   scripts/test-integration-shard.
#
# Sourced by scripts/test-local-parallel and directly by
# scripts/test-push-gate-lock.sh.

# Base file-descriptor number for slot <i>; slot <i> always maps to
# PUSH_GATE_FD_BASE + i. Fixed numbers rather than bash 4.1's `exec {var}<>`
# keep the 3.2 floor above.
PUSH_GATE_FD_BASE=200

# Resolve the city root, validating any env var before trusting it so a
# stray GC_CITY_PATH can't redirect the lock directory arbitrarily.
push_gate_city_root() {
    local _pgc_var _pgc_candidate _pgc_abs
    for _pgc_var in GC_CITY_PATH GC_CITY GC_CITY_ROOT; do
        _pgc_candidate="${!_pgc_var:-}"
        [[ -n "$_pgc_candidate" ]] || continue
        _pgc_abs="$(cd "$_pgc_candidate" 2>/dev/null && pwd)" || continue
        if [[ -f "$_pgc_abs/city.toml" || -d "$_pgc_abs/.gc" ]]; then
            printf '%s\n' "$_pgc_abs"
            return 0
        fi
    done

    # Walk-up discovery: bash port of cmd/gc/city_discovery.go's
    # findCityWithOptions. city.toml wins outright; a legacy .gc/-only
    # ancestor is remembered as a fallback but only used if no city.toml is
    # ever found before the ceiling.
    local _pgc_dir="$PWD" _pgc_home="${HOME:-}" _pgc_legacy="" _pgc_parent
    while :; do
        if [[ -f "$_pgc_dir/city.toml" ]]; then
            printf '%s\n' "$_pgc_dir"
            return 0
        fi
        if [[ -z "$_pgc_legacy" && -d "$_pgc_dir/.gc" ]]; then
            _pgc_legacy="$_pgc_dir"
        fi
        if [[ -n "$_pgc_home" && "$_pgc_dir" == "$_pgc_home" ]]; then
            break
        fi
        _pgc_parent="$(dirname "$_pgc_dir")"
        [[ "$_pgc_parent" == "$_pgc_dir" ]] && break
        _pgc_dir="$_pgc_parent"
    done

    if [[ -n "$_pgc_legacy" ]]; then
        printf '%s\n' "$_pgc_legacy"
        return 0
    fi
    return 1
}

# Print the slot directory to use (city-rooted, or common-Git-dir fallback).
# Does not create it.
push_gate_slots_dir() {
    local _pgs_city_root
    if _pgs_city_root="$(push_gate_city_root)"; then
        printf '%s/.gc/gate-slots\n' "$_pgs_city_root"
        return 0
    fi
    # --git-common-dir (Git 2.5+) may print a path relative to $PWD, so
    # absolutize it here rather than with --path-format=absolute (Git 2.31+):
    # git rev-parse echoes an unrecognized option and still exits 0, which
    # would smuggle garbage past this `if` on older git.
    local _pgs_git_common
    if _pgs_git_common="$(git rev-parse --git-common-dir 2>/dev/null)" && [[ -n "$_pgs_git_common" ]]; then
        _pgs_git_common="$(cd "$_pgs_git_common" 2>/dev/null && pwd)" || return 1
        printf '%s/gate-slots\n' "$_pgs_git_common"
        return 0
    fi
    return 1
}

# Print one diagnostic line per currently-occupied slot.
push_gate_describe_slots() {
    local _pgd_dir="$1" _pgd_max="$2"
    local _pgd_i _pgd_slot _pgd_line _pgd_pid
    for (( _pgd_i = 0; _pgd_i < _pgd_max; _pgd_i++ )); do
        _pgd_slot="$_pgd_dir/slot-${_pgd_i}.lock"
        [[ -f "$_pgd_slot" ]] || continue
        # Still held? A successful non-blocking probe means it's free —
        # nothing to report (its file may hold a stale line from the last
        # holder, which would be misleading to print as a current holder).
        if flock -n "$_pgd_slot" -c 'exit 0' 2>/dev/null; then
            continue
        fi
        _pgd_line=""
        IFS= read -r _pgd_line <"$_pgd_slot" 2>/dev/null || true
        # The slot is held but the process that stamped it is gone, so the
        # holder line names the wrong process. The lock is being kept alive
        # by a descendant that inherited the descriptor — the one case
        # `lsof` on the slot file answers and the holder line does not.
        _pgd_pid="${_pgd_line%% *}"
        if [[ "$_pgd_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$_pgd_pid" 2>/dev/null; then
            _pgd_line="$_pgd_line (holder pid dead — likely a leaked descendant)"
        fi
        printf '  slot-%s: %s\n' "$_pgd_i" "$_pgd_line"
    done
}

# True when file descriptor $1 is already open in this shell. Slot FDs are
# fixed numbers, so a second acquire in the same process must not re-open a
# number it already holds: `exec N<>file` on a live N closes the old
# descriptor and silently drops that slot's lock.
_push_gate_fd_in_use() {
    ( true <&"$1" ) 2>/dev/null
}

# Print a validated numeric tunable. $1 = env var name, $2 = documented
# default, $3 = minimum allowed value. A malformed value is reported by name
# and replaced by the default, so it never reaches arithmetic or `sleep`.
_push_gate_tunable() {
    local _pgt_name="$1" _pgt_default="$2" _pgt_min="$3"
    local _pgt_value="${!_pgt_name:-$_pgt_default}"
    if ! [[ "$_pgt_value" =~ ^[0-9]+$ ]] || [[ "$_pgt_value" -lt "$_pgt_min" ]]; then
        echo "push-gate: ignoring malformed ${_pgt_name}='${_pgt_value}' — using default ${_pgt_default}" >&2
        _pgt_value="$_pgt_default"
    fi
    printf '%s\n' "$_pgt_value"
}

# Acquire one of PUSH_GATE_MAX_CONCURRENT slots, polling with a bounded wait
# on contention. See header for the full contract.
push_gate_acquire_slot() {
    local _pgl_slot_dir="$1"
    local _pgl_fd_var="$2"
    local _pgl_label="${3:-}"

    if [[ -z "$_pgl_label" ]]; then
        _pgl_label="${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}"
    fi

    if [[ "${GC_PUSH_GATE_NO_CAP:-}" == "1" ]]; then
        eval "$_pgl_fd_var="
        return 0
    fi

    # flock(1) is the entire mechanism. Without it every slot probe fails and
    # the caller would burn the whole wait bound before reporting a confusing
    # timeout, so degrade best-effort with a diagnostic instead.
    if ! command -v flock >/dev/null 2>&1; then
        echo "push-gate: flock(1) not found — running without a cross-invocation cap (brew install flock)" >&2
        eval "$_pgl_fd_var="
        return 0
    fi

    local _pgl_max _pgl_max_wait _pgl_poll
    _pgl_max="$(_push_gate_tunable PUSH_GATE_MAX_CONCURRENT 2 1)"
    _pgl_max_wait="$(_push_gate_tunable PUSH_GATE_MAX_WAIT_SECONDS 600 0)"
    _pgl_poll="$(_push_gate_tunable PUSH_GATE_POLL_SECONDS 15 1)"
    local _pgl_host
    _pgl_host="$(hostname 2>/dev/null || echo unknown)"

    # An unwritable slot dir (e.g. a parent path component that is a file,
    # as .git is in a linked worktree prior to push_gate_slots_dir's
    # common-dir fix) is a degrade case, not a wait-bound timeout — same
    # `return 1` used to mean both, which sent operators chasing fleet
    # contention that did not exist. Degrade best-effort instead.
    if ! mkdir -p "$_pgl_slot_dir" 2>/dev/null; then
        echo "push-gate: cannot create slot dir $_pgl_slot_dir — running without a cross-invocation cap" >&2
        eval "$_pgl_fd_var="
        return 0
    fi

    local _pgl_i _pgl_slot _pgl_fd _pgl_announced=0 _pgl_start=0

    while :; do
        for (( _pgl_i = 0; _pgl_i < _pgl_max; _pgl_i++ )); do
            _pgl_slot="$_pgl_slot_dir/slot-${_pgl_i}.lock"
            _pgl_fd=$(( PUSH_GATE_FD_BASE + _pgl_i ))
            if _push_gate_fd_in_use "$_pgl_fd"; then
                continue
            fi
            eval "exec ${_pgl_fd}<>\"\$_pgl_slot\"" || continue
            if flock -n "$_pgl_fd"; then
                printf '%s %s %s %s\n' "$$" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$_pgl_label" "$_pgl_host" >"$_pgl_slot" 2>/dev/null || true
                eval "$_pgl_fd_var=\$_pgl_fd"
                if [[ "$_pgl_announced" -eq 1 ]]; then
                    echo "push-gate: slot-${_pgl_i} acquired after wait" >&2
                fi
                return 0
            fi
            eval "exec ${_pgl_fd}>&-" || true
        done

        if [[ "$_pgl_announced" -eq 0 ]]; then
            _pgl_announced=1
            _pgl_start="$(date +%s)"
            echo "push-gate: all $_pgl_max slot(s) busy, waiting up to ${_pgl_max_wait}s (checking every ${_pgl_poll}s):" >&2
            push_gate_describe_slots "$_pgl_slot_dir" "$_pgl_max" >&2
        fi

        if (( $(date +%s) - _pgl_start >= _pgl_max_wait )); then
            echo "push-gate: timed out after ${_pgl_max_wait}s waiting for a free slot" >&2
            push_gate_describe_slots "$_pgl_slot_dir" "$_pgl_max" >&2
            return 1
        fi

        sleep "$_pgl_poll"
    done
}

# Explicitly release + close the slot FD. Normally unnecessary.
push_gate_release_slot() {
    local _pgl_fd="$1"
    [[ "$_pgl_fd" =~ ^[0-9]+$ ]] || return 0
    # Unlock before closing: the flock -u releases the open-file-description
    # itself, so any descendant that inherited a copy of this FD stops
    # holding the slot too.
    flock -u "$_pgl_fd" 2>/dev/null || true
    eval "exec ${_pgl_fd}>&-" 2>/dev/null || true
}
