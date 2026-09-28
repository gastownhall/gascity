# Release Gate: storebinding migration-guard hang budget

Deploy bead: `ga-d7ve5d`  
Review bead: `ga-i1r52s`  
Build bead: `ga-42mt5x.10`  
Reviewed source: `054f470452b7c145cbf7e02bd78be2a8115ddfc1`  
Base: `origin/main@7a36644ade7e2d21043dac51cc00937e238c2100`  
Gate date: 2026-08-20

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-i1r52s` is closed PASS for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | The migration-guard helper readiness wait now uses a documented 60-second hang budget derived as `6 * testutil.ExecRaceTimeout`; the deadline and diagnostic use the same value. Passing waits still return immediately. |
| 3 | Tests pass | PASS | The focused top-level storebinding suite reports 331 PASS groups, 0 FAIL, 0 SKIP under `-race`; all eight runnable tests in the modified file pass by name. The required 40-job union reports 35 PASS jobs, 5 FAIL jobs, 0 job-level SKIP. All six top-level failures are tracked and structurally unreachable from this package-local `_test.go` diff. `make test-ci-policy`, `go vet ./...`, and gofmt pass. |
| 4 | No high-severity review findings open | PASS | The reviewer found no security, correctness, or style issue; the change matches the established hang-budget precedent. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; `git diff --check origin/main...054f4704...` passes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 054f4704...` succeeded against `origin/main@7a36644a...`, producing `e5ed8be77ffcd391d69e6c58dab47644ec2783c7`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. |
| 7 | Single feature theme | PASS | One commit changes one `internal/storebinding` test file to apply the repository's hang-budget convention to one migration-guard child readiness wait. |

## Acceptance Evidence

- `migrationGuardHangBudget` is documented and derives from the central
  `testutil.ExecRaceTimeout` floor rather than duplicating a duration literal.
- `TestAcquireMigrationGuardExcludesSecondProcessAndRecoversAfterSIGKILL`
  uses the new budget in both the context deadline and timeout diagnostic.
- The changed SIGKILL recovery test passes under `-race` in 0.11s, confirming
  the value is an outer hang ceiling rather than a latency accommodation.
- No production file, wire type, configuration, persistence path, or runtime
  behavior changes.

## Test Evidence

```text
test_cmd: go test -race -v -count=1 -timeout 300s ./internal/storebinding
test_counts: 331 PASS groups, 0 FAIL, 0 SKIP (1,082 test/subtest starts)
diff_tests_executed: 8 PASS, 0 FAIL, 0 SKIP
  TestAcquireMigrationGuardClaimsAreRefCountedAndBoundToCityGeneration PASS
  TestMigrationGuardFileReleaserRetriesOnlyPendingStages PASS
  TestRejectedMigrationGuardCleanupRetainsRetryOwnershipBeforeFlock PASS
  TestAcquireMigrationGuardReturnsCleanupCapableGuardAfterCanceledAcquisitionCleanupFails PASS
  TestAcquireWriterFenceRejectsReplacedMigrationGuardDirectoryBeforeProviderMutation PASS
  TestRejectedWriterFenceCleanupReleasesOwnedClaimAfterDirectoryReplacement PASS
  TestAcquireMigrationGuardExcludesSecondProcessAndRecoversAfterSIGKILL PASS
  TestMigrationGuardHelperProcess PASS
waiver_ref: none for diff-owned tests
focused_log: /var/tmp/ga-d7ve5d-storebinding-focused.log

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 35 PASS jobs, 5 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.uRVygn

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
format_lane: gofmt check on changed Go file — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. None is diff-owned. The
candidate changes only `internal/storebinding/migration_guard_unix_test.go`;
Go compiles package-local `_test.go` files only into that package's test binary,
so this diff cannot execute in `cmd/gc`, `internal/bdflags`,
`internal/runtime/tmux`, or `test/integration`. The candidate-owned package
passed both the focused race run and the required union's `unit-core` lane.

- FAIL — attributed: `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill`
  -> `ga-hgjlhi`. The tracked five-second async-start bound expired under
  shard-parallel load. This occurrence was recorded and read back.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The installed
  `bd` flag surface is ahead of the checked-in manifest; this exact signature
  predates the candidate. The occurrence was recorded and read back.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. The host
  tmux default key table is empty; this exact signature predates the candidate.
  Both occurrences were recorded and read back.
- FAIL — attributed: `TestE2E_SuspendResume_City` -> `ga-yc0e3a`. The expected
  `citysus.report` did not arrive under load. The tracker contains exact
  candidate/base A/B evidence for this signature; this occurrence was recorded
  and read back.
- FAIL — attributed: `TestCleanInstallTutorialPath` -> `ga-hrdd3h`. Beads
  circuit-breaker cleanup diagnostics contaminated the expected `issue_prefix`
  stdout; this exact signature predates the candidate. The occurrence was
  recorded and read back.

```text
failure_attribution: TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill -> ga-hgjlhi + separate-package no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + separate-package no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-package no-mechanism proof
failure_attribution: TestE2E_SuspendResume_City -> ga-yc0e3a + prior exact candidate/base A/B + separate-package no-mechanism proof
failure_attribution: TestCleanInstallTutorialPath -> ga-hrdd3h + separate-package no-mechanism proof
```

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the reviewed source after the
final `origin/main` refresh. The target has not already merged or been
superseded through a PR, so normal isolated-branch deployment applies. The
shared builder branch has since moved; its newer tip was treated as provenance
only and was not used.

## Commands

```text
git fetch origin
git merge-tree --write-tree origin/main 054f470452b7c145cbf7e02bd78be2a8115ddfc1
assert_deploy_ancestry_scope origin/main 054f470452b7c145cbf7e02bd78be2a8115ddfc1 ga-d7ve5d ga-i1r52s ga-42mt5x.10
git diff --check origin/main...054f470452b7c145cbf7e02bd78be2a8115ddfc1
go test -race -v -count=1 -timeout 300s ./internal/storebinding
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go vet ./...
```
