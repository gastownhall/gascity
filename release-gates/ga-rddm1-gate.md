# Release Gate: Wisps planner index verification rebase

- Deploy bead: ga-rddm1
- Source bead: ga-n3ru6.2
- Prior failed gate: ga-d9hf0
- Branch: builder/ga-n3ru6-2
- Implementation commits: 70ec07d41, 7a32fd4fa
- Base checked: origin/main at ed237aa31b6b
- Gate date: 2026-05-22 PDT
- Manifest note: docs/PROJECT_MANIFEST.md is not present in this repo snapshot; this gate uses the deployer release criteria.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-rddm1` contains `Re-review verdict: PASS` for `fork/builder/ga-n3ru6-2` at 7a32fd4fa. |
| 2 | Acceptance criteria met | PASS | Source bead `ga-n3ru6.2` records the planner verification: the mail-check query uses `idx_wisps_type_status_assignee`; the composite-only PrimeWisps `status=open` query scanned the table; `idx_wisps_status` was added only after that scan was observed; the planner output is captured in bead notes; no production migration was attempted. The branch implements those outcomes in `schemas/wisps-composite-index/{common,migrate,rollback}.sh`. |
| 3 | Tests pass | PASS | `bash -n schemas/wisps-composite-index/common.sh schemas/wisps-composite-index/migrate.sh schemas/wisps-composite-index/rollback.sh`, `git diff --check origin/main...HEAD`, `go vet ./...`, and `make test-fast-parallel` all passed. |
| 4 | No high-severity review findings open | PASS | Review notes carry one NONCRITICAL style finding about a redundant `show_wisps_indexes` call. No blocking or HIGH findings are open. |
| 5 | Final branch is clean | PASS | Before writing this gate, `git status --short --branch` showed a clean branch. After committing this gate, the deployer rechecked that the worktree was clean before push. |
| 6 | Branch diverges cleanly from main | PASS | `git log origin/main..fork/builder/ga-n3ru6-2` showed exactly the two migration commits, `git diff --name-status origin/main...fork/builder/ga-n3ru6-2` showed only the three `schemas/wisps-composite-index` scripts, and `git merge-tree --write-tree origin/main HEAD` exited 0. |

## Acceptance Evidence

| Acceptance criterion | Evidence |
|---|---|
| Mail-check planner path uses `idx_wisps_type_status_assignee`, or exact planner output and correction are recorded. | `ga-n3ru6.2` notes record `IndexedTableAccess(wisps), index: [wisps.issue_type,wisps.status,wisps.assignee]` after migration. `common.sh` verifies `idx_wisps_type_status_assignee` has `issue_type`, `status`, and `assignee` in sequence. |
| PrimeWisps `status=open` query is checked after the composite index exists. | `ga-n3ru6.2` notes record the composite-only `status=open` EXPLAIN as a table scan before the secondary index was added. |
| Secondary `idx_wisps_status` is added only if EXPLAIN still shows a full scan. | `migrate.sh` adds `idx_wisps_status` as the second migration step, matching the recorded planner result. `common.sh` verifies the status-only index shape. |
| Verification output is captured with enough detail for operator confirmation. | `ga-n3ru6.2` notes include the composite-only scan, final `SHOW INDEX` rows, indexed mail-check EXPLAIN, indexed PrimeWisps EXPLAIN, rollback result, and test summary. |
| No production migration is attempted by this bead unless dev verification has passed. | `ga-n3ru6.2` notes explicitly state the verification ran on a temporary dev Dolt SQL server and no production migration was attempted. |

## Test Commands

```text
bash -n schemas/wisps-composite-index/common.sh schemas/wisps-composite-index/migrate.sh schemas/wisps-composite-index/rollback.sh
git diff --check origin/main...HEAD
go vet ./...
make test-fast-parallel
git merge-tree --write-tree origin/main HEAD
```
