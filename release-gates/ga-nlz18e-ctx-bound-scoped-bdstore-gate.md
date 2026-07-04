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
| 2 | Acceptance criteria met | PASS | Verified on the final clean branch (a61385aa2) after the compile fix below; `go build ./...` and `go vet ./...` clean, `gofmt -l` clean on the touched file. |
| 3 | Tests pass | PASS | See "Test verification after fix" below. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no blockers and only one non-blocking security observation; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Worktree clean at a61385aa2. |
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

## Test verification after fix

Ran with `TMPDIR=/var/tmp/gc-nlz18e-verify2*` (this box's `/tmp` tmpfs is at
~100% full independent of this change — see mail to mayor 2026-07-04; using
`/var/tmp` avoids spurious "no space left on device" noise).

- `go build ./...`, `go vet ./...`: clean.
- `gofmt -l internal/api/store_health_test.go`: clean.
- `go test ./internal/api/... -run TestComputeStoreHealthUsesDoltlitePathFromMetadata -v`: PASS.
- `go test ./internal/api/...` (full package): all PASS (58.0s).
- `TMPDIR=/var/tmp/gc-nlz18e-verify2-full make test-fast-parallel`: 5/8 shards
  PASS. 3 shards failed, all on the same 3 tests:
  `TestRegisterCityWithSupervisorRejectsStandaloneController`,
  `TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity`,
  `TestSupervisorCreatesControllerSocketForManagedCity` (`cmd/gc`, unrelated
  supervisor/dolt-lifecycle package — no import relationship to
  `internal/api/store_health_test.go`). All three fail on a hard 50-60s
  supervisor/dolt-city startup timeout, not an assertion mismatch.
  **Confirmed pre-existing/environmental, not caused by this change:** re-ran
  the same 3 tests in isolation against a clean, unmodified `origin/main`
  checkout (`git worktree add --detach /var/tmp/gc-main-verify-nlz18e
  origin/main`, commit d82074594) under the same load — identical failures,
  identical signatures, comparable timings (46-60s). This box is running a
  very large number of concurrent agent/test workloads right now; these tests
  spin up a real supervisor + dolt sql-server and are sensitive to that load.
  Worktree removed after comparison (`git worktree remove
  /var/tmp/gc-main-verify-nlz18e`).
