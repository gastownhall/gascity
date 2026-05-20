#!/usr/bin/env bash
# refinery-rebase.sh — safely rebase a polecat branch onto a merge target.
#
# Usage:
#   refinery-rebase.sh <branch> <target>
#
# Behavior:
#   - Fetches origin, creates a `temp` working branch at origin/<branch>,
#     and rebases it onto origin/<target>.
#   - On success (exit 0): the working branch is `temp`, ready for the
#     refinery's run-tests / merge-push steps.
#   - On rebase conflict (exit 3): `git rebase --abort` runs first, the
#     `temp` branch is deleted, and the worktree is left on <target>.
#     A one-line summary naming the conflict files is printed on stderr
#     and (if REFINERY_REBASE_REPORT is set) written to that file.
#   - On pre-rebase failure (exit 4): fetch or checkout failed before the
#     rebase began; no `temp` branch or rebase state to clean up.
#   - On usage error (exit 2): missing arguments.
#
# Why this script exists
# ----------------------
# The earlier refinery wrapped `git rebase` in `git rebase ... 2>&1 | tail -15`.
# Without `set -o pipefail`, the pipe swallowed rebase's non-zero exit:
# the wrapper proceeded as if the rebase had succeeded and pushed the
# pre-rebase ancestor to the target branch, silently dropping commits
# (issue ga-vnr; post-mortem pe-wisp-tj0bj5). This script encodes the
# safe pattern (set -euo pipefail, output redirected to a tempfile,
# explicit exit codes) so the refinery cannot regress to the buggy
# shape by accident.
#
# Exit codes:
#   0  — rebase succeeded; `temp` branch holds the rebased history
#   2  — usage error (missing branch/target argument)
#   3  — rebase conflict (worktree cleaned up, conflict files reported)
#   4  — fetch / branch-checkout failed before rebase began
set -euo pipefail

BRANCH="${1:-}"
TARGET="${2:-}"

if [ -z "$BRANCH" ] || [ -z "$TARGET" ]; then
    echo "usage: refinery-rebase.sh <branch> <target>" >&2
    exit 2
fi

LOG="$(mktemp -t refinery-rebase.XXXXXX.log)"
trap 'rm -f "$LOG"' EXIT

drop_temp_if_present() {
    if git rev-parse --verify --quiet temp >/dev/null 2>&1; then
        # Avoid `git branch -D` while checked out on temp.
        if [ "$(git rev-parse --abbrev-ref HEAD)" = "temp" ]; then
            git checkout "$TARGET" >>"$LOG" 2>&1 || true
        fi
        git branch -D temp >>"$LOG" 2>&1 || true
    fi
}

if ! git fetch --prune origin >>"$LOG" 2>&1; then
    cat "$LOG" >&2
    exit 4
fi

drop_temp_if_present

if ! git checkout -b temp "origin/$BRANCH" >>"$LOG" 2>&1; then
    cat "$LOG" >&2
    exit 4
fi

# Run the rebase with output redirected to a tempfile. Never pipe `git
# rebase` through `tail`/`grep` — that masks rebase's non-zero exit
# without `pipefail` and is fragile even with `pipefail`.
if git rebase "origin/$TARGET" >>"$LOG" 2>&1; then
    tail -n 5 "$LOG" || true
    exit 0
fi

# Rebase failed. Capture conflicted paths BEFORE aborting — after `git
# rebase --abort` the conflict markers and unmerged paths are gone.
CONFLICT_FILES="$(git diff --name-only --diff-filter=U 2>/dev/null | tr '\n' ' ' | sed 's/[[:space:]]*$//' || true)"

git rebase --abort >>"$LOG" 2>&1 || true
git checkout "$TARGET" >>"$LOG" 2>&1 || true
drop_temp_if_present

if [ -z "$CONFLICT_FILES" ]; then
    CONFLICT_FILES="<unknown>"
fi
REPORT="Rebase conflict: $BRANCH onto $TARGET. Conflict files: $CONFLICT_FILES."
echo "$REPORT" >&2
if [ -n "${REFINERY_REBASE_REPORT:-}" ]; then
    printf '%s\n' "$REPORT" >"$REFINERY_REBASE_REPORT"
fi
exit 3
