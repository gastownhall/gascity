#!/usr/bin/env bash
#
# rebase-resolve-lib.sh — conservative trivial-conflict auto-resolution for
# the deployer's bounded self-rebase path (bead ga-gcy0cd; architecture
# ga-h7hnpt FR-5/FR-6).
#
# is_additive_keepboth_path, resolve_conflict_markers_in_file, and
# attempt_trivial_conflict_resolution below are a PORTED COPY of
# packs/maintainer-pr-review/scripts/rebase-resolve-lib.sh (byte-identical
# logic) — copied rather than shared, matching this codebase's established
# per-pack script-copy convention (e.g. worktree-setup.sh is independently
# copied into 15+ packs). There is no shared cross-pack lib directory; do NOT
# modify the maintainer-pr-review original to stay in sync with this file or
# vice versa, and do not import across packs. If the two copies drift and
# need a bugfix in lockstep repeatedly, that's a signal to revisit the
# no-shared-lib decision (see ga-h7hnpt Trade-offs) — not to improvise an
# import here.
#
# This file ONLY defines functions; sourcing it must not produce output or
# mutate state. It is sourced by the deployer's evaluate-gate step
# (formulas/mol-deployer-gate.formula.toml, prompts/deployer.md Guardrails)
# and by tests/test-rebase-resolve.sh.
#
# DESIGN — err toward routing, never toward a wrong auto-resolve.
#
# A wrong auto-resolve silently corrupts the branch, so the bar for
# "trivial" is deliberately high. We resolve a conflicted file ONLY when EVERY
# conflict hunk in it falls into one of three provably-safe shapes:
#
#   1. IDENTICAL  — both sides of the hunk are byte-identical. Take one side.
#                   (Common when the same change was cherry-picked onto both
#                    branches.)
#
#   2. ONE-SIDE-EMPTY — one side of the hunk is empty and the other adds lines.
#                   This is a pure addition on one branch against no change on
#                   the other. Take the non-empty side. (Two branches
#                   appending to different regions of the same file
#                   frequently surface as one-side-empty hunks after rebase.)
#
#   3. ADDITIVE-BOTH on an ALLOWLISTED additive file — both sides are non-empty
#                   and differ, AND the file path is a test / doc / fixture
#                   file (the only place "just keep both" is provably safe per
#                   the operator: "it's common that a and b both add tests and
#                   we just want both"). We KEEP BOTH sides (ours then theirs),
#                   dropping only the conflict markers. We do NOT do this for
#                   source code — concatenating two divergent code edits can
#                   produce a duplicate-definition or logic corruption.
#
# If a file contains ANY hunk that is none of the above (i.e. both sides are
# non-empty, differ, and the file is NOT an allowlisted additive file), the
# whole file is declared a REAL conflict and resolution fails — the caller
# aborts the rebase and routes to the builder.
#
# Required external commands: git, awk, cmp, mktemp.

# is_additive_keepboth_path <path>
#
# Returns 0 when <path> is a test / doc / fixture file for which concatenating
# both sides of an additive conflict ("keep both") is safe. Conservative: an
# unrecognized path returns 1 so the only auto-keep-both behavior is on files
# whose semantics are "a bag of independent entries" (tests, docs, fixtures,
# changelog/news fragments), never on importable source.
is_additive_keepboth_path() {
    local path="$1"
    # Normalize to lowercase for matching; keep original for extension checks.
    local lower="${path,,}"
    local base="${path##*/}"
    local lbase="${base,,}"

    # --- documentation ---
    case "$lbase" in
        *.md|*.mdx|*.rst|*.txt|*.adoc) return 0 ;;
    esac
    case "$lower" in
        docs/*|*/docs/*|doc/*|*/doc/*) return 0 ;;
        changelog*|*/changelog*|news/*|*/news/*|changes/*|*/changes/*) return 0 ;;
    esac

    # --- fixtures / golden / testdata ---
    case "$lower" in
        */fixtures/*|fixtures/*) return 0 ;;
        */testdata/*|testdata/*) return 0 ;;
        */__fixtures__/*) return 0 ;;
        */golden/*|golden/*|*.golden) return 0 ;;
        */snapshots/*|__snapshots__/*|*.snap) return 0 ;;
    esac

    # --- test source files (path- or name-based) ---
    case "$lower" in
        */tests/*|tests/*|*/test/*|test/*|*/__tests__/*) return 0 ;;
        spec/*|*/spec/*) return 0 ;;
    esac
    case "$lbase" in
        *_test.go|*_test.py|test_*.py|*.test.js|*.test.ts|*.test.jsx|*.test.tsx) return 0 ;;
        *.spec.js|*.spec.ts|*.spec.jsx|*.spec.tsx) return 0 ;;
        *_spec.rb|*_test.rb) return 0 ;;
        *.bats) return 0 ;;
        test-*.sh|test_*.sh|*-test.sh|*_test.sh) return 0 ;;
    esac

    return 1
}

# resolve_conflict_markers_in_file <path> <allow_additive_keepboth: 0|1>
#
# Reads a single working-tree file containing git conflict markers and writes
# the resolved content back IN PLACE, but ONLY if every hunk is trivially
# resolvable per the rules above. Returns:
#   0  — file fully resolved (no markers left); file rewritten in place.
#   1  — file contains a real (non-trivial) conflict; file left UNTOUCHED.
#   2  — usage / IO error (treated as non-trivial by callers).
#
# <allow_additive_keepboth> is passed by the caller after it has checked
# is_additive_keepboth_path on the file. Keeping the path check in the caller
# (and passing a bare flag) makes this function easy to unit-test with either
# policy independent of path heuristics.
#
# Implementation note: we do the parse/classify/rewrite in a single awk pass so
# the logic is auditable in one place. awk exits 0 (resolved) / 1 (real
# conflict) / 2 (malformed markers); we mirror that exit code.
resolve_conflict_markers_in_file() {
    local path="$1"
    local allow_keepboth="${2:-0}"
    [[ -f "$path" ]] || return 2

    local tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/gc-rebase-resolve.XXXXXX")" || return 2

    # awk state machine over conflict markers.
    #
    #   state 0: outside a conflict — copy lines through.
    #   state 1: inside "ours"  (after `<<<<<<<`, before `=======`).
    #   state 2: inside "theirs" (after `=======`, before `>>>>>>>`).
    #
    # A diff3-style merge can also emit a `|||||||` "base" section between ours
    # and `=======`. We track and discard the base section (it's not part of
    # either resolution). Its presence does not change triviality.
    #
    # For each completed hunk we decide:
    #   identical(ours,theirs)            -> emit ours
    #   ours empty, theirs non-empty      -> emit theirs
    #   theirs empty, ours non-empty      -> emit ours
    #   both empty                        -> emit nothing (degenerate; trivial)
    #   both non-empty & differ:
    #       allow_keepboth==1             -> emit ours then theirs (union)
    #       else                          -> REAL CONFLICT (exit 1)
    if awk -v allow_keepboth="$allow_keepboth" '
        function flush_hunk(   i, ours_n, theirs_n, identical) {
            ours_n = o_count
            theirs_n = t_count
            identical = 0
            if (ours_n == theirs_n) {
                identical = 1
                for (i = 1; i <= ours_n; i++) {
                    if (ours[i] != theirs[i]) { identical = 0; break }
                }
            }
            if (identical) {
                for (i = 1; i <= ours_n; i++) print ours[i]
            } else if (ours_n == 0 && theirs_n == 0) {
                # nothing to emit
            } else if (ours_n == 0) {
                for (i = 1; i <= theirs_n; i++) print theirs[i]
            } else if (theirs_n == 0) {
                for (i = 1; i <= ours_n; i++) print ours[i]
            } else {
                # both sides non-empty and differ.
                if (allow_keepboth == 1) {
                    for (i = 1; i <= ours_n; i++) print ours[i]
                    for (i = 1; i <= theirs_n; i++) print theirs[i]
                } else {
                    real_conflict = 1
                    exit 1
                }
            }
            # reset hunk buffers
            o_count = 0; t_count = 0
        }
        BEGIN { state = 0; o_count = 0; t_count = 0; real_conflict = 0 }
        # Marker detection is anchored at column 1 and requires the canonical
        # 7-character marker so we do not misfire on a "<<<<<<<" that appears
        # mid-content (rare, but be precise).
        /^<<<<<<< / {
            if (state != 0) { malformed = 1; exit 2 }
            state = 1; o_count = 0; t_count = 0; in_base = 0
            next
        }
        /^\|\|\|\|\|\|\|/ {
            if (state != 1) { malformed = 1; exit 2 }
            in_base = 1
            next
        }
        /^=======$/ {
            if (state != 1) { malformed = 1; exit 2 }
            state = 2; in_base = 0
            next
        }
        /^>>>>>>> / {
            if (state != 2) { malformed = 1; exit 2 }
            flush_hunk()
            state = 0
            next
        }
        {
            if (state == 0) { print; next }
            if (state == 1) {
                if (in_base) next            # discard diff3 base section
                ours[++o_count] = $0; next
            }
            if (state == 2) { theirs[++t_count] = $0; next }
        }
        END {
            if (state != 0) { exit 2 }       # unterminated conflict marker
        }
    ' "$path" > "$tmp"; then
        # awk exited 0 — fully resolved. Replace the file.
        mv -f "$tmp" "$path"
        return 0
    else
        local rc=$?
        rm -f "$tmp"
        # rc==1 real conflict, rc==2 malformed; both mean "not trivially
        # resolvable" to the caller.
        return "$rc"
    fi
}

# attempt_trivial_conflict_resolution
#
# Operates on the CURRENT git repo (cwd) that is mid-rebase/merge with
# conflicts. For every unmerged path it tries resolve_conflict_markers_in_file
# (choosing the keep-both policy per is_additive_keepboth_path) and `git add`s
# the file on success. Returns:
#   0  — every unmerged path was trivially resolved and staged. Caller can
#        `git rebase --continue` (or commit the merge).
#   1  — at least one path is a real conflict. Caller MUST abort and route.
#        Already-resolved files are left staged; the caller aborts the whole
#        rebase anyway, so partial staging is harmless.
#
# Conflict types we do NOT touch (always real → return 1): delete/modify,
# rename/rename, add/add of a binary file, submodule conflicts. These show up
# in `git status --porcelain` with codes other than the content-conflict codes
# we handle (UU, AA), or have no parseable text markers; we detect them and
# bail rather than guess.
attempt_trivial_conflict_resolution() {
    local any_real=0
    local resolved_count=0
    local porcelain
    porcelain="$(git status --porcelain 2>/dev/null)" || return 1

    # Unmerged entries have an XY status from this set:
    #   DD, AU, UD, UA, DU, AA, UU
    # We only attempt the two TEXT content-conflict shapes:
    #   UU = both modified, AA = both added.
    # Every other unmerged shape (delete/modify, rename, etc.) is a structural
    # conflict we refuse to auto-resolve.
    local line xy file
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        xy="${line:0:2}"
        file="${line:3}"
        # `git status --porcelain` quotes paths with special chars; strip a
        # surrounding pair of double quotes if present (best-effort — quoted
        # paths are rare in the trees we maintain and a mismatch just routes).
        if [[ "$file" == \"*\" ]]; then
            file="${file#\"}"
            file="${file%\"}"
        fi
        case "$xy" in
            UU|AA)
                local policy=0
                if is_additive_keepboth_path "$file"; then
                    policy=1
                fi
                if resolve_conflict_markers_in_file "$file" "$policy"; then
                    git add -- "$file" >/dev/null 2>&1 || { any_real=1; break; }
                    resolved_count=$((resolved_count + 1))
                else
                    # Real conflict (or unreadable). Stop — caller routes.
                    any_real=1
                    break
                fi
                ;;
            DD|AU|UD|UA|DU)
                # Structural conflict — never auto-resolve.
                any_real=1
                break
                ;;
            *)
                # Not an unmerged entry (e.g. plain modified/added from the
                # rebase replay). Ignore — `git rebase --continue` handles it.
                :
                ;;
        esac
    done <<<"$porcelain"

    if (( any_real )); then
        return 1
    fi

    # Guard: if NOTHING was resolved but git still reports unmerged files, the
    # porcelain parse missed something — treat as real conflict, don't claim
    # success on an unresolved tree.
    if (( resolved_count == 0 )) && git ls-files --unmerged 2>/dev/null | grep -q .; then
        return 1
    fi

    # Final safety net: no conflict markers may remain in any tracked file.
    if git -c core.pager=cat grep -lE '^(<<<<<<< |=======$|>>>>>>> )' -- . >/dev/null 2>&1; then
        if git -c core.pager=cat grep -lE '^(<<<<<<< |=======$|>>>>>>> )' -- . 2>/dev/null | grep -q .; then
            return 1
        fi
    fi

    return 0
}

# ---------------------------------------------------------------------------
# Deployer-specific driver — NOT part of the ported classifier above; new for
# ga-gcy0cd / FR-5 / FR-6. Bounds the self-rebase to internally-authored
# branches: the deployer only ever holds builder/deployer-owned branches
# (contributor PRs are structurally out of the deployer's scope per
# prompts/deployer.md's own "never touch a contributor's work" guardrail), so
# this function does not re-derive fork/authorship — that separation is
# enforced by who calls it, not by a runtime check here. The contributor-fork
# rebase path stays exclusively maintainer-pr-review's
# attempt_rebase_against_base() in commands/run-pr.sh; this function must
# never be used as a substitute for that path.
# ---------------------------------------------------------------------------

# attempt_bounded_self_rebase <branch> [<base_ref>]
#
# Attempts a bounded, provably-trivial self-rebase of <branch> onto
# origin/<base_ref> (default: main) and, on success, force-with-lease-pushes
# the result. Assumes the CURRENT working tree (cwd) is already the
# deployer's checkout of <branch> — unlike maintainer-pr-review's
# attempt_rebase_against_base, this never clones or checks out a PR; the
# deployer is always already sitting in its own branch's worktree by the time
# the evaluate-gate step runs.
#
# On success (return 0), prints two lines to stdout for the caller to log to
# the bead notes for audit (FR-5's requirement):
#   BEFORE_SHA=<sha-before-rebase>
#   AFTER_SHA=<sha-after-rebase>
#
# Returns:
#   0  — rebased (trivial conflicts auto-resolved, or no conflicts at all)
#        and force-with-lease-pushed. BEFORE_SHA/AFTER_SHA printed to stdout.
#   20 — no-op: <branch> already contains origin/<base_ref>. Nothing to
#        rebase; caller should treat criterion 6 as already passing.
#   10 — setup failure: bad arguments, <branch> is a protected name
#        (main/master), cwd is not checked out to <branch>, the working tree
#        is dirty, or the fetch failed. Caller falls back to route-to-builder.
#   12 — real (non-trivial) conflict. The rebase was aborted and <branch> is
#        left exactly as it was before this call. Caller falls back to
#        route-to-builder — this is the "fall back to today's unchanged
#        behavior" path required by FR-6.
#   13 — rebased cleanly but the force-with-lease push was rejected (the
#        lease went stale — something else pushed to <branch> concurrently).
#        The local branch IS rebased but the remote is NOT updated. Caller
#        falls back to route-to-builder; the next gate cycle re-fetches and
#        retries from current state.
attempt_bounded_self_rebase() {
    local branch="$1"
    local base_ref="${2:-main}"

    [[ -n "$branch" ]] || return 10
    case "$branch" in
        main|master) return 10 ;;  # never self-rebase a protected branch
    esac

    local current_branch
    current_branch="$(git symbolic-ref --short HEAD 2>/dev/null || git branch --show-current 2>/dev/null)"
    [[ -n "$current_branch" && "$current_branch" == "$branch" ]] || return 10

    # A dirty working tree is a setup failure, not something to negotiate.
    # Criterion 5 (clean tree) is evaluated separately and should already
    # guarantee this; a rebase into an unexpectedly dirty tree is unsafe.
    [[ -z "$(git status --porcelain 2>/dev/null)" ]] || return 10

    git fetch origin "$base_ref" >/dev/null 2>&1 || return 10

    local before_sha
    before_sha="$(git rev-parse HEAD 2>/dev/null)" || return 10

    # Already on top of base? Then criterion 6's FAIL was stale — nothing to
    # rebase, no push needed.
    if git merge-base --is-ancestor "origin/$base_ref" HEAD 2>/dev/null; then
        return 20
    fi

    if git rebase "origin/$base_ref" >/dev/null 2>&1; then
        : # clean rebase, no conflicts at all — itself a trivial outcome
    else
        local steps=0 max_steps=50
        while :; do
            # Are we actually mid-rebase with conflicts? If the rebase
            # stopped for another reason, abort and route rather than guess.
            if [[ ! -d .git/rebase-merge && ! -d .git/rebase-apply ]]; then
                git rebase --abort >/dev/null 2>&1 || true
                return 12
            fi
            steps=$((steps + 1))
            if (( steps > max_steps )); then
                git rebase --abort >/dev/null 2>&1 || true
                return 12
            fi
            # attempt_trivial_conflict_resolution returns 0 only when EVERY
            # unmerged path was provably-trivially resolved + staged.
            if ! attempt_trivial_conflict_resolution; then
                git rebase --abort >/dev/null 2>&1 || true
                return 12   # real / non-trivial conflict
            fi
            # Continue the rebase with the staged resolutions. GIT_EDITOR=true
            # accepts the existing commit message non-interactively.
            if GIT_EDITOR=true git rebase --continue >/dev/null 2>&1; then
                break   # rebase finished cleanly
            fi
            # --continue returned non-zero: either the next commit also
            # conflicts (loop again) or a hard failure. The loop's top
            # re-checks for a rebase-in-progress and bails if not.
        done
    fi

    # Belt-and-suspenders: no conflict markers may remain anywhere.
    if git -c core.pager=cat grep -lE '^(<<<<<<< |=======$|>>>>>>> )' -- . 2>/dev/null | grep -q .; then
        git rebase --abort >/dev/null 2>&1 || true
        return 12
    fi

    # Force-push the rebased branch. --force-with-lease (NOT --force) keeps
    # us from clobbering a concurrent push to this same branch.
    if ! GIT_TERMINAL_PROMPT=0 git push --force-with-lease origin "$branch" >/dev/null 2>&1; then
        return 13
    fi

    local after_sha
    after_sha="$(git rev-parse HEAD)"
    printf 'BEFORE_SHA=%s\nAFTER_SHA=%s\n' "$before_sha" "$after_sha"
    return 0
}
