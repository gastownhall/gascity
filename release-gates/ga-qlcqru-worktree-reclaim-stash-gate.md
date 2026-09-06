# Release Gate: Worktree reclaim gates on repo-global 'git stash list'

Bead: `ga-qlcqru`
Source bead: `ga-pyp2oh`
Prior reviewed SHA: `a68e8f6ceb302e0599082abe9ac2766b8af074b7` (gate FAIL, criterion 3 — see Prior Failure below)
Resubmitted SHA: `33277c9f98de12ba7e32ab2058fc612e31392121` (rebase of the same reviewed content onto current `origin/main`; no content changes)
Deploy branch: `builder/ga-pyp2oh` (per bead instruction: open a fresh PR from this branch directly, do not fold onto any other branch)
Base: `origin/main` at `422f89c19`

Gate result: PASS

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-qlcqru` records reviewer PASS for `ga-pyp2oh` on commit `a68e8f6ce`; content is unchanged by this rebase (`git diff a68e8f6ce 33277c9f9` is empty modulo parent history). |
| 2 | Acceptance criteria met | PASS | Same reviewed change: removes the repo-global `git stash list` gate (`HasStashesResult`) from `session_worktree_prune.go`, `bead_worktree_reaper.go`, and `internal/doctor/checks_semantic.go`, since the gate is unconditionally true fleet-wide and guards a loss (worktree removal destroying a stash) that does not occur. |
| 3 | Tests pass | PASS (see Prior Failure + Diagnosis below) | `go build ./...` clean. `go vet ./...` clean. `gofmt -l` on all 6 changed files: clean. Focused stash-gate regressions (`TestPruneAgentHomeWorktreeIfSafe\|TestNestedWorktreePruneCheck\|TestReadGitAdminDir\|TestBeadWorktreeReaper\|TestReapClosedBeadWorktrees` across `./cmd/gc/...` and `./internal/doctor/...`): all PASS incl. both new real-git regression tests. Full `internal/doctor/...`: PASS (11.955s). Full `internal/git/...`: PASS (1.027s). Full `make test-cmd-gc-process-parallel` (the gate that failed previously): **all 7 jobs passed** — `cmd-gc-process-{1..6}-of-6` and `productmetrics-testhook` all `ok`, including shard 6 which previously failed. |
| 4 | No high-severity review findings open | PASS | Unchanged from prior review: `ga-pyp2oh` review verdict is `pass`, no HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` → `## builder/ga-pyp2oh...origin/main [ahead 2]`, no uncommitted files. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reports `merged`, no conflicts. |
| 7 | Single feature theme | PASS | Unchanged: 6 files, one subsystem (nested-worktree-prune / worktree-reaper stash gate removal). |

## Prior Failure (criterion 3, SHA `a68e8f6ce`)

`make test-cmd-gc-process-parallel` failed 1/7 jobs: cmd-gc-process shard 6 failed
`TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` with `raw bd create
returned 'schema migration: pending schema migrations alter pre-existing dirty tables:
events'`. Five other cmd/gc shards and productmetrics passed; build, vet, formatting,
focused stash-gate regressions, full `internal/doctor`, and full `internal/git` all
passed on that SHA too.

## Diagnosis

The failing test (`cmd/gc/cmd_bd_test.go`) exercises real managed Dolt sql-servers via
`startPasswordedDoltServer` and drives real `bd`/`gc bd` subprocesses against them. The
exact failure signature — "alter pre-existing dirty tables" on a schema migration check —
is a **documented pre-existing race** in this test suite's own comments, unconnected to
this bead's diff:

`test/integration/integration_test.go:529-536` (`waitForPIDsReaped`):

> Without it, a SIGKILL returns before the kernel has torn the process down and released
> its open files: a following `t.TempDir()` RemoveAll then races a dying managed Dolt
> server under `cityDir/.beads/dolt` ("directory not empty"), and a following test can
> re-bind the just-freed managed Dolt port and adopt a half-dead server whose DB still has
> prior tables ("alter pre-existing dirty tables"). The deadline guarantees a wedged
> process can never hang the suite.

This is exactly the error string that shard 6 hit. It is a cross-test/cross-process Dolt
server lifecycle race that manifests under the concurrent load of a multi-shard parallel
run, not something introduced by this bead's diff.

Ruling this out as diff-related, not just diff-adjacent:

- `ga-pyp2oh`'s diff touches exactly 6 files — `cmd/gc/bead_worktree_reaper.go`,
  `cmd/gc/session_worktree_prune.go`, `cmd/gc/session_worktree_prune_info_test.go`,
  `cmd/gc/session_worktree_prune_test.go`, `internal/doctor/checks_semantic.go`,
  `internal/doctor/checks_semantic_test.go` — and does exactly one thing: removes the
  `HasStashesResult()` gate call and its plumbing. Nothing in the diff touches Dolt,
  schema migrations, bd process lifecycle, or the rig/worktree *store* (as opposed to
  the nested-worktree-prune *doctor check*, which is a different, unrelated meaning of
  "worktree" in this codebase).
- `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` itself is untouched
  by the diff and does not exercise `session_worktree_prune.go`,
  `bead_worktree_reaper.go`, or the doctor check.
- The failing test passed 8/8 consecutive isolated reps
  (`go test -run '^TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore$'
  -count=8`), and passed cleanly once more inside the full 7-job parallel gate run below
  — with zero occurrences of "dirty tables" or "schema migration" anywhere in that run's
  output.

Resolution: rebased `builder/ga-pyp2oh` onto current `origin/main` (`422f89c19`, 26
commits ahead of the previously reviewed base) to produce a new SHA per TESTING.md's
no-same-SHA-retry policy, and reran the full gate. Content is identical to the
reviewed SHA; only the base moved forward.

## Diff Summary

`git diff --name-status origin/main..HEAD`:

```text
M	cmd/gc/bead_worktree_reaper.go
M	cmd/gc/session_worktree_prune.go
M	cmd/gc/session_worktree_prune_info_test.go
M	cmd/gc/session_worktree_prune_test.go
M	internal/doctor/checks_semantic.go
M	internal/doctor/checks_semantic_test.go
```

`git diff --stat origin/main..HEAD`:

```text
 cmd/gc/bead_worktree_reaper.go             |   7 +-
 cmd/gc/session_worktree_prune.go           |  25 +------
 cmd/gc/session_worktree_prune_info_test.go |  24 -------
 cmd/gc/session_worktree_prune_test.go      |  72 ++++++++++++++------
 internal/doctor/checks_semantic.go         |  23 ++-----
 internal/doctor/checks_semantic_test.go    | 103 +++++++++++++++++++++--------
 6 files changed, 135 insertions(+), 119 deletions(-)
```

## Note on PR #4619

Per the mayor sequencing ruling recorded in `ga-qlcqru`'s notes (2026-07-25/26): PR
#4619 (`deploy/ga-8mode6-gate`) was not touched, rebased, or merged as part of this
deploy. It remains held on its own architectural hold (`gm-21ld6`) and needs a merits
re-examination after this lands — flagged to mayor, not resolved here.
