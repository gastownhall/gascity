# Release Gate: ga-7qdxe4 worktree-stale marker producer

- Bead: `ga-7qdxe4`
- Source bead: `ga-vzt5pq.3`
- Deploy branch: `deploy/ga-7qdxe4-gate`
- Reviewed deploy SHA: `5538000b7b60d30ee25adf2302d5987edb5a8d45`
- Base checked: `origin/main` at `bac288647e0bbbbe2e68bdbe588709eb2827f5ee`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-base origin/main 5538000b7b60d30ee25adf2302d5987edb5a8d45` returned `bac288647e0bbbbe2e68bdbe588709eb2827f5ee` and `git merge-tree --write-tree origin/main 5538000b7b60d30ee25adf2302d5987edb5a8d45` exited 0. |
| 1 | Review PASS present | PASS | `bd show ga-vzt5pq.3` records `REVIEW VERDICT: PASS`; `bd show ga-7qdxe4` describes the reviewed branch as PASSED and routed for deploy. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `cmd/gc/session_worktree_prune.go`, `cmd/gc/session_worktree_prune_test.go`, and `cmd/gc/session_worktree_prune_info_test.go`. `gitProbe` now exposes `CurrentBranch`; both raw and Info prune paths write `.worktree-stale` with `uncommitted-work`, `unpushed-commits`, or `stashed-work` only after confirmed dirty probes; marker writes are best-effort and do not alter control flow. Focused tests cover marker presence and absence across dirty, no-op, probe-error, and happy paths. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestPruneAgentHomeWorktreeIfSafe\|WorktreeStale\|CurrentBranch' -v` passed in this deploy worktree. `go vet ./...` passed. `make test-fast-parallel` passed all 8 fast jobs from a clean non-nested `/var/tmp` worktree at the deploy SHA. The same fast command in the nested deployer worktree first failed only on `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`; that targeted failure reproduced on `origin/main` in the same nested worktree and passed in the clean `/var/tmp` worktree, matching the known current-worktree artifact documented on the bead. |
| 4 | No high-severity review findings open | PASS | Review notes report OWASP no findings. The only review observation is explicitly minor/non-blocking: `CurrentBranch()` errors produce `branch=""` in the advisory marker while the consumer revalidates branch state before acting. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` on `deploy/ga-7qdxe4-gate` showed only `## deploy/ga-7qdxe4-gate`. This gate file is the only deployer-authored change and is committed on top of the reviewed deploy SHA. |
| 7 | Single feature theme | PASS | Commit range `origin/main..5538000b7b60d30ee25adf2302d5987edb5a8d45` contains the red/green pair for one subsystem: agent-home worktree prune stale-marker production and its symmetric tests. |

## Commit Range

| Commit | Purpose |
|--------|---------|
| `926d429ed044218457223cbe10d738122e4bfe8f` | Red tests for `.worktree-stale` marker production on dirty prune skips. |
| `5538000b7b60d30ee25adf2302d5987edb5a8d45` | Implementation: write best-effort stale markers from raw and Info prune paths. |
