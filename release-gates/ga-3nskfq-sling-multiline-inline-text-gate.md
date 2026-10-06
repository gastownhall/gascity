# Release Gate: `gc sling` multi-line inline text refusal

- Deploy bead: `ga-3nskfq`
- Source bead: `ga-d1n9bd`
- Reviewed commit: `e4645baf38f1f90ab6568a41133dfac04f968720`
- Deploy branch: `deploy/ga-3nskfq-gate`
- Evaluated: 2026-07-25
- Gate source: deployer prompt release-gate table. `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Summary

PASS. `gc sling` now rejects pasted multi-line inline text before creating a bead, both for real runs and dry-runs. Single-line inline text and bead IDs keep their existing behavior.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git fetch origin main`; `git merge-tree --write-tree origin/main e4645baf38f1f90ab6568a41133dfac04f968720` returned tree `9dccb7bed629238c001c1ca7af1f00339c1f7d6c`; `git diff --check origin/main...e4645baf38f1f90ab6568a41133dfac04f968720` produced no output. |
| 1 | Review PASS present | PASS | Deploy bead `ga-3nskfq` records reviewer PASS for source bead `ga-d1n9bd`; source notes contain `Review verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | Commit set is the expected red/green pair: `4faae17ea` and `e4645baf3`. Diff is limited to `cmd/gc/cmd_sling.go` and `cmd/gc/cmd_sling_test.go`. Focused tests verify multi-line inline text errors for real and dry-run paths, while single-line text and bead IDs keep existing behavior. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestResolveInlineBeadAction' -count=1 -v` passed 14/14. `go build ./...` passed. `go vet ./...` passed. Initial `make test-fast-parallel` failed on unrelated `TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted` and `TestProductMetricsDirectChildEnvSessionSubmitPoller`; isolated retries of both tests passed, and bounded full-suite rerun of `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0 | rg -i -- 'ga-3nskfq|ga-d1n9bd|HIGH|request-changes|security'` returned only sling helper bead `ga-qk6m1q`; no open HIGH/request-changes finding was found. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` returned only `## deploy/ga-3nskfq-gate`. The gate file is committed as the final branch tip before push. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: `gc sling` inline text parsing and its tests. The sibling conflicting-route refusal is intentionally separate. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main e4645baf38f1f90ab6568a41133dfac04f968720
git diff --check origin/main...e4645baf38f1f90ab6568a41133dfac04f968720
git log --oneline --reverse 80e5166473033b9f2807dad048ddcb70dfc3b86e..e4645baf38f1f90ab6568a41133dfac04f968720
git diff --stat 80e5166473033b9f2807dad048ddcb70dfc3b86e..e4645baf38f1f90ab6568a41133dfac04f968720
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./cmd/gc -run 'TestResolveInlineBeadAction' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go build ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go vet ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./cmd/gc -run '^TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted$' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./internal/session -run '^TestProductMetricsDirectChildEnvSessionSubmitPoller$' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
bd list --status open --limit 0 | rg -i -- 'ga-3nskfq|ga-d1n9bd|HIGH|request-changes|security'
```
