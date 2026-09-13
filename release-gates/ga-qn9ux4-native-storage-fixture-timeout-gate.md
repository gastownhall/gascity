# Release Gate: native storage fixture boot timeout

Deploy bead: `ga-qn9ux4`  
Review bead: `ga-bqbwui`  
Build bead: `ga-uswva7`  
Reviewed source: `02c3c19ed33d68081e29ff7e9e9899ba11d157ee`  
Mayor-directed rebased source: `63e786ef83024ba1090f0717be7a9bf6cc216d91`  
Base: `origin/main@fae58763fc7c264d9ca74edc90529c3ea8260e95`  
Gate date: 2026-08-21

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-bqbwui` is closed with an unambiguous PASS for reviewed source `02c3c19...`. The two candidate patches have identical patch IDs after the mayor-directed rebase to `63e786ef...`. |
| 2 | Acceptance criteria met | PASS | Both real-Dolt `OpenNativeStorage` fixtures now use one named 60-second cold-boot budget instead of independent 15-second literals. A guard test enforces the 60-second floor, and production behavior is unchanged. |
| 3 | Tests pass | PASS | All three diff-owned tests report PASS by name. The required 40-job union reports 28 PASS jobs, 12 FAIL jobs, 0 job-level SKIP; its 13 raw top-level failures are tracked, occur in packages outside this `_test.go`-only diff, and are attributed below. Four dirty-schema failures remain raw FAIL and are explicitly WAIVED only under the mayor's recorded standing authorization. All six `cmd-gc-process` shards pass. `make test-ci-policy`, `go vet ./...`, `go build ./...`, gofmt, and `git diff --check` pass. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no security or correctness finding and only a non-blocking style preference. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; gofmt and `git diff --check origin/main...63e786ef...` pass. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 63e786ef...` succeeded against `origin/main@fae58763...`, producing tree `b7ce79f43a678fd3186b45286fa93930193935db`. `assert_deploy_ancestry_scope` passed for `ga-qn9ux4` and `ga-uswva7`. |
| 7 | Single feature theme | PASS | Two commits modify three `cmd/gc` test files for one fixture-startup timeout issue. No production or unrelated subsystem change is present. |

## Acceptance Evidence

- `nativeStorageFixtureBootTimeout` is a single named `60 * time.Second`
  fixture budget with a comment documenting the measured isolated and
  shard-contention behavior.
- `TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox` and
  `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` both use
  that shared budget around `beads.OpenNativeStorage`.
- `TestNativeStorageFixtureBootTimeoutSurvivesShardContention` prevents the
  budget from regressing below 60 seconds.
- The diff contains only `_test.go` files. It changes no production timeout,
  retry policy, API, configuration, storage schema, or runtime behavior.

## Test Evidence

```text
test_cmd: GC_FAST_UNIT=0 go test -count=1 -run '^(TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox|TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore|TestNativeStorageFixtureBootTimeoutSurvivesShardContention)$' -v -timeout 5m ./cmd/gc/...
test_counts: 3 PASS, 0 FAIL, 0 SKIP
diff_tests_executed:
  TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox PASS
  TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore PASS
  TestNativeStorageFixtureBootTimeoutSurvivesShardContention PASS
waiver_ref: none for diff-owned tests

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel
test_counts: 28 PASS jobs, 12 FAIL jobs, 0 job-level SKIP (40 total)
cmd_gc_process_shards: 6 PASS, 0 FAIL, 0 SKIP
full_logs: /var/tmp/gc-local-tests.XzPpJZ

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
build_lane: go build ./... — PASS
format_lane: gofmt check on all three changed Go files — PASS
diff_check: git diff --check origin/main...63e786ef83024ba1090f0717be7a9bf6cc216d91 — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. None is diff-owned. The
candidate changes only three `cmd/gc/*_test.go` files, which Go compiles only
into the `cmd/gc` test binary. It therefore cannot execute in
`internal/doctor`, `examples/gastown`, `internal/bdflags`,
`internal/runtime/tmux`, or `test/integration`. Those packages also have no
path overlap with the diff. All six candidate-owned `cmd-gc-process` shards
and all three changed tests passed.

- FAIL — attributed: `TestCustomTypesCheck_TableDrift` -> `ga-t33q83`.
  `t.TempDir` cleanup observed `directory not empty` in `internal/doctor`.
- FAIL — attributed: `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` ->
  `ga-yxgivi`. The tracked real-Dolt SIGKILL cleanup failure occurred in
  `examples/gastown`.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The installed
  `bd` flag surface differs from the checked-in manifest in `internal/bdflags`.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. The host's
  tmux 3.7b default binding behavior differs in `internal/runtime/tmux`.
- FAIL — attributed: `TestE2E_SuspendResume_City` -> `ga-yc0e3a`. The expected
  `citysus.report` did not arrive under integration load.
- FAIL — attributed: `TestDoltConfigWiringExternalHost` -> `ga-l4xwgh`
  (active fix/deploy for root tracker `ga-gajll3`). `bd init` timed out in the
  integration fixture.
- FAIL — attributed: `TestAdoptPRFormulaCompileAndRun` and
  `TestAdoptPRFormulaRetriesTransientReviewerStep` -> `ga-0qjpzc`. Both hit
  the tracked Dolt identity config-probe timeout in integration fixtures.
- RAW FAIL — WAIVED under the mayor's 2026-08-18 standing authorization on
  `ga-6bnc42`: `TestGraphWorkflowFailureRunsCleanup`,
  `TestGCLiveContract_BeadsAndEvents`, `TestHumaBinary_SessionMessageAsync`,
  and `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` hit the exact
  dirty-schema migration signature tracked by `ga-lpfjhc`. This occurrence,
  exact test list, candidate SHA, and log directory were appended to
  `ga-lpfjhc` and read back before scoring the gate.

```text
failure_attribution: TestCustomTypesCheck_TableDrift -> ga-t33q83 + separate-package no-mechanism proof
failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-yxgivi + separate-package no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + separate-package no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-package no-mechanism proof
failure_attribution: TestE2E_SuspendResume_City -> ga-yc0e3a + separate-package no-mechanism proof
failure_attribution: TestDoltConfigWiringExternalHost -> ga-l4xwgh + separate-package no-mechanism proof
failure_attribution: TestAdoptPRFormulaCompileAndRun -> ga-0qjpzc + separate-package no-mechanism proof
failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-0qjpzc + separate-package no-mechanism proof
failure_attribution: dirty-schema migration quartet -> ga-lpfjhc / ga-6bnc42 + mayor standing authorization + separate-package no-mechanism proof
```

### Rebase pre-push evidence

The canonical bounded-rebase helper produced candidate
`63e786ef83024ba1090f0717be7a9bf6cc216d91`. Its normal pre-push check ran
`make test-fast-parallel` and reported 9 PASS jobs, 1 FAIL job, 0 job-level
SKIP. The sole raw failure was
`internal/runtime/herdr.TestProviderLiveClaudeKindPath` with
`agent_pane_busy` on `w1:p1`. This separate-package host signature is covered
by `waiver_ref=mayor-2026-08-20-herdr-pane-standing`; the specific-head push
used an explicit SHA-pinned `--force-with-lease` and `--no-verify` under the
shared pre-push attribution protocol. Log:
`/var/tmp/gc-local-tests.iIkLix/unit-core.log`.

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the original reviewed source.
The target has not already merged or been superseded through a PR, so normal
isolated-branch deployment applies. The provenance branch was rebased and
pushed only under the mayor's explicit ruling; the deploy branch is cut from
the resulting exact SHA, not from a mutable branch tip.

## Commands

```text
git fetch origin main
git merge-tree --write-tree origin/main 63e786ef83024ba1090f0717be7a9bf6cc216d91
assert_deploy_ancestry_scope origin/main 63e786ef83024ba1090f0717be7a9bf6cc216d91 ga-qn9ux4 ga-uswva7
git diff --check origin/main...63e786ef83024ba1090f0717be7a9bf6cc216d91
GC_FAST_UNIT=0 go test -count=1 -run '^(TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox|TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore|TestNativeStorageFixtureBootTimeoutSurvivesShardContention)$' -v -timeout 5m ./cmd/gc/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel
make test-ci-policy
go vet ./...
go build ./...
```
