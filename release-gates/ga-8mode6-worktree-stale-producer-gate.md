# Release Gate: `.worktree-stale` producer

- Deploy bead: `ga-8mode6`
- Source bead: `ga-vzt5pq.3`
- Reviewed commit: `4ca644542590dcede58955dfb2c18bb8d19b4e09`
- Deploy branch: `deploy/ga-8mode6-gate`
- Evaluated: 2026-07-24
- Gate source: deployer prompt release-gate table. `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Summary

PASS. This change wires the existing agent-home worktree cleanup marker into the prune path: when an agent-home worktree is confirmed dirty, Gas City writes `.worktree-stale` with the current branch and reason so the existing cleanup consumer can revisit it later.

The candidate is unchanged from the reviewed SHA. A prior gate attempt failed under fast-suite contention on a `city_runtime_test.go` timeout; builder re-ran the exact failing test/shard three times clean, and this gate's bounded rerun also produced a clean fast-suite pass.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git fetch origin main`; `git merge-tree --write-tree origin/main 4ca644542590dcede58955dfb2c18bb8d19b4e09` returned tree `48ac259dd904133abcb8aa2c606f7917f2e1099d`; `git diff --check origin/main...4ca644542590dcede58955dfb2c18bb8d19b4e09` produced no output. |
| 1 | Review PASS present | PASS | Deploy bead `ga-8mode6` records reviewer PASS for source bead `ga-vzt5pq.3`; source notes contain `REVIEW VERDICT: PASS`. Builder retry notes routed the unchanged reviewed commit back to deploy after reproducing the prior gate failure as a timing-sensitive flake. |
| 2 | Acceptance criteria met | PASS | Commit set is the expected red/green pair: `890b6bd6d` and `4ca644542`. Diff is limited to `cmd/gc/session_worktree_prune.go`, `cmd/gc/session_worktree_prune_test.go`, and `cmd/gc/session_worktree_prune_info_test.go`. Focused tests verify marker writes for uncommitted, unpushed, and stashed work, plus marker absence on no-op/error paths. |
| 3 | Tests pass | PASS | Focused `go test ./cmd/gc -run 'TestPruneAgentHomeWorktreeIfSafe|WorktreeStale|CurrentBranch|TestCleanupClosedBeadAgentHomeWorktrees_DetachesToMainNotCurrentBranch' -count=1 -v` passed. `go build ./...` passed. `go vet ./...` passed. Initial `make test-fast-parallel` failed on unrelated `TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity`; isolated single-test retry passed, exact shard 4/6 retry passed, and bounded full-suite rerun of `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0 | rg -i -- 'ga-8mode6|ga-vzt5pq\\.3|HIGH|request-changes|security'` returned only sling helper beads `ga-htgtnv` and `ga-n2ch96`; no open HIGH/request-changes finding was found. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` returned only `## deploy/ga-8mode6-gate`. The gate file is committed as the final branch tip before push. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: the session worktree prune marker producer and its tests. It does not alter the cleanup consumer or unrelated runtime behavior. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 4ca644542590dcede58955dfb2c18bb8d19b4e09
git diff --check origin/main...4ca644542590dcede58955dfb2c18bb8d19b4e09
git log --oneline --reverse 97e1cb5272a41f21efd7e137a143c35cf34cc713..4ca644542590dcede58955dfb2c18bb8d19b4e09
git diff --stat 97e1cb5272a41f21efd7e137a143c35cf34cc713..4ca644542590dcede58955dfb2c18bb8d19b4e09
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./cmd/gc -run 'TestPruneAgentHomeWorktreeIfSafe|WorktreeStale|CurrentBranch|TestCleanupClosedBeadAgentHomeWorktrees_DetachesToMainNotCurrentBranch' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go build ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go vet ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./cmd/gc -run '^TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity$' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=20m ./scripts/test-go-test-shard ./cmd/gc 4 6
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
bd list --status open --limit 0 | rg -i -- 'ga-8mode6|ga-vzt5pq\.3|HIGH|request-changes|security'
```
