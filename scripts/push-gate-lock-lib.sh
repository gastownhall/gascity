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
#   lock automatically when the holder's process — and any children that
#   inherited the FD — exits, crash or normal exit alike, so a stale slot
#   can never survive a dead holder; no PID-file liveness probing needed).
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
#     release.
#   - A timed-out acquire returns 1 (shell-false). This library never calls
#     `exit` itself — mapping a timeout to process exit code 75 is the
#     caller's job (scripts/test-local-parallel), keeping this file a pure,
#     testable function library.
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
#       push_gate_city_root resolves, else <repo_root>/.git/gate-slots
#       (NFR4 fallback, never /tmp — see AGENTS.md Build Cache Conventions).
#       Does not create the directory (push_gate_acquire_slot does).
#   push_gate_acquire_slot <slot_dir> <fd_out_var> [holder_label]
#       Reads tunables from env: PUSH_GATE_MAX_CONCURRENT (default 2),
#       PUSH_GATE_MAX_WAIT_SECONDS (default 600), PUSH_GATE_POLL_SECONDS
#       (default 15). holder_label defaults to
#       ${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}. Sweeps
#       slots 0..N-1 non-blocking; acquires the first free one immediately
#       (fd assigned to the caller's <fd_out_var>, return 0). If all slots
#       are busy: prints an immediate unbuffered diagnostic naming current
#       holders (FR5), then re-sweeps every POLL_SECONDS until a slot frees
#       or MAX_WAIT_SECONDS elapses. Returns 0 (acquired) or 1 (timed out —
#       caller should `exit 75`).
#   push_gate_describe_slots <slot_dir> <max_concurrent>
#       Print one "slot-<i>: <holder line>" line per currently-occupied
#       slot, for the FR5 wait message and FR8 operator diagnostics.
#   push_gate_release_slot <fd>
#       Explicit release + close. Normally unnecessary (process exit
#       releases the flock) — provided for tests and tight loops, mirroring
#       mpr_release_run_lock.
#
# Sourced by scripts/test-local-parallel and directly by
# scripts/test-push-gate-lock.sh.

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

# Print the slot directory to use (city-rooted, or repo-relative fallback).
# Does not create it.
push_gate_slots_dir() {
    local _pgs_city_root
    if _pgs_city_root="$(push_gate_city_root)"; then
        printf '%s/.gc/gate-slots\n' "$_pgs_city_root"
        return 0
    fi
    local _pgs_repo_root
    if _pgs_repo_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
        printf '%s/.git/gate-slots\n' "$_pgs_repo_root"
        return 0
    fi
    return 1
}

# Print one diagnostic line per currently-occupied slot.
push_gate_describe_slots() {
    local _pgd_dir="$1" _pgd_max="$2"
    local _pgd_i _pgd_slot _pgd_line
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
        printf '  slot-%s: %s\n' "$_pgd_i" "$_pgd_line"
    done
}

# Acquire one of PUSH_GATE_MAX_CONCURRENT slots, polling with a bounded wait
# on contention. See header for the full contract.
push_gate_acquire_slot() {
    local _pgl_slot_dir="$1"
    local -n _pgl_fd_out="$2"
    local _pgl_label="${3:-}"

    if [[ -z "$_pgl_label" ]]; then
        _pgl_label="${GC_SESSION_NAME:-${GC_AGENT:-${GC_TEMPLATE:-unknown}}}"
    fi

    if [[ "${GC_PUSH_GATE_NO_CAP:-}" == "1" ]]; then
        _pgl_fd_out=""
        return 0
    fi

    local _pgl_max="${PUSH_GATE_MAX_CONCURRENT:-2}"
    local _pgl_max_wait="${PUSH_GATE_MAX_WAIT_SECONDS:-600}"
    local _pgl_poll="${PUSH_GATE_POLL_SECONDS:-15}"
    local _pgl_host
    _pgl_host="$(hostname 2>/dev/null || echo unknown)"

    mkdir -p "$_pgl_slot_dir" 2>/dev/null || return 1

    local _pgl_i _pgl_slot _pgl_fd _pgl_announced=0 _pgl_start=0

    while :; do
        for (( _pgl_i = 0; _pgl_i < _pgl_max; _pgl_i++ )); do
            _pgl_slot="$_pgl_slot_dir/slot-${_pgl_i}.lock"
            exec {_pgl_fd}<>"$_pgl_slot" || continue
            if flock -n "$_pgl_fd"; then
                printf '%s %s %s %s\n' "$$" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$_pgl_label" "$_pgl_host" >"$_pgl_slot" 2>/dev/null || true
                _pgl_fd_out="$_pgl_fd"
                if [[ "$_pgl_announced" -eq 1 ]]; then
                    echo "push-gate: slot-${_pgl_i} acquired after wait" >&2
                fi
                return 0
            fi
            exec {_pgl_fd}>&- || true
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
    [[ -n "$_pgl_fd" ]] || return 0
    flock -u "$_pgl_fd" 2>/dev/null || true
    exec {_pgl_fd}>&- 2>/dev/null || true
}
