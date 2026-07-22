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
# GC_SESSION_ID, GC_ALIAS, GC_AGENT — mirroring the claim path), still
# routed to this session's config identity, and not held by the mayor or an
# external actor.
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
# This file ONLY defines functions and one default-value assignment;
# sourcing it must not produce output or otherwise mutate state.
#
# Set POG_DISABLE=1 to short-circuit assert_bead_still_claimed to a bare
# `return 0`. This exists for test harnesses that call
# attempt_bounded_self_rebase directly against synthetic repos with no real
# bead behind them (e.g. scripts/test-rebase-resolve.sh) and must stay
# hermetic — it is not meant to be set on a real push path.

POG_TIMEOUT_SECONDS="${POG_TIMEOUT_SECONDS:-5}"

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

# _pog_resolve_bead_id: prints the bead id this push should be checked
# against; prints nothing if none can be resolved. Resolution order:
#   1. The current branch name, matched against ga-[0-9a-z]{6}(\.[0-9]+)? —
#      the bead's own id format, extended with an optional sub-bead suffix
#      because this repo's real branch convention is
#      builder/<bead-id>-<slug> and sub-beads (e.g. ga-fip9ps.1) are
#      routine; the literal 6-char-only pattern would misresolve to the
#      parent bead on a sub-bead's own branch.
#   2. Falls back to this session's single in-progress assignment
#      (bd list --assignee="$GC_AGENT" --status=in_progress --json) when
#      the branch name doesn't match.
# If both resolve and disagree, the branch match wins (it's the more
# specific signal) and a warning goes to stderr — this is a best-effort
# cross-check, not a hard failure, since branch-naming habits can
# legitimately drift from bd's bookkeeping.
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
# out from under it — confirmed via manual repro, see
# test_fallback_cannot_detect_staleness_after_status_leaves_in_progress in
# scripts/test-push-ownership-guard.sh. The fallback query shape matches
# ga-fip9ps.1's own spec verbatim; widening it (e.g. dropping the status
# filter) trades this gap for ambiguous multi-match resolution against an
# agent's whole bead history, which is a real design decision for whoever
# owns that tradeoff, not a mechanical fix — left for a follow-up bead.
# Prints nothing (not an error) when neither resolves — the caller treats
# that as "nothing to check," which is what lets Layer A wire this in
# unconditionally without blocking every push in the repo.
_pog_resolve_bead_id() {
    local branch=""
    branch="$(git symbolic-ref --short HEAD 2>/dev/null || git branch --show-current 2>/dev/null || true)"

    local branch_id=""
    if [[ -n "$branch" ]]; then
        branch_id="$(grep -oE 'ga-[0-9a-z]{6}(\.[0-9]+)?' <<<"$branch" | head -1 || true)"
    fi

    local assignee_id=""
    if [[ -n "${GC_AGENT:-}" ]] && command -v bd >/dev/null 2>&1; then
        local list_json
        list_json="$(_pog_timeout "$POG_TIMEOUT_SECONDS" bd list --assignee="$GC_AGENT" --status=in_progress --json 2>/dev/null || true)"
        if [[ -n "$list_json" ]]; then
            assignee_id="$(jq -r '.[0].id // empty' <<<"$list_json" 2>/dev/null || true)"
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
    if [[ -z "$id" ]]; then
        return 0  # nothing to check
    fi

    if ! command -v bd >/dev/null 2>&1; then
        echo "push-ownership-guard: BLOCKED — bd is not on PATH, cannot verify $id is still claimed. Bypass with: git push --no-verify" >&2
        return 1
    fi

    local json
    if ! json="$(_pog_timeout "$POG_TIMEOUT_SECONDS" bd show "$id" --json 2>/dev/null)" || [[ -z "$json" ]]; then
        echo "push-ownership-guard: BLOCKED — bd show $id timed out or bd/Dolt is unreachable; cannot confirm $id is still claimed. Bypass with: git push --no-verify" >&2
        return 1
    fi
    if ! jq -e '.' <<<"$json" >/dev/null 2>&1; then
        echo "push-ownership-guard: BLOCKED — bd show $id --json returned unparseable output; cannot confirm $id is still claimed. Bypass with: git push --no-verify" >&2
        return 1
    fi

    local status assignee routed_to labels
    status="$(jq -r '.[0].status // empty' <<<"$json")"
    assignee="$(jq -r '.[0].assignee // empty' <<<"$json")"
    routed_to="$(jq -r '.[0].metadata."gc.routed_to" // empty' <<<"$json")"
    labels="$(jq -r '.[0].labels[]? // empty' <<<"$json")"

    if [[ "$status" != "in_progress" && "$status" != "open" ]]; then
        echo "push-ownership-guard: BLOCKED — $id status is '$status', not in_progress/open; the claim behind this push is stale. Bypass with: git push --no-verify" >&2
        return 1
    fi

    # A session-run claim sets bead.assignee from the first non-empty of
    # GC_SESSION_NAME, GC_SESSION_ID, GC_ALIAS, GC_AGENT (see
    # cmd/gc/cmd_hook.go's firstNonEmptyHookValue). Accept ANY of this
    # session's live identities — GC_AGENT alone falsely blocks a push whose
    # bead is legitimately assigned to the session name/id. Fail-closed
    # semantics preserved: with identities present, an assignee matching none
    # (including empty) still blocks.
    local -a _pog_identities=()
    local _pog_ident
    for _pog_ident in "${GC_SESSION_NAME:-}" "${GC_SESSION_ID:-}" "${GC_ALIAS:-}" "${GC_AGENT:-}"; do
        [[ -n "$_pog_ident" ]] && _pog_identities+=("$_pog_ident")
    done
    if [[ ${#_pog_identities[@]} -gt 0 ]]; then
        local _pog_owned=0
        for _pog_ident in "${_pog_identities[@]}"; do
            if [[ -n "$assignee" && "$assignee" == "$_pog_ident" ]]; then _pog_owned=1; break; fi
        done
        if [[ $_pog_owned -eq 0 ]]; then
            echo "push-ownership-guard: BLOCKED — $id assignee is '$assignee', not any current-session identity (${_pog_identities[*]}); it was reassigned since this push began. Bypass with: git push --no-verify" >&2
            return 1
        fi
    fi

    if [[ -n "${GC_TEMPLATE:-}" && -n "$routed_to" && "$routed_to" != "$GC_TEMPLATE" ]]; then
        echo "push-ownership-guard: BLOCKED — $id gc.routed_to is '$routed_to', not this session's config identity ($GC_TEMPLATE); it was rerouted since this push began. Bypass with: git push --no-verify" >&2
        return 1
    fi

    if grep -qx 'hold:mayor' <<<"$labels"; then
        echo "push-ownership-guard: BLOCKED — $id is held (hold:mayor); a mayor ruling is pending. Bypass with: git push --no-verify" >&2
        return 1
    fi
    if grep -qx 'hold:external' <<<"$labels"; then
        echo "push-ownership-guard: BLOCKED — $id is held (hold:external). Bypass with: git push --no-verify" >&2
        return 1
    fi

    return 0
}
