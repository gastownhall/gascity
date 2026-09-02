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

# _pog_branch_id_bead_inactive <id>: true (rc 0) only if a FRESH bd show
# confirms <id>'s status is neither in_progress nor open -- i.e. the branch-
# derived bead is no longer the live claim. Fails safe: any read/parse
# failure returns 1 (treat as still-active / not confirmed inactive), so an
# unreachable bd never triggers the branch-reuse override below -- it just
# falls through to the existing branch-id-wins path, which itself blocks
# safely on the same failure via assert_bead_still_claimed's own read.
_pog_branch_id_bead_inactive() {
    local id="$1"
    local json
    json="$(_pog_read_with_retry bd show "$id" --json)" || return 1
    [[ -n "$json" ]] || return 1
    jq -e '.' <<<"$json" >/dev/null 2>&1 || return 1
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

# _pog_resolve_bead_id: prints the bead id this push should be checked
# against; prints nothing if none can be resolved. Resolution order:
#   1. The current branch name, matched against ga-[0-9a-z]{6}(\.[0-9]+)* —
#      the bead's own id format, extended with zero or more repeated
#      sub-bead suffixes because this repo's real branch convention is
#      builder/<bead-id>-<slug> and sub-beads are routine at any nesting
#      depth: a single-level sub-bead (e.g. ga-fip9ps.1) as well as a
#      grandchild (e.g. ga-o3ko1j.4.3). The suffix group must repeat (`*`),
#      not just appear once (`?`) — a single optional group truncates a
#      grandchild id after its first dotted segment, misresolving to the
#      wrong (and possibly closed) parent/child bead instead of the actual
#      grandchild bead the branch is for. The literal 6-char-only pattern
#      would misresolve to the root bead on any sub-bead's own branch.
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
# branch-derived bead is confirmed no longer live (a fresh bd show shows it's
# not in_progress/open) AND a DIFFERENT in-progress bead in this session's
# own assignment list declares itself that bead's continuation via
# metadata.branch (matching the literal branch name) or metadata.build_bead
# (matching the branch-derived id), that successor id wins instead. This is
# an explicit declared-link check, not a blind "first in-progress bead"
# pick — a session identity routinely holds several unrelated in-progress
# beads at once, so picking one without a relational check would validate
# this push against a totally unrelated concurrent task.
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

    local branch_id=""
    if [[ -n "$branch" ]]; then
        branch_id="$(grep -oE 'ga-[0-9a-z]{6}(\.[0-9]+)*' <<<"$branch" | head -1 || true)"
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
    if [[ -n "$successor_id" && "$successor_id" != "$branch_id" ]] && _pog_branch_id_bead_inactive "$branch_id"; then
        echo "push-ownership-guard: NOTE branch $branch was reused after $branch_id closed; this session's in-progress $successor_id declares itself that bead's continuation (metadata.branch/build_bead), using $successor_id instead" >&2
        printf '%s' "$successor_id"
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
            if [[ -n "$undeclared_id" && "$undeclared_id" != "$branch_id" ]] && _pog_branch_id_bead_inactive "$branch_id"; then
                local undeclared_json
                if undeclared_json="$(_pog_read_with_retry bd show "$undeclared_id" --json)" \
                    && [[ -n "$undeclared_json" ]] \
                    && jq -e '.' <<<"$undeclared_json" >/dev/null 2>&1 \
                    && _pog_ownership_violation "$undeclared_json" >/dev/null 2>&1; then
                    echo "push-ownership-guard: NOTE branch $branch was reused after $branch_id closed; this session has exactly one other in-progress bead ($undeclared_id) with no declared continuation link, using it as the undeclared single match" >&2
                    printf '%s' "$undeclared_id"
                    return
                fi
            fi
        fi
    fi

    if [[ -n "$branch_id" && -n "$assignee_id" && "$branch_id" != "$assignee_id" ]]; then
        echo "push-ownership-guard: WARNING branch name resolves to $branch_id but this session's in-progress assignment is $assignee_id; using $branch_id (branch name wins)" >&2
    fi

    if [[ -n "$branch_id" ]]; then
        printf '%s' "$branch_id"
    else
        printf '%s' "$assignee_id"
    fi
}

# assert_bead_still_claimed: the exported guard. Returns 0 to allow the
# push, non-zero to block it. See file header for the full contract.
assert_bead_still_claimed() {
    if [[ "${POG_DISABLE:-0}" == "1" ]]; then
        return 0
    fi

    local id
    id="$(_pog_resolve_bead_id)"
    if [[ "$id" == "$POG_AMBIGUOUS_SENTINEL" ]]; then
        echo "push-ownership-guard: BLOCKED — deploy-gate branch: could not read this session's in-progress assignment (bd unreachable or not on PATH), so ownership cannot be verified; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
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
    if ! json="$(_pog_read_with_retry bd show "$id" --json)" || [[ -z "$json" ]]; then
        echo "push-ownership-guard: BLOCKED — bd show $id unreachable after $POG_READ_ATTEMPTS attempts; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        return 1
    fi
    if ! jq -e '.' <<<"$json" >/dev/null 2>&1; then
        echo "push-ownership-guard: BLOCKED — bd show $id --json returned unparseable output; re-run the push first — if it keeps failing, bd/Dolt needs attention. Last resort: git push --no-verify" >&2
        return 1
    fi

    local reason
    if ! reason="$(_pog_ownership_violation "$json")"; then
        echo "push-ownership-guard: BLOCKED — $id $reason. Bypass with: git push --no-verify" >&2
        return 1
    fi
    return 0
}
