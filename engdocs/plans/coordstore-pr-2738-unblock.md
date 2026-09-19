# PR #2738 Coordstore Unblock Plan

Root bead: `ga-snab2n`

Architecture decision: `ga-linjt0.1`

Downstream deploy bead blocked by this plan: `ga-linjt0.2`

## Problem

The retention-aware coordstore import/shadow deploy cannot resume until PR
#2738, `feat(coordstore): opt-in SQLite-cgo bead store backend`, lands in
`origin/main`.

The architect decision in `ga-linjt0.1` found no safe independent deploy path
for the retention-aware follow-up commit `07a9fd70a` because the files it
patches are introduced by PR #2738. PR #2738 is currently conflicting with
`origin/main` and has no formal review recorded.

Known PR #2738 conflict files:

- `cmd/gc/main.go`
- `go.sum`
- `internal/beads/exec/exec_test.go`

## Scope

This plan only unblocks the underlying PR #2738 coordstore SQLite stack. It
does not merge, cherry-pick, or otherwise include the retention-aware follow-up
commit `07a9fd70a`.

PM is not choosing a new release topology here. The release-path decision has
already been made by architecture: wait for PR #2738 to land, then resume the
retention-aware deploy work from a clean base.

## Work Packages

### `ga-snab2n.1` - Rebase PR #2738

Route: `gascity/builder`

Label: `ready-to-build`

Acceptance:

- Work from PR #2738 branch `builder/ga-aec8q.16-sqlite-cutover`, or document
  the replacement branch/PR used for the same stack.
- Rebase or otherwise refresh the PR branch onto current `origin/main` so
  GitHub no longer reports merge conflicts.
- Resolve the known conflict files without introducing unrelated scope outside
  the PR #2738 coordstore SQLite stack.
- Preserve the PR #2738 review boundary; do not include the retention-aware
  follow-up commit `07a9fd70a`.
- Run and record focused evidence for the touched conflict areas plus any
  standard PR gate commands the branch already requires.
- Update the bead with branch name, commit SHA, conflict-resolution summary,
  diffstat summary, and commands run.

### `ga-snab2n.2` - Validate Merge and Test Evidence

Route: `gascity/validator`

Label: `needs-tests`

Blocked by: `ga-snab2n.1`

Acceptance:

- Use the builder-provided branch/commit from `ga-snab2n.1`.
- Confirm the branch merges cleanly with current `origin/main` using a
  merge-tree or equivalent non-mutating mergeability check.
- Confirm the known conflict files are resolved.
- Confirm the diff remains within the PR #2738 coordstore SQLite stack and does
  not include retention-aware follow-up commit `07a9fd70a`.
- Run or verify `go test ./...` and `go vet ./...` for the refreshed branch, or
  record the exact failing command and blocker.
- Record a PASS/FAIL result with exact commands, branch, commit SHA, and any
  remediation bead needed.

### `ga-snab2n.3` - Complete Review and Merge Gate

Route: `gascity/builder`

Label: `ready-to-build`

Blocked by: `ga-snab2n.2`

Acceptance:

- Confirm the validation child bead has a PASS result before requesting merge
  completion.
- Update PR #2738 with the validation evidence or a link to the validator bead.
- Ensure a formal review is requested or completed; record reviewer/status and
  PR URL.
- Close this bead only after PR #2738 is merged into `origin/main`; record the
  merge SHA and date.
- If maintainer review or merge authority blocks completion, leave this bead
  open and notify PM/mayor with the precise external blocker.
- Do not include or request merging the retention-aware follow-up commit
  `07a9fd70a` as part of PR #2738.

## Dependency Graph

`ga-snab2n.1` -> `ga-snab2n.2` -> `ga-snab2n.3` -> `ga-linjt0.2`

## Handoff Notes

`ga-linjt0.2` is now blocked on `ga-snab2n.3` so the retention-aware deploy
branch cannot resume until PR #2738 has landed. If PR #2738 is split or
restructured during review, architecture must re-evaluate which landed PR
introduces the files needed by the retention-aware follow-up.

Tracker import was skipped because no tracker skill is installed in this
worktree.
