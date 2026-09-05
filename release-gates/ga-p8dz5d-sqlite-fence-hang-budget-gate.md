# Release Gate: SQLite fence child hang budget

Deploy bead: `ga-p8dz5d`  
Review bead: `ga-91mxgs`  
Build bead: `ga-sptey3`  
Reviewed source: `d710bfee4d9aa0d5e9cb483a05c60be84c8ff0b3`  
Base: `origin/main@feee9ba8eade88450abd7cb8fc3890280c6f1e95`  
Gate date: 2026-08-20

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-91mxgs` closed PASS in round 2 for the exact reviewed source. The earlier request-changes concerned evidence, not code, and its close reason records that the evidence gap was resolved. |
| 2 | Acceptance criteria met | PASS | The package now derives a 60-second pure-hang-detector budget as `6 * testutil.ExecRaceTimeout`; both child protocol-line and process-wait deadline/message sites use it. Passing waits still return immediately, so production behavior and successful test duration are unchanged. |
| 3 | Tests pass | PASS | The focused SQLite suite reports 139 PASS, 0 FAIL, 0 SKIP under `-race`; all seven runnable tests in the modified file pass by name. The required 40-job union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. All six top-level failures are tracked and structurally unreachable from this package-local `_test.go` diff; the two beads#4566 tests are preserved below as FAIL — WAIVED. `make test-ci-policy`, `go vet ./...`, and gofmt pass. |
| 4 | No high-severity review findings open | PASS | The reviewer found the implementation technically sound and reported no security or code findings. The sole round-1 evidence issue was resolved in round 2. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; `git diff --check origin/main...d710bfee...` passes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main d710bfee...` succeeded against `origin/main@feee9ba8...`, producing `5c727593361612e4920b9990dd09829fb3e456c3`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. |
| 7 | Single feature theme | PASS | One commit changes one `internal/storebinding/sqlite` test file to apply the repository's established hang-budget convention to the two coupled fence-child wait helpers. |

## Acceptance Evidence

- `sqliteFenceHangBudget` is documented and derives from the central
  `testutil.ExecRaceTimeout` floor rather than duplicating a duration literal.
- `(*sqliteFenceChild).line` and `(*sqliteFenceChild).wait` both use the new
  budget in their context deadlines and diagnostic messages.
- No production file, wire type, configuration, persistence path, or runtime
  behavior changes.
- The formerly timing-sensitive
  `TestSQLiteLegacySnapshotSIGKILLAtBoundaries` passes in 25.75s; the graph,
  reservation-boundary, kernel-lock, source-census, and process-composition
  tests in the same file also pass.

## Test Evidence

```text
test_cmd: go test -race -v -count=1 -timeout 300s ./internal/storebinding/sqlite/...
test_counts: 139 PASS, 0 FAIL, 0 SKIP
diff_tests_executed: 7 PASS, 0 FAIL, 0 SKIP
  TestSQLiteFenceHelperProcess PASS
  TestSQLiteLegacySnapshotSIGKILLAtBoundaries PASS
  TestSQLiteGraphSnapshotSIGKILLAtBoundaries PASS
  TestSQLiteWriterFenceSIGKILLAtReservationBoundaries PASS
  TestSQLiteWriterFenceUsesExactKernelLockModes PASS
  TestSQLiteSourceCensusPermitsContinuousReaderMarkChurn PASS
  TestSQLiteWriterFenceProcessComposition PASS
waiver_ref: none for diff-owned tests
focused_log: /var/tmp/ga-p8dz5d-sqlite-focused.log

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.M7ChZr

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
format_lane: gofmt check on changed Go file — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. None is diff-owned. The
candidate changes only `internal/storebinding/sqlite/sqlite_fence_process_linux_test.go`;
Go compiles package-local `_test.go` files only into that package's test binary,
so this diff cannot execute in `cmd/gc`, `internal/bdflags`,
`internal/runtime/tmux`, or `test/integration`. The candidate-owned package
passed both the focused race run and the required union's `unit-core` lane.

- FAIL — WAIVED: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
  and `TestHumaBinary_SessionMessageAsync` failed before their assertions with
  the exact `gastownhall/beads#4566` dirty-table schema-migration signature.
  Root tracker: `ga-lpfjhc`; standing authorization: `ga-6bnc42`. Both new
  occurrences were recorded and read back on the tracker.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The installed
  `bd` flag surface is ahead of the checked-in manifest; this exact signature
  predates the candidate. The occurrence was recorded and read back.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. The host
  tmux default key table is empty; this exact signature predates the candidate.
  Both occurrences were recorded and read back.
- FAIL — attributed: `TestCleanInstallTutorialPath` -> `ga-hrdd3h`. Beads
  circuit-breaker cleanup diagnostics contaminated the expected `issue_prefix`
  stdout; this exact signature predates the candidate. The occurrence was
  recorded and read back.

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix + TestHumaBinary_SessionMessageAsync -> ga-lpfjhc + standing authorization ga-6bnc42 + exact beads#4566 signature + package-local test-only no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + separate-package no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-package no-mechanism proof
failure_attribution: TestCleanInstallTutorialPath -> ga-hrdd3h + separate-package no-mechanism proof
```

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the reviewed source after the
final `origin/main` refresh. The target has not already merged or been
superseded through a PR, so normal isolated-branch deployment applies.

## Commands

```text
git fetch origin
git merge-tree --write-tree origin/main d710bfee4d9aa0d5e9cb483a05c60be84c8ff0b3
assert_deploy_ancestry_scope origin/main d710bfee4d9aa0d5e9cb483a05c60be84c8ff0b3 ga-p8dz5d ga-91mxgs ga-sptey3
git diff --check origin/main...d710bfee4d9aa0d5e9cb483a05c60be84c8ff0b3
go test -race -v -count=1 -timeout 300s ./internal/storebinding/sqlite/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go vet ./...
```
