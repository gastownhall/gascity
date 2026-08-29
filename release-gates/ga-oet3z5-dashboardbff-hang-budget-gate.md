# Release Gate: dashboard BFF hang-budget migration

Date: 2026-08-21  
Deployer: `gascity/deployer`  
Deploy bead: `ga-oet3z5`  
Build bead: `ga-42mt5x.2`  
Review bead: `ga-iroh78`  
Reviewed commit: `e1e5972b46655be23cca75620ed1926d81ed16f3`  
Base checked: `origin/main` at `151168c76b7b5902676ade495c15b13b59186a8b`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This evaluation
therefore uses the deployer release criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-iroh78` is closed with verdict `pass` for the exact reviewed commit. Its independent run recorded 136 PASS, 0 FAIL, 0 SKIP in `internal/api/dashboardbff` and a clean fast gate. |
| 2 | Acceptance criteria met | PASS | The diff adds one package-local `hangBudget` derived at exactly 1x `testutil.GoroutineRaceTimeout`, documents that it is a pure hang detector rather than a latency assertion, and converts exactly 14 direct floor-as-deadline waits. Every converted site waits for a goroutine-lifecycle signal; the scenario-input, polling, and negative-assertion timers remain unchanged. No direct `time.After(testutil.ExecRaceTimeout/GoroutineRaceTimeout)` use remains in the package. |
| 3 | Tests pass | PASS | The exact reviewed commit passed the focused package run (136 PASS, 0 FAIL, 0 SKIP), all seven tests containing changed waits passed by name, the required fast retry passed 10/10 jobs, and build, vet, workflow policy, and dashboard CI passed. The complete 40-job local CI union finished with 36 PASS jobs, 4 FAIL jobs, 0 SKIP jobs. Three non-diff failures are attributed below after exact-main reproduction; the fourth was the tracked SQLite cleanup race, did not reproduce on main, and was cleared only by a complete clean `make test-fast-parallel` retry whose unit-core lane passed. `diff_tests_executed` and attribution details are below. `waiver_ref: none`. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no security findings, blockers, HIGH, or CRITICAL findings. The change is test-only and introduces no production, dependency, authentication, data, configuration, or wire surface. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty at the reviewed commit after all test and dashboard-generation checks. This checklist is the deployer's only additional tracked change and is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main e1e5972b46655be23cca75620ed1926d81ed16f3` succeeded against current main and produced tree `9f93af739291dd658bc59f47aa4463d4be580d1f`; no self-rebase was needed. The ancestry-scope guard passed for deploy bead `ga-oet3z5` plus its confirmed build bead `ga-42mt5x.2`. |
| 7 | Single feature theme | PASS | The one-commit range changes only six test files in `internal/api/dashboardbff`: a shared hang-detector budget plus the 14 package-local wait conversions it serves. |

## Acceptance Evidence

- `git diff` removes 14 direct
  `time.After(testutil.GoroutineRaceTimeout)` deadlines and replaces each with
  `time.After(hangBudget)`.
- `internal/api/dashboardbff/hangbudget_test.go` defines the package-local
  constant at 1x the existing floor and documents why it must not be used for
  latency bounds or negative assertions.
- All converted waits guard lifecycle signal channels such as `started`,
  `oldDone`, `readyCh`, `doneCh`, and `startReturned`.
- A fresh package grep found no remaining direct
  `time.After(testutil.ExecRaceTimeout)` or
  `time.After(testutil.GoroutineRaceTimeout)` use.
- Existing scenario-input and negative/polling windows, including the
  package's explicit two- and five-second timers, were not widened.

## Test Evidence

`test_counts`:

- `go test -count=1 -json ./internal/api/dashboardbff/...`: 136 PASS,
  0 FAIL, 0 SKIP.
- `make test-local-full-parallel`: 40 jobs completed; 36 PASS, 4 FAIL,
  0 SKIP. The failures were all outside the diff and were either attributed
  under criterion 3a or cleared through the documented retry path below.
- `GO_TEST_TIMEOUT=30m make test-fast-parallel` retry: 10 PASS, 0 FAIL,
  0 SKIP jobs, including a clean `unit-core` lane.

`diff_tests_executed`:

- `TestRunTailerManagerRebindDiscardsInFlightEnrichment` PASS
- `TestRunProjectionSourceReturnsImmediatelyWhileColdLoadIsPending` PASS
- `TestPlaneStartDoesNotBlockOnColdLoad` PASS
- `TestRunDetailStreamFirstFrame` PASS
- `TestRunTailerColdLoadAndLiveTail` PASS
- `TestRunTailerManagerRebindsChangedEventsPath` PASS
- `TestRunTailerLogsColdLoadFailureOnce` PASS
- `hangbudget_test.go` adds shared test-only configuration and contains no test
  function of its own.

`skip_justification`: none needed; the focused diff-owned run reported 0 SKIP
and every changed-wait test passed by name.  
`waiver_ref`: none.

`failure_attribution`:

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The candidate does not touch
  `internal/bdflags`; the identical installed-`bd` flag drift reproduced on
  `origin/main@151168c76b7b5902676ade495c15b13b59186a8b`.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. The
  candidate does not touch `internal/runtime/tmux`; both empty-binding failures
  reproduced on the exact base ref.
- `TestSQLiteWriterFenceSIGKILLAtReservationBoundaries/WAL_without_SHM/reservation-open-pending`
  hit the contention-family cleanup race tracked by `ga-ggrykt`, but its exact
  base-ref run passed. It was therefore not attributed. A subsequent complete
  fast/unit retry passed 10/10 jobs, including `unit-core`.

`policy_lane`: `make test-ci-policy` PASS (5 runner-policy tests, 15 suite-
coverage tests, `scripts/cipolicy`, and the focused static-scope contract).

Additional required checks:

- `go build ./...` PASS
- `go vet ./...` PASS
- `make dashboard-ci` PASS
- `git diff --check origin/main...HEAD` PASS

## Commands Run

```text
git fetch origin main
gh api repos/gastownhall/gascity/commits/e1e5972b46655be23cca75620ed1926d81ed16f3/pulls
git merge-tree --write-tree origin/main e1e5972b46655be23cca75620ed1926d81ed16f3
git diff --check origin/main...e1e5972b46655be23cca75620ed1926d81ed16f3
go test -count=1 -json ./internal/api/dashboardbff/...
GO_TEST_TIMEOUT=30m make test-local-full-parallel
go test -count=1 -run '^TestSQLiteWriterFenceSIGKILLAtReservationBoundaries$' -v ./internal/storebinding/sqlite  # exact base; PASS
go test -tags integration -count=1 -run '^TestBdFlagManifestCurrent$' -v ./internal/bdflags  # exact base; reproduced FAIL
go test -tags integration -count=1 -run '^TestGetKeyBinding_CapturesDefaultBinding(WithArgs)?$' -v ./internal/runtime/tmux  # exact base; reproduced FAIL
GO_TEST_TIMEOUT=30m make test-fast-parallel
go build ./...
go vet ./...
make test-ci-policy
make dashboard-ci
```

## Decision

PASS. The reviewed commit satisfies all seven release criteria and is ready on
an isolated deploy branch for merge-authority review.
