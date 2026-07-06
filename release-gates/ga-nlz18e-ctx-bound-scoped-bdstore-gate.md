# Release gate: ga-nlz18e ctx-bound scoped BdStore

Date: 2026-07-04
Result: PASS

## Candidate

- Deploy bead: ga-nlz18e
- Source implementation bead: ga-cdmx6x
- Review bead: ga-bytd3q
- Reviewed commit: 5e8459b3893fdc2e18f127c8200c98fc1fac1cf2 on gc-builder-3-dad840a7d698
- Clean release branch tested: release/ga-nlz18e-ctx-bound-scoped-bdstore
- Base: origin/main at d82074594d7594eea890e5300d7936540f30bd9e
- Tested commit: a61385aa2 (4e45acbd4 clean cherry-pick of 5e8459b38, plus a
  compile-only fix for the gap below)

The reviewed builder branch was stacked on deploy/ga-oz3ow5.1-graphonlyready-clean,
which is not in origin/main and carries a separate graph-only readiness feature.
For the single-bead gate, I tested the reviewed status patch by itself on current
origin/main. The cherry-pick applied without conflicts.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-bytd3q is closed with `REVIEW VERDICT: PASS` for commit 5e8459b3893fdc2e18f127c8200c98fc1fac1cf2. |
| 2 | Acceptance criteria met | PASS | Verified on the final clean branch (code commit a61385aa2, gate tip 00b7977a); `go build ./...`, `go vet ./...`, `git diff --check`, and `gofmt -l` on touched Go files are clean. |
| 3 | Tests pass | PASS | See "Deployer re-verification before PR" below; `make test-fast-parallel` passed all 8 shards on the final branch. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no blockers and only one non-blocking security observation; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Worktree clean before this evidence refresh; final status rechecked after committing the refreshed gate. |
| 6 | Branch diverges cleanly from main | PASS | Clean branch was cut from origin/main and the reviewed commit cherry-picked with no merge conflicts. |
| 7 | Single feature theme | PASS | The clean branch contains only the status/API scoped BdStore cancellation change (plus the one-line test compile fix), not the unrelated graph-only readiness parent stack. |

## Fix applied (previous FAIL -> this PASS)

Prior FAIL: `internal/api/store_health_test.go:171:9: not enough arguments in
call to s.computeStoreHealth; have (); want ("context".Context)`.
`TestComputeStoreHealthUsesDoltlitePathFromMetadata` still called
`computeStoreHealth()` with no argument after the clean cherry-pick changed the
signature to require `context.Context`, unlike its two sibling call sites in the
same file which already passed `context.Background()`.

Fix: commit a61385aa2 on `release/ga-nlz18e-ctx-bound-scoped-bdstore` — one-line,
compile-only, no behavior change (passes `context.Background()`, matching the
sibling call sites).

## Deployer re-verification before PR

Ran with `TMPDIR=/var/tmp/gc-nlz18e-deploy` from branch tip 00b7977a (same code
candidate a61385aa2; the only later commit is this release gate).

- `git fetch origin main`: origin/main remains d82074594.
- `git merge-tree --write-tree HEAD origin/main`: PASS, tree
  a9fe60a569e349d2eed6213de90f28aa85fa00c1, no conflicts.
- `git diff --check origin/main...HEAD`: clean.
- `gofmt -l` on all touched Go files: clean.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `go test ./internal/api/... ./internal/beads/...`: PASS.
- `GC_REAL_PROCESS_SIGNAL_TESTS=1 GC_FAST_UNIT=0 go test ./cmd/gc ./internal/api
  -run 'Test(ScopedBdStoreForCityKillsChildOnCtxCancel|LoadStatusSessionSnapshotKillsBdChildOnTimeout|StatusSessionSnapshotKillsBdChildOnTimeout|StatusListStoreWithTimeoutKillsBdChildOnTimeout|ComputeStoreHealthUsesDoltlitePathFromMetadata)$'
  -count=1 -v`: PASS.
- `make test-fast-parallel`: PASS, all 8 fast shards passed.
- `make dashboard-check`: PASS; OpenAPI/generated dashboard paths have no diff.
