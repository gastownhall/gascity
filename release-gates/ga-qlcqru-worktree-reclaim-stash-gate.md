# Release Gate: Worktree reclaim gates on repo-global 'git stash list'

Bead: `ga-qlcqru`
Build bead: `ga-14w`
Review bead: `ga-zpo` (round 1: request-changes, this section only; round 2: this correction)
Branch: `builder/ga-pyp2oh` (forked from `origin/main`)
RED: `b6d2d07d8d68e85019c2fb2b2eb35253c77b6b6c`
GREEN / current HEAD: `79431771405fb76a63fbe8a3f33241b16c09184b`
Base: `origin/main`

Gate result: PASS

## Correction note (round 2)

An earlier version of this document described a 6-file diff that also
touched `internal/doctor/checks_semantic.go` and
`internal/doctor/checks_semantic_test.go`, with an invented diffstat and a
test-timing claim (`11.955s`) for `internal/doctor`. That content described
a real but **earlier, wider-scoped** attempt at this fix (deploy bead
`ga-qlcqru`'s own original submission, SHA `a68e8f6ceb302e0599082abe9ac2766b8af074b7`,
which genuinely did touch `internal/doctor`) — it was not invented from
nothing, but it was left in place after the branch was later rebased and
rescoped by build bead `ga-14w`, which explicitly narrowed the fix to
**exclude** `internal/doctor/checks_semantic.go`:

> Leave `internal/doctor/checks_semantic.go` alone — its stash check
> reports to a human operator rather than gating a destructive removal, so
> the repo-global answer is acceptable there.

Nobody updated this file to match the rescoped diff, so it kept describing
a change that never shipped on this branch. Review bead `ga-zpo` (round 1)
caught the mismatch: `git log --oneline -- internal/doctor/` on this
branch is empty, and `internal/doctor/checks_semantic.go` at current
`origin/main` still calls `HasStashesResult()` unchanged — exactly as
`ga-14w` intended, not a missed fix. This revision replaces that section
with the actual 5-file diff below, verified first-hand against the current
branch tip (`79431771` = `ga-14w`'s `tdd_green`).

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS (round 1, code only) | Review bead `ga-zpo` round 1: build/vet/gofmt/golangci-lint clean, targeted + full acceptance tests pass, OWASP walk found no blocker/major findings, safety-gate removal reasoned sound. Sole blocking finding was this document's fabricated `internal/doctor` claim (round 1 verdict: request-changes), addressed in this revision. |
| 2 | Acceptance criteria met | PASS | Removes the repo-global `git stash list` gate (`HasStashesResult()`) from the two `cmd/gc` call sites (`bead_worktree_reaper.go`, `session_worktree_prune.go`) per `ga-14w`'s spec. `refs/stash` is one ref shared by the whole repo (lives in the shared `git-common-dir`, not per-worktree), so the removed check was unconditionally true fleet-wide and blocked reaping of worktrees with no relationship to the stash that tripped it. `internal/doctor/checks_semantic.go` is deliberately untouched — out of scope per `ga-14w` (see Correction note above). |
| 3 | Tests pass | PASS | See Verification evidence below. |
| 4 | No high-severity review findings open | PASS | `ga-zpo` round 1 recorded no blocker/major findings on the code; the sole blocking item was this document, corrected here. |
| 5 | Final branch is clean | PASS | Worktree clean at `79431771` before this correction was added. |
| 6 | Branch diverges cleanly from main | PASS | `builder/ga-pyp2oh` forked directly from `origin/main`; `ga-zpo`'s own structural check confirmed the merge-base already contains PR #4816's reachability rework this fix sits on top of, with no intervening `origin/main` commits touching the diffed files. |
| 7 | Single feature theme | PASS | 5 files, one subsystem: the repo-global stash-gate false positive in worktree reclaim (`cmd/gc`), plus this gate record. |

## Root cause

`refs/stash` lives in the shared `git-common-dir`, not per-worktree — so
`HasStashesResult()` run from any worktree saw stashes made in *any other*
worktree of the same repo. The check was meant to protect a worktree's own
stashed work from being lost on removal, but `git worktree remove` never
touches the shared object store or refs where the stash lives, so the
stash was never actually at risk — the check only ever produced false
positives, blocking removal of worktrees unrelated to whichever stash
existed anywhere in the repo.

## Diff Summary

`git diff --name-status origin/main...HEAD` (5 files):

```text
M	cmd/gc/bead_worktree_reaper.go
M	cmd/gc/session_worktree_prune.go
M	cmd/gc/session_worktree_prune_info_test.go
M	cmd/gc/session_worktree_prune_test.go
A	release-gates/ga-qlcqru-worktree-reclaim-stash-gate.md
```

`git diff --stat origin/main...HEAD`:

```text
 cmd/gc/bead_worktree_reaper.go                     |   9 +-
 cmd/gc/session_worktree_prune.go                   |  25 +----
 cmd/gc/session_worktree_prune_info_test.go         |  24 -----
 cmd/gc/session_worktree_prune_test.go              |  71 ++++++++++----
 release-gates/ga-qlcqru-worktree-reclaim-stash-gate.md | (this file)
 5 files changed, 160 insertions(+), 75 deletions(-)
```

`internal/git/git.go` still defines `HasStashesResult()` (used by
`internal/doctor`, which is out of scope here per the Correction note
above); only its two `cmd/gc` call sites were removed. A repo-wide grep
confirms zero remaining references to `HasStashesResult(` under `cmd/gc/`.

## Verification evidence

Independently reproduced in a fresh, isolated scratch worktree at the
current branch tip (`79431771405fb76a63fbe8a3f33241b16c09184b`), not the
shared rig:

1. `go build ./...` — clean.
2. `go vet ./...` — clean.
3. `gofmt -l` on all 4 touched `.go` files — clean.
4. `golangci-lint run --new-from-rev=origin/main ./cmd/gc/...` (v2.12.0,
   scoped to touched lines via git-diff-aware mode) — 0 issues.
5. `GC_FAST_UNIT=0 go test ./cmd/gc/ -run 'Worktree|Prune|Reap' -count=1 -v`
   (exact command from `ga-14w`'s done-when checklist) — **215 PASS, 0
   FAIL, 0 SKIP**, `ok` in 13.503s. Includes the diff-owned regression test
   `TestPruneAgentHomeWorktreeIfSafe_UnrelatedStashDoesNotBlock`, which
   `ga-zpo`'s round-1 review independently confirmed fails at RED
   (`b6d2d07d8d68e85019c2fb2b2eb35253c77b6b6c`) with the expected pre-fix
   symptom and passes at current HEAD — genuine red/green, not just
   claimed. (Two tests self-skip without `GC_FAST_UNIT=0`; `ga-zpo`'s
   round-1 run without that variable set observed 213 PASS/2 SKIP on the
   same test set — same evidence, environment-driven skip count, nothing
   failed either way.)

## Note on PR #4619

Per the mayor sequencing ruling recorded in `ga-qlcqru`'s notes
(2026-07-25/26): PR #4619 (`deploy/ga-8mode6-gate`) touches the same
`cmd/gc` files this fix does and was ordered to land *after* this fix, not
folded into it. Not touched, rebased, or merged as part of this branch. It
remains held on its own architectural hold (`gm-21ld6`) and needs a merits
re-examination after this lands — flagged to mayor, not resolved here.

## Disposition

- `builder/ga-pyp2oh` is pushed to origin at
  `79431771405fb76a63fbe8a3f33241b16c09184b`, this correction added as a
  follow-up commit on the same branch.
- PR #4732 (`gastownhall/gascity`) already tracks this branch, is
  `CLEAN`/`MERGEABLE`, and carries a human APPROVAL
  (`csauer02-personal-user`, 2026-08-18) on this exact commit. Merge
  authority is operator/mayor/mpr only — not resolved here.
- Next: hand back to review bead `ga-zpo` for round-2 re-review of this
  correction (code is unchanged from round 1; only this document changed).
