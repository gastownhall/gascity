#!/usr/bin/env bash
#
# push-ownership-guard.sh — re-checks bd bead ownership/staleness
# immediately before an in-flight `git push` executes (bead ga-fip9ps.1;
# guards the race described in ga-fip9ps).
#
# THE RACE: an agent claims a bead, does work, and queues a push. A mayor
# stand-down/deconfliction ruling (reassign, reroute, close, hold) can land
# in the gap between "work finished" and "push actually executes" — e.g. a
# backgrounded push that was already queued before the ruling arrived. A
# stale push in that gap can clobber a branch another agent has since taken
# over (this happened on PR #4243, clobbering ga-lrqmb7's base). This guard
# closes that gap by re-reading bd's live state at the last possible moment
# before the push leaves the machine.
#
# THE EXPORTED FUNCTION: assert_bead_still_claimed. Re-resolves which bead
# this push is for (branch name, falling back to this session's in-progress
# assignment), re-reads its live state from bd, and returns non-zero
# (blocking the push) unless the bead is still open/in_progress, still
# assigned to one of this session's identities (any of GC_SESSION_NAME,
# GC_SESSION_ID, GC_ALIAS, GC_AGENT — mirroring the claim path — or
# GC_TEMPLATE, for work routed to the whole pool rather than claimed by one
# instance; see ga-kzl21p), still routed to this session's config identity,
# and not held by the mayor or an external actor.
#
# TWO CALL SITES (defense in depth — see ga-fip9ps.1 bead notes):
#   Layer A — .githooks/pre-push calls this unconditionally for every
#             non-deletion push, independent of what changed. Escape hatch:
#             `git push --no-verify` (git-native, skips this hook entirely).
#   Layer B — scripts/rebase-resolve-lib.sh's attempt_bounded_self_rebase
#             calls this as the last check before its own
#             --force-with-lease push, in case that push executes in a
#             context where Layer A's hook isn't wired up (e.g. a clone
#             without core.hooksPath configured).
#
# FAIL CLOSED: any ambiguity (bd unreachable, the read times out, the
# response doesn't parse) blocks the push. The only sanctioned bypass is
# `git push --no-verify` for Layer A; Layer B has no bypass by design — an
# automated force-push is exactly the case this guard exists to stop.
#
# CROSS-RIG BRANCH TOKENS (ga-f59876): branch names can be built from the
# beads of OTHER configured rigs (builder/mc-k2p9x-fix names an mc-rig bead
# while the caller's store is gascity's). The resolver discovers every
# prefix and its store from .beads/routes.jsonl (written by gc) plus the
# built-in local ga-, resolves a branch token only when it is local or its
# routed store's exact-id probe finds the bead present, and reads the bead
# from the store it resolved from — see the prefix-table block below. An
# absent token is skipped (a definitive answer, not ambiguity); an
# unreadable or missing routed store blocks. Ordinary branch words never
# gain a prefix-shaped misreading: a token only exists under a configured
# prefix, and a routed one must additionally probe present.
# EXCEPTION (deploy/*-gate branches): these deliberately ignore the
# branch-embedded id and resolve solely via the assignee fallback (see
# _pog_resolve_bead_id). A *failed* assignee read is still ambiguity and
# still blocks; but a read that succeeds and finds no in-progress
# assignment leaves nothing to check, and the push is allowed — the same
# "no session, nothing to check" semantics every unmatched branch already
# has.
#
# This file ONLY defines functions and one default-value assignment;
# sourcing it must not produce output or otherwise mutate state.
#
# Set POG_DISABLE=1 to short-circuit assert_bead_still_claimed to a bare
# `return 0`. This exists for test harnesses that call
# attempt_bounded_self_rebase directly against synthetic repos with no real
# bead behind them (e.g. scripts/test-rebase-resolve.sh) and must stay
# hermetic — it is not meant to be set on a real push path.
#
# bd/Dolt reads below are wrapped by _pog_read_with_retry: a transient
# failure (lock contention, a slow response) is retried up to
# POG_READ_ATTEMPTS times, each attempt bounded by POG_TIMEOUT_SECONDS, with
# a short sleep between attempts. Only once every attempt fails does the
# guard block — this does not weaken fail-closed semantics (a persistently
# unreachable bd still blocks) and does not mask a genuine ownership change
# (a real answer, allow or block, is accepted on its first attempt; only a
# failed/empty read is retried). Override POG_READ_ATTEMPTS for test
# harnesses that want to exercise a specific attempt count without eating
# the real sleep/timeout cost of the production default.

POG_TIMEOUT_SECONDS="${POG_TIMEOUT_SECONDS:-5}"
POG_READ_ATTEMPTS="${POG_READ_ATTEMPTS:-3}"

# Sentinel emitted by _pog_resolve_bead_id when it cannot resolve an id
# *and* the failure is ambiguous (a failed bd read) rather than a clean
# "no such assignment". Not a valid bead id by construction.
POG_AMBIGUOUS_SENTINEL="__pog_unresolved_ambiguous__"

# _pog_timeout <seconds> <cmd...>: run <cmd...> bounded by <seconds>,
# mirroring the timeout/gtimeout fallback shim in
# test/agents/graph-dispatch.sh (the only bounded-exec precedent in this
# repo). Falls back to unbounded passthrough when neither is available
# rather than failing the whole guard open or closed on a missing dev tool.
_pog_timeout() {
    local bound="$1"
    shift
    if command -v timeout >/dev/null 2>&1; then
        timeout "$bound" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$bound" "$@"
    else
        "$@"
    fi
}

# _pog_read_with_retry <cmd...>: run <cmd...> (each attempt bounded by
# _pog_timeout/POG_TIMEOUT_SECONDS), retrying up to POG_READ_ATTEMPTS times
# with a short sleep between attempts (1s, then 2s) whenever an attempt
# exits non-zero or prints nothing — the shape of a transient bd/Dolt read
# (lock contention, a slow response), not a genuine answer. Prints the
# first successful attempt's stdout and returns 0; if every attempt fails,
# prints nothing and returns 1 so the caller still fails closed. Never
# inspects the content of a successful read — a real answer (allow- or
# block-worthy) is accepted on its first attempt exactly the same way, so
# retrying cannot mask a genuine ownership change.
_pog_read_with_retry() {
    local attempt=1
    local out
    while (( attempt <= POG_READ_ATTEMPTS )); do
        if out="$(_pog_timeout "$POG_TIMEOUT_SECONDS" "$@" 2>/dev/null)" && [[ -n "$out" ]]; then
            printf '%s' "$out"
            return 0
        fi
        if (( attempt < POG_READ_ATTEMPTS )); then
            sleep "$attempt"
        fi
        attempt=$((attempt + 1))
    done
    return 1
}

# Sentinel emitted by _pog_resolve_bead_id when the routed-token resolution
# walk hit a hard failure it could not resolve into an id or a clean
# "nothing to check": a routed store that does not exist or cannot be read
# (ga-f59876). The walk has already printed the full BLOCKED reason (token,
# prefix, store path, remedies) to stderr by the time this surfaces;
# assert_bead_still_claimed maps it to rc=1 without printing another line.
# Not a valid bead id by construction.
POG_ROUTED_READ_SENTINEL="__pog_routed_read_failed__"

# --- Cross-rig routed branch tokens (ga-f59876) -----------------------------
#
# Branch names are built from bead ids, and a rig's beads can live in other
# rigs' stores (mc-k2p9x on a gascity branch): gc writes the prefix→scope
# mapping into .beads/routes.jsonl (cmd/gc/rig_beads.go writeAllRoutes, one
# {"prefix","path"} JSON object per line, path relative to the scope root
# that owns the .beads dir, "." for the local scope). The prefix table below
# is built lazily from that file (plus the built-in ga→. so a store with no
# routes keeps today's exact behavior), the candidate scanner finds
# <prefix>-(mol|wisp-)?<id>(.sub)* tokens at every non-alphanumeric boundary
# of the branch name, and the resolution walk picks the first token that is
# either local (no I/O) or demonstrably present in its routed store.

# Prefix table state. _POG_PREFIX_KIND[prefix] is "local" (the current
# store) or "routed"; _POG_PREFIX_STORE[prefix] holds the routed store dir.
# _POG_PREFIX_ARR lists the prefixes for iteration — associative-array KEY
# iteration is not portable across bash and zsh (both source this file), but
# iterating an indexed array and subscript reads are.
_POG_TABLE_BUILT=0

# _pog_prefix_valid <prefix>: rc 0 iff <prefix> is ^[a-z0-9]+(-[a-z0-9]+)*$
# — the shape safe to match branch tokens against.
_pog_prefix_valid() {
    printf '%s' "$1" | grep -qE '^[a-z0-9]+(-[a-z0-9]+)*$'
}

# _pog_table_add <prefix> <route_path>: record one route; the first entry
# for a prefix wins, later ones (routes lines, config aliases) are ignored.
# NOTE: the parameter is route_path, never path — zsh ties lowercase $path
# to $PATH, so a local named path would clobber command lookup for every
# helper called below (ga-f59876 zsh run).
_pog_table_add() {
    local prefix="$1" route_path="$2"
    if [[ -n "${_POG_PREFIX_KIND[$prefix]:-}" ]]; then
        return 0
    fi
    if [[ "$route_path" == "." ]]; then
        _POG_PREFIX_KIND[$prefix]="local"
    else
        _POG_PREFIX_KIND[$prefix]="routed"
        _POG_PREFIX_STORE[$prefix]="$_POG_ROUTE_BASE/$route_path/.beads"
    fi
    _POG_PREFIX_ARR+=("$prefix")
}

# _pog_build_prefix_table: build the prefix table once per guard run. D is
# $BEADS_DIR when set (what bd itself honors), else <git top level>/.beads;
# the route base B is D's parent, because route paths are relative to the
# scope root owning D. A missing or unparsable routes file leaves just the
# built-in ga entry; an invalid line is skipped with one WARNING so one bad
# entry never disables the rest.
_pog_build_prefix_table() {
    if [[ "$_POG_TABLE_BUILT" == "1" ]]; then
        return 0
    fi
    _POG_TABLE_BUILT=1
    typeset -gA _POG_PREFIX_KIND _POG_PREFIX_STORE
    _POG_PREFIX_ARR=()
    local d="" top
    if [[ -n "${BEADS_DIR:-}" ]]; then
        d="$BEADS_DIR"
    else
        top="$(git rev-parse --show-toplevel 2>/dev/null || true)"
        if [[ -n "$top" ]]; then
            d="$top/.beads"
        fi
    fi
    if [[ -n "$d" ]]; then
        _POG_ROUTE_BASE="$(dirname -- "$d")"
    else
        _POG_ROUTE_BASE=""
    fi
    local route_lines=""
    if [[ -n "$d" && -f "$d/routes.jsonl" ]]; then
        route_lines="$(jq -rR 'fromjson? | select(type == "object")
            | select(.prefix != null and .path != null)
            | [(.prefix | tostring), (.path | tostring)] | @tsv' "$d/routes.jsonl" 2>/dev/null || true)"
    fi
    if [[ -n "$route_lines" ]]; then
        # route_path, not path: zsh ties $path to $PATH (see _pog_table_add).
        local prefix route_path
        while IFS=$'\t' read -r prefix route_path; do
            # gc writes dashless prefixes ("mc"); bd-style routes write the
            # dashed form ("mc-"). Strip one trailing dash, not more: the
            # entry meant exactly one.
            prefix="${prefix%-}"
            if [[ -z "$route_path" ]] || ! _pog_prefix_valid "$prefix"; then
                echo "push-ownership-guard: WARNING routes.jsonl entry with prefix '${prefix}' or its path is invalid (need ^[a-z0-9]+(-[a-z0-9]+)*\$ and a non-empty path); ignoring that entry, the others still apply" >&2
                continue
            fi
            _pog_table_add "$prefix" "$route_path"
        done <<<"$route_lines"
    fi
    # Built-in contract: the local store's own ga- prefix always resolves,
    # exactly as it did before this table existed.
    if [[ -z "${_POG_PREFIX_KIND[ga]:-}" ]]; then
        _pog_table_add "ga" "."
    fi
}

# _pog_scan_candidates <branch>: print the bead-id-shaped tokens in
# <branch>, one per line, in branch order, deduplicated. A token starts at
# the beginning of the name or right after any non-alphanumeric character
# (so "omega-checker" never yields ga-checker), continues with a configured
# prefix, a "-", an optional mol/wisp infix, at least three id characters
# and any number of dotted sub-bead suffixes — the local id grammar,
# generalized over prefixes. The longest configured prefix wins where
# several overlap at one position, and EVERY boundary is tried (unlike a
# single grep -o pass, which consumes "bd-zoo" and never sees "zoo-k2p9x").
# Character-level matching in awk, not regex intervals: keeps the scanner
# portable across awk implementations.
_pog_scan_candidates() {
    awk -v prefixes="${_POG_PREFIX_ARR[*]}" '
        BEGIN {
            np = split(prefixes, P, " ")
            # sort prefixes longest-first so the longest wins on overlap
            for (i = 2; i <= np; i++) {
                v = P[i]
                for (j = i - 1; j >= 1 && length(P[j]) < length(v); j--) P[j+1] = P[j]
                P[j+1] = v
            }
        }
        {
            s = $0; n = length(s)
            for (i = 1; i <= n; i++) {
                if (i > 1 && substr(s, i-1, 1) ~ /[0-9A-Za-z]/) continue
                tok = pog_match_at(s, i, n)
                if (tok != "" && !(tok in seen)) { seen[tok] = 1; print tok }
            }
        }
        function pog_match_at(s, i, n,    j, p, e) {
            for (j = 1; j <= np; j++) {
                p = P[j] "-"
                if (substr(s, i, length(p)) != p) continue
                e = pog_tail_end(s, i + length(p), n)
                if (e > 0) return substr(s, i, e - i + 1)
            }
            return ""
        }
        # pog_tail_end(s, i, n): index of the last char of the id tail that
        # starts at i, or 0 when no tail matches: optional mol-/wisp- infix,
        # >=3 chars of [0-9a-z], then repeated "." digits groups.
        function pog_tail_end(s, i, n,    j, k, m2) {
            if (substr(s, i, 4) == "mol-") i += 4
            else if (substr(s, i, 5) == "wisp-") i += 5
            j = i
            while (j <= n && substr(s, j, 1) ~ /[0-9a-z]/) j++
            if (j - i < 3) return 0
            k = j - 1
            while (k < n && substr(s, k+1, 1) == ".") {
                m2 = k + 2
                if (m2 > n) break
                while (m2 <= n && substr(s, m2, 1) ~ /[0-9]/) m2++
                if (m2 == k + 2) break
                k = m2 - 1
            }
            return k
        }
    ' <<<"$1"
}

# _pog_token_prefix <token>: the longest configured prefix <token> starts
# with; "" when none (impossible for scanner output, defensive only).
_pog_token_prefix() {
    local tok="$1" best="" p
    for p in "${_POG_PREFIX_ARR[@]}"; do
        if [[ "$tok" == "$p"-* ]] && (( ${#p} > ${#best} )); then
            best="$p"
        fi
    done
    printf '%s' "$best"
}

# _pog_routed_bd <store> <args...>: run `bd <args...>` with BEADS_DIR pointed
# at a routed store, under the same timeout/retry budget as every other bd
# read. _pog_timeout can only exec an external command, so a shell function
# or cd cannot carry the redirection; env(1) is the argv prefix.
_pog_routed_bd() {
    local store="$1"
    shift
    _pog_read_with_retry env "BEADS_DIR=$store" bd "$@"
}

# _pog_block_routed_missing_store <token> <prefix> <store>: the routed-block
# message for a route whose target .beads directory is absent. Printed where
# the walk discovers it; names the token, the route target and the remedies.
_pog_block_routed_missing_store() {
    echo "push-ownership-guard: BLOCKED — branch token $1 (prefix $2) routes to store directory $3 (from .beads/routes.jsonl) which does not exist, so ownership of $1 cannot be verified. Fix that store or .beads/routes.jsonl, or rename the branch if $1 is not a bead id. Last resort: git push --no-verify" >&2
}

# _pog_block_routed_probe_failed <token> <prefix> <store>: the routed-block
# message for a probe that never produced a definitive JSON array (non-zero
# exit, timeout, empty or non-array output) within the retry budget.
_pog_block_routed_probe_failed() {
    echo "push-ownership-guard: BLOCKED — reading routed store $3 for branch token $1 (prefix $2, from .beads/routes.jsonl) failed after $POG_READ_ATTEMPTS attempts, so ownership of $1 cannot be verified; re-run the push first — if it keeps failing, bd/Dolt or that store needs attention. Fix that store or .beads/routes.jsonl, or rename the branch if $1 is not a bead id. Last resort: git push --no-verify" >&2
}

# _pog_walk_branch_tokens <branch> <allow_probes>: resolve the branch-derived
# bead id AND the store it must be read from, walking the candidate tokens
# in branch order:
#   - a LOCAL token (routes to ".") resolves immediately with no I/O — the
#     final verify still blocks on absence, exactly as the pre-routes guard
#     did;
#   - a ROUTED token resolves only after an exact-id probe of its store
#     (bd list --id=<that store's tokens in this branch> --all ... --json, one
#     batched probe per store) finds it present; an absent token is a
#     definitive answer and is skipped without retrying; a store that does
#     not exist or cannot be read blocks the walk (rc 2, reason on stderr);
#   - allow_probes=0 (deploy/*-gate branches, whose embedded id is the gated
#     bead, not this push's) never probes: routed tokens are skipped so that
#     branch shape keeps exactly its pre-routes reads.
# Sets _pog_walk_id / _pog_walk_store ("" store = the current one) and
# returns 0, or returns 2 after printing a BLOCKED reason.
_pog_walk_branch_tokens() {
    _pog_walk_id=""
    _pog_walk_store=""
    _pog_build_prefix_table
    local branch="$1" allow_probes="$2"
    local -a tokens=()
    local tok
    while IFS= read -r tok; do
        [[ -n "$tok" ]] && tokens+=("$tok")
    done < <(_pog_scan_candidates "$branch")
    if (( ${#tokens[@]} == 0 )); then
        return 0
    fi

    # Group the routed tokens by store so each store is probed once, with
    # all of its tokens in one batched --id= filter.
    typeset -A store_ids
    local p kind store
    for tok in "${tokens[@]}"; do
        p="$(_pog_token_prefix "$tok")"
        [[ -n "$p" ]] || continue
        kind="${_POG_PREFIX_KIND[$p]:-}"
        [[ "$kind" == "routed" ]] || continue
        store="${_POG_PREFIX_STORE[$p]:-}"
        [[ -n "$store" ]] || continue
        if [[ -n "${store_ids[$store]:-}" ]]; then
            store_ids[$store]="${store_ids[$store]},$tok"
        else
            store_ids[$store]="$tok"
        fi
    done

    typeset -A probed_ids
    local -a probed_stores=()
    local already s2 probe_json
    for tok in "${tokens[@]}"; do
        p="$(_pog_token_prefix "$tok")"
        [[ -n "$p" ]] || continue
        if [[ "${_POG_PREFIX_KIND[$p]:-}" == "local" ]]; then
            _pog_walk_id="$tok"
            _pog_walk_store=""
            return 0
        fi
        store="${_POG_PREFIX_STORE[$p]:-}"
        [[ -n "$store" ]] || continue
        if [[ "$allow_probes" != "1" ]]; then
            continue
        fi
        already=0
        for s2 in "${probed_stores[@]}"; do
            if [[ "$s2" == "$store" ]]; then already=1; break; fi
        done
        if (( already == 0 )); then
            if ! [[ -d "$store" ]]; then
                _pog_block_routed_missing_store "$tok" "$p" "$store"
                return 2
            fi
            if ! probe_json="$(_pog_routed_bd "$store" list --id="${store_ids[$store]}" --all --include-infra --include-ephemeral --include-gates --include-templates --flat --json)" \
                || [[ -z "$probe_json" ]] \
                || ! jq -e 'type == "array"' <<<"$probe_json" >/dev/null 2>&1; then
                _pog_block_routed_probe_failed "$tok" "$p" "$store"
                return 2
            fi
            probed_ids[$store]="$(jq -r '.[].id // empty' <<<"$probe_json" 2>/dev/null || true)"
            probed_stores+=("$store")
        fi
        if grep -Fx -- "$tok" <<<"${probed_ids[$store]}" >/dev/null 2>&1; then
            _pog_walk_id="$tok"
            _pog_walk_store="$store"
            return 0
        fi
        # Absent from its store: a definitive answer — skip, keep walking.
    done
    return 0
}

# _pog_returned_bead_id <json>: prints the id of the bead an already-fetched
# `bd show <id> --json` payload describes (.[0].id), or nothing when that id
# is missing, empty or not a string. `bd show` falls back to substring
# matching when no bead has the exact id, so a successful read can describe a
# different bead; callers compare this against the id they asked for before
# trusting any other field of the payload.
_pog_returned_bead_id() {
    jq -r '.[0]?.id? | strings' <<<"$1" 2>/dev/null || true
}

# _pog_branch_id_bead_inactive <id> [store]: true (rc 0) only if a FRESH bd
# show returns <id> itself and confirms its status is neither in_progress nor
# open -- i.e. the branch-derived bead is no longer the live claim. A
# non-empty <store> reads the id from that routed store instead of the
# current one. Fails safe: any read/parse failure, or a response for any
# other id, returns 1 (treat as still-active / not confirmed inactive), so
# an unreachable bd never triggers the branch-reuse override below -- it
# just falls through to the existing branch-id-wins path, which itself
# blocks safely on the same failure via assert_bead_still_claimed's own
# read.
_pog_branch_id_bead_inactive() {
    local id="$1" store="${2:-}"
    local json
    if [[ -n "$store" ]]; then
        json="$(_pog_routed_bd "$store" show "$id" --json)" || return 1
    else
        json="$(_pog_read_with_retry bd show "$id" --json)" || return 1
    fi
    [[ -n "$json" ]] || return 1
    jq -e '.' <<<"$json" >/dev/null 2>&1 || return 1
    [[ "$(_pog_returned_bead_id "$json")" == "$id" ]] || return 1
    local st
    st="$(jq -r '.[0].status // empty' <<<"$json" 2>/dev/null || true)"
    [[ "$st" != "in_progress" && "$st" != "open" && -n "$st" ]]
}

# _pog_ownership_violation <json>: pure predicate over an already-fetched
# `bd show <id> --json` payload (parsed here, not re-read). Prints nothing
# and returns 0 when the bead reads as currently owned by this session;
# otherwise prints a short, undecorated reason and returns 1. Checks, in
# order: status open/in_progress, assignee among this session's live
# identities, gc.routed_to unset-or-matching, no hold:mayor/hold:external
# label. Shared by assert_bead_still_claimed (the public push gate, which
# wraps the reason into its own BLOCKED/--no-verify message below) and
# _pog_resolve_bead_id's undeclared-single-match tier (ga-0ywrmy.1, which
# discards the reason and uses only the pass/fail to decide whether an
# undeclared in-progress bead may stand in for a closed branch-derived one).
#
# NOTE: never name a local here 'status' — it is a zsh special parameter
# (linked to $?, alongside $pipestatus) and this file is sourced into the
# deployer's ambient zsh shell (ga-xi7wi6); binding a local named 'status'
# there is a read-only-variable error, not a shadow.
_pog_ownership_violation() {
    local json="$1"
    local bead_status assignee routed_to labels
    bead_status="$(jq -r '.[0].status // empty' <<<"$json")"
    assignee="$(jq -r '.[0].assignee // empty' <<<"$json")"
    routed_to="$(jq -r '.[0].metadata."gc.routed_to" // empty' <<<"$json")"
    labels="$(jq -r '.[0].labels[]? // empty' <<<"$json")"

    if [[ "$bead_status" != "in_progress" && "$bead_status" != "open" ]]; then
        printf "status is '%s', not in_progress/open; the claim behind this push is stale" "$bead_status"
        return 1
    fi

    local -a _pog_identities=()
    local _pog_ident
    for _pog_ident in "${GC_SESSION_NAME:-}" "${GC_SESSION_ID:-}" "${GC_ALIAS:-}" "${GC_AGENT:-}" "${GC_TEMPLATE:-}"; do
        [[ -n "$_pog_ident" ]] && _pog_identities+=("$_pog_ident")
    done
    if [[ ${#_pog_identities[@]} -gt 0 ]]; then
        local _pog_owned=0
        for _pog_ident in "${_pog_identities[@]}"; do
            if [[ -n "$assignee" && "$assignee" == "$_pog_ident" ]]; then _pog_owned=1; break; fi
        done
        if [[ $_pog_owned -eq 0 ]]; then
            printf "assignee is '%s', not any current-session identity (%s); it was reassigned since this push began" "$assignee" "${_pog_identities[*]}"
            return 1
        fi
    fi

    if [[ -n "${GC_TEMPLATE:-}" && -n "$routed_to" && "$routed_to" != "$GC_TEMPLATE" ]]; then
        printf "gc.routed_to is '%s', not this session's config identity (%s); it was rerouted since this push began" "$routed_to" "$GC_TEMPLATE"
        return 1
    fi

    if grep -qx 'hold:mayor' <<<"$labels"; then
        printf 'is held (hold:mayor); a mayor ruling is pending'
        return 1
    fi
    if grep -qx 'hold:external' <<<"$labels"; then
        printf 'is held (hold:external)'
        return 1
    fi

    return 0
}

# _pog_resolve_bead_id: prints "<id><TAB><store>" — the bead id this push
# should be checked against and the store it must be read from (empty store
# = the current one) — or nothing if no id can be resolved. Resolution
# order:
#   1. The current branch name, scanned for bead-id tokens under EVERY
#      configured prefix (see _pog_scan_candidates and the prefix table):
#      the local ga- grammar — optional mol/wisp infix, three or more id
#      characters, repeated dotted sub-bead suffixes, read only at the
#      start of the name or after a non-alphanumeric separator so a word
#      such as `omega-checker` is not read as the bead ga-checker — plus
#      every prefix .beads/routes.jsonl routes to another store (ga-f59876:
#      the ga-only scan let a revoked writer push a cross-rig bead's branch
#      as "nothing to check"). The walk resolves the first token that is
#      local (no I/O) or present in its routed store (one exact-id
#      bd list --id= probe per store, batched over that store's tokens);
#      absent tokens are skipped, unreadable or missing stores block. On
#      deploy/*-gate branches the walk never probes (the embedded id is the
#      gated bead, not this push's — see below), so only local tokens can
#      resolve there.
#   2. Falls back to this session's in-progress assignment
#      (bd list --assignee="$GC_AGENT" --status=in_progress --json) when
#      the branch name doesn't match. Only resolves when that query returns
#      EXACTLY one bead — two or more in-progress beads for this session is
#      not a positive identification of which one (if any) this push is
#      for, so it resolves the same as finding none (ga-1qepfl mechanism 2:
#      an unrelated in-progress bead, e.g. one correctly held open for its
#      own merge-tracking, was blocking pushes for a completely different
#      deploy/*-gate branch because path 2 took .[0] of the match list
#      unconditionally). This single-match requirement applies to EVERY
#      branch shape that falls through to the fallback, not only
#      deploy/*-gate — a non-gate branch that encodes no bead id and a
#      session holding 2+ in-progress beads now resolves to nothing and is
#      not checked at all. Deliberate: .[0] of an unordered multi-match was
#      never sound enforcement in either direction. Pinned by
#      test_bead_id_general_branch_ignores_ambiguous_inprogress_beads.
# If both resolve and disagree, the branch match wins (it's the more
# specific signal) and a warning goes to stderr — this is a best-effort
# cross-check, not a hard failure, since branch-naming habits can
# legitimately drift from bd's bookkeeping. EXCEPTION: deploy/*-gate
# branches (see below) embed the id of the bead being gated, not the bead
# this push is for, so for that branch shape the live assignee wins instead.
# EXCEPTION (branch reuse, ga-bf39j8): a branch name can be reused across a
# sequence of beads — a predecessor bead closes and a live successor bead
# continues the identical work on the identical branch name. When the
# branch-derived bead is confirmed no longer live (a fresh bd show — from
# ITS store — shows it's not in_progress/open) AND a DIFFERENT in-progress
# bead in this session's own assignment list declares itself that bead's
# continuation via metadata.branch (matching the literal branch name) or
# metadata.build_bead (matching the branch-derived id), that successor id
# wins instead. This is an explicit declared-link check, not a blind "first
# in-progress bead" pick — a session identity routinely holds several
# unrelated in-progress beads at once, so picking one without a relational
# check would validate this push against a totally unrelated concurrent
# task.
#
# KNOWN LIMITATION of path 2 (confirmed by manual repro, not yet filed as
# its own bead): the fallback query itself filters on --status=in_progress,
# so it cannot find a bead that has already left in_progress (e.g. closed
# by the exact mayor ruling this guard exists to catch) by the time the
# fallback runs. In that narrow intersection — branch name doesn't encode
# the bead id AND the status flip lands before this resolves — no id
# resolves at all, and assert_bead_still_claimed's "nothing to check"
# branch below allows the push. This does NOT affect path 1: this repo's
# real branch convention (builder/<bead-id>-<slug>) always encodes the
# bead id, so the primary path is unaffected by a bead's status changing
# out from under it — with one deliberate exception: deploy/*-gate branches
# now route through path 2 by design (their branch-embedded id is the gated
# bead, not this push's bead), so that branch shape inherits this gap.
# Confirmed via manual repro, see
# test_fallback_cannot_detect_staleness_after_status_leaves_in_progress in
# scripts/test-push-ownership-guard.sh. The fallback query shape matches
# ga-fip9ps.1's own spec verbatim; widening it further (e.g. dropping the
# status filter) would trade this gap for ambiguous multi-match resolution
# against an agent's whole bead history, which is a real design decision for
# whoever owns that tradeoff, not a mechanical fix — left for a follow-up
# bead. (The narrower multi-match case that --status=in_progress alone can
# already produce — more than one bead simultaneously in_progress under the
# same assignee — is handled: see the single-match requirement on path 2
# above, ga-1qepfl.)
# Prints nothing (not an error) when neither resolves — the caller treats
# that as "nothing to check," which is what lets Layer A wire this in
# unconditionally without blocking every push in the repo.
_pog_resolve_bead_id() {
    local branch=""
    branch="$(git symbolic-ref --short HEAD 2>/dev/null || git branch --show-current 2>/dev/null || true)"

    local branch_id="" branch_store=""
    if [[ -n "$branch" ]]; then
        local allow_probes=1
        if [[ "$branch" == deploy/*-gate ]]; then
            allow_probes=0
        fi
        _pog_walk_branch_tokens "$branch" "$allow_probes"
        local walk_rc=$?
        if [[ "$walk_rc" == "2" ]]; then
            printf '%s' "$POG_ROUTED_READ_SENTINEL"
            return
        fi
        branch_id="$_pog_walk_id"
        branch_store="$_pog_walk_store"
    fi

    # assignee_read_failed distinguishes "the read failed" (ambiguity) from
    # "the read succeeded and found nothing" (a clean answer):
    # _pog_read_with_retry returns non-zero only when every attempt failed or
    # produced no output, and a successful `[]` read is non-empty, so its exit
    # status separates the two cleanly.
    local assignee_id=""
    local assignee_read_failed=0
    if [[ -n "${GC_AGENT:-}" ]]; then
        if ! command -v bd >/dev/null 2>&1; then
            assignee_read_failed=1
        else
            local list_json
            if list_json="$(_pog_read_with_retry bd list --assignee="$GC_AGENT" --status=in_progress --json)"; then
                # A single in-progress bead is a positive identity signal;
                # two or more is not -- picking .[0] from an unordered
                # multi-match would silently assert ownership of whichever
                # bead the query happens to list first, which is not
                # evidence it corresponds to THIS push (ga-1qepfl: an
                # unrelated in-progress bead, e.g. one correctly held open
                # for its own merge-tracking, blocked pushes for a
                # completely different deploy/*-gate branch). Leaving
                # assignee_id empty here is a clean, successful read that
                # found no single answer -- the same "nothing to check"
                # outcome as finding zero, not the read-failure ambiguity
                # assignee_read_failed exists to catch.
                local assignee_count
                assignee_count="$(jq -r 'length' <<<"$list_json" 2>/dev/null || echo 0)"
                if [[ "$assignee_count" == "1" ]]; then
                    assignee_id="$(jq -r '.[0].id // empty' <<<"$list_json" 2>/dev/null || true)"
                fi
            else
                assignee_read_failed=1
            fi
        fi
    fi

    # deploy/*-gate branches embed the id of the bead being GATED, not the
    # bead this push is for -- that gated bead is routinely closed by the
    # time its deploy-gate branch is pushed (that's the whole point of a
    # deploy gate: ga-wwswme). For this branch shape the live in-progress
    # assignment is the correct id and must win over the branch-derived id.
    if [[ "$branch" == deploy/*-gate ]]; then
        if [[ -n "$branch_id" && -n "$assignee_id" && "$branch_id" != "$assignee_id" ]]; then
            echo "push-ownership-guard: NOTE deploy-gate branch resolves to $branch_id (the gated bead, not this push's bead); using this session's in-progress assignment $assignee_id instead" >&2
        fi
        # Discarding the branch-derived id means the assignee read is the ONLY
        # signal left for this branch shape, so a failed read is ambiguity, not
        # "nothing to check" — hand the caller the sentinel so it fails closed.
        if [[ -z "$assignee_id" && $assignee_read_failed -eq 1 ]]; then
            printf '%s' "$POG_AMBIGUOUS_SENTINEL"
            return
        fi
        printf '%s' "$assignee_id"
        return
    fi

    # BRANCH REUSE (ga-bf39j8): see the EXCEPTION note above the function
    # doc comment. Look for a live in-progress bead that explicitly declares
    # itself the branch-derived bead's continuation, but only act on it once
    # the branch-derived bead is confirmed no longer live — an explicit link
    # to a still-active bead is not this guard's problem to resolve, it just
    # falls through to the ordinary disagreement warning below.
    local successor_id=""
    if [[ -n "$branch_id" ]]; then
        successor_id="$(jq -r --arg br "$branch" --arg bid "$branch_id" \
            '[.[] | select(.metadata.branch == $br or .metadata.build_bead == $bid)][0].id // empty' \
            <<<"${list_json:-[]}" 2>/dev/null || true)"
    fi
    if [[ -n "$successor_id" && "$successor_id" != "$branch_id" ]] && _pog_branch_id_bead_inactive "$branch_id" "$branch_store"; then
        echo "push-ownership-guard: NOTE branch $branch was reused after $branch_id closed; this session's in-progress $successor_id declares itself that bead's continuation (metadata.branch/build_bead), using $successor_id instead" >&2
        printf '%s\t%s' "$successor_id" ""
        return
    fi

    # UNDECLARED SINGLE MATCH (ga-0ywrmy.1): reached only once the declared-
    # successor check above found nothing. Requires the SAME freshness gate
    # (branch-derived bead confirmed inactive) plus one more condition: this
    # session's in-progress list (already fetched above, not re-queried) has
    # EXACTLY one entry -- no declared link required, cardinality alone is
    # the safety bound, mirroring path 2's own single-match discipline. The
    # sole candidate still has to pass a fresh live-ownership check below; an
    # unrelated bead that happens to be this session's only other
    # in-progress claim but reads back not-owned (closed, reassigned, held,
    # ...) on its own fresh bd show must still not validate this push -- this
    # tier only ever widens *resolution*, never *verification*.
    #
    # Cheap conditions (branch_id present, exactly one candidate already sitting
    # in ${list_json} from the assignee-fallback fetch above, that candidate
    # isn't literally branch_id) are checked FIRST, purely from data already in
    # hand -- no I/O. _pog_branch_id_bead_inactive is deliberately the LAST
    # link in this && chain, short-circuited so it only runs (and only pays for
    # its own bd show + retries) once every cheap prerequisite already holds.
    # The ordinary push -- branch_id resolves, no reuse in play at all -- must
    # cost exactly the one bd show that assert_bead_still_claimed's own final
    # check already makes; putting the I/O check first would double that cost
    # on every push through this guard, not just the rare reuse case (caught
    # by test_retry_exhausted_still_blocks's exact-call-count assertion).
    if [[ -n "$branch_id" ]]; then
        local undeclared_count
        undeclared_count="$(jq -r 'length' <<<"${list_json:-[]}" 2>/dev/null || echo 0)"
        if [[ "$undeclared_count" == "1" ]]; then
            local undeclared_id
            undeclared_id="$(jq -r '.[0].id // empty' <<<"${list_json:-[]}" 2>/dev/null || true)"
            if [[ -n "$undeclared_id" && "$undeclared_id" != "$branch_id" ]] && _pog_branch_id_bead_inactive "$branch_id" "$branch_store"; then
                local undeclared_json
                if undeclared_json="$(_pog_read_with_retry bd show "$undeclared_id" --json)" \
                    && [[ -n "$undeclared_json" ]] \
                    && jq -e '.' <<<"$undeclared_json" >/dev/null 2>&1 \
                    && _pog_ownership_violation "$undeclared_json" >/dev/null 2>&1; then
                    echo "push-ownership-guard: NOTE branch $branch was reused after $branch_id closed; this session has exactly one other in-progress bead ($undeclared_id) with no declared continuation link, using it as the undeclared single match" >&2
                    printf '%s\t%s' "$undeclared_id" ""
                    return
                fi
            fi
        fi
    fi

    if [[ -n "$branch_id" && -n "$assignee_id" && "$branch_id" != "$assignee_id" ]]; then
        echo "push-ownership-guard: WARNING branch name resolves to $branch_id but this session's in-progress assignment is $assignee_id; using $branch_id (branch name wins)" >&2
    fi

    if [[ -n "$branch_id" ]]; then
        printf '%s\t%s' "$branch_id" "$branch_store"
    else
        printf '%s\t%s' "$assignee_id" ""
    fi
}

# assert_bead_still_claimed: the exported guard. Returns 0 to allow the
# push, non-zero to block it. See file header for the full contract.
assert_bead_still_claimed() {
    if [[ "${POG_DISABLE:-0}" == "1" ]]; then
        return 0
    fi

    local id_store id store tab=$'\t'
    id_store="$(_pog_resolve_bead_id)"
    if [[ "$id_store" == *"$tab"* ]]; then
        id="${id_store%%"$tab"*}"
        store="${id_store#*"$tab"}"
    else
        id="$id_store"
        store=""
    fi
    if [[ "$id" == "$POG_AMBIGUOUS_SENTINEL" ]]; then
        echo "push-ownership-guard: BLOCKED — deploy-gate branch: could not read this session's in-progress assignment (bd unreachable or not on PATH), so ownership cannot be verified; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        return 1
    fi
    if [[ "$id" == "$POG_ROUTED_READ_SENTINEL" ]]; then
        # _pog_walk_branch_tokens already printed the BLOCKED reason (token,
        # prefix, store, remedies) where the failure happened.
        return 1
    fi
    if [[ -z "$id" ]]; then
        return 0  # nothing to check
    fi

    if ! command -v bd >/dev/null 2>&1; then
        echo "push-ownership-guard: BLOCKED — bd is not on PATH, cannot verify $id is still claimed. Bypass with: git push --no-verify" >&2
        return 1
    fi

    local json
    if [[ -n "$store" ]]; then
        json="$(_pog_routed_bd "$store" show "$id" --json)"
    else
        json="$(_pog_read_with_retry bd show "$id" --json)"
    fi
    if [[ -z "$json" ]]; then
        if [[ -n "$store" ]]; then
            echo "push-ownership-guard: BLOCKED — bd show $id failed after $POG_READ_ATTEMPTS attempts against routed store $store (prefix from .beads/routes.jsonl; the bead may be absent from that store, or bd/Dolt may be unreachable), so ownership cannot be verified; re-run the push first — if it keeps failing, bd/Dolt or that store needs attention. Fix that store or .beads/routes.jsonl, or rename the branch if $id is not a bead id. Last resort: git push --no-verify" >&2
        else
            echo "push-ownership-guard: BLOCKED — bd show $id failed after $POG_READ_ATTEMPTS attempts (the bead may be absent from this store, or bd/Dolt may be unreachable), so ownership cannot be verified; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        fi
        return 1
    fi
    if ! jq -e '.' <<<"$json" >/dev/null 2>&1; then
        echo "push-ownership-guard: BLOCKED — bd show $id --json returned unparseable output; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        return 1
    fi

    local returned_id
    returned_id="$(_pog_returned_bead_id "$json")"
    if [[ -z "$returned_id" ]]; then
        echo "push-ownership-guard: BLOCKED — bd show $id returned no bead id, so ownership of $id cannot be verified; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        return 1
    fi
    if [[ "$returned_id" != "$id" ]]; then
        echo "push-ownership-guard: BLOCKED — bd show $id returned bead $returned_id, not $id (no exact match; bd matched another bead), so ownership of $id cannot be verified. Bypass with: git push --no-verify" >&2
        return 1
    fi

    local reason
    if ! reason="$(_pog_ownership_violation "$json")"; then
        echo "push-ownership-guard: BLOCKED — $id $reason. Bypass with: git push --no-verify" >&2
        return 1
    fi
    return 0
}
