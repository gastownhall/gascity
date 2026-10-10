# Release Gate: Dolt cleanup container and GOTMPDIR protection

- Deploy bead: `ga-hlp7ut`
- Build bead: `ga-sm1cvj`
- Review bead: `ga-u65zzv`
- Reviewed source: `925939b7349490c1ff18bb1fc4a1080c31d30887`
- Merge base: `fc7a23a292dc50b9a2251e90b76ab4d467c799f1`
- Base evaluated: `origin/main@c9b1eaebfadb31ed7a204d8c00ea3f6d5e90e405`
- Deploy mode: remote
- Date: 2026-09-05
- Overall verdict: **PASS**

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` and `work-packages/` are absent at the reviewed
source; this gate therefore uses the deployer release criteria together with
`TESTING.md`, the Makefile, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-u65zzv` records a fresh PASS with no findings and pins the reviewed source exactly to `925939b7349490c1ff18bb1fc4a1080c31d30887`. |
| 2 | Acceptance criteria met | PASS | Linux process discovery records Docker/Podman cgroup ownership; cleanup protects container-managed bare Dolt servers before considering host test-path ownership; `/var/tmp/gotmp/Test*` and `$GOTMPDIR/Test*` are recognized; the human and typed JSON reports explain protected PIDs while retaining `protected_pids` and schema v1; existing rig-port, active-test-root, and managed-config protections remain intact. |
| 3 | Tests pass | PASS WITH ATTRIBUTED FAILURES | The documented `make test-local-full-parallel` union ran all 40 jobs: 36 PASS jobs, 4 raw FAIL jobs, and 0 skipped/omitted jobs. All six non-short `cmd/gc` process shards and all six `cmd/gc` integration shards passed. A supplemental exact-name run confirmed all 34 top-level tests in the modified test file PASS, 0 FAIL, 0 SKIP, with 12 PASS subtests. The raw failures are non-diff-owned and attributed below. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS WITH ATTRIBUTED LINT DIAGNOSTICS | Build, vet, changed-file formatting, CI-policy, docs-sync, module/dependency/event-export/core-boundary, and native-DoltLite checks passed. `lint-affected` reported six pre-existing diagnostics in five unchanged `cmd/gc` files; their exact base/head blob identity and trackers are recorded below. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (the candidate does not change a CI job, matrix, timeout, required-check list, or test target)`. |
| 4 | No high-severity review findings open | PASS | The reviewer found no security, correctness, architecture, or test-coverage issue. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Before this checklist was created, `git status --short` was empty, `git diff --check` passed, and repository hooks resolved to `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 925939b7349490c1ff18bb1fc4a1080c31d30887` exited 0 and produced tree `f9a9109985f42b8ad7d6903e3d8ca235adaf5452`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Four files implement and test one safety improvement: prevent host PID cleanup from acting on container-managed Dolt servers and recognize the fleet's Go test scratch root. The additive report detail supports that same operator workflow. |

## Criterion 2: acceptance evidence

1. `discoverDoltProcesses` now reads `/proc/<pid>/cgroup` under the existing
   bounded read helper and records `podman` for `libpod-` markers or `docker`
   for `docker-` markers. Missing, unreadable, or unrecognized cgroup data
   remains an empty, protect-neutral signal.
2. `classifyDoltProcess` protects a bare container-managed server before the
   host `--data-dir` allowlist path. Its reason identifies the container
   runtime and directs the operator to remove the container with the runtime
   CLI instead of killing a host-visible PID.
3. `isTestConfigPath` recognizes the configured fleet root
   `/var/tmp/gotmp/Test*` and an alternate `$GOTMPDIR/Test*`, preserving the
   existing `/tmp`, `os.TempDir`, and home `.gotmp` rules.
4. `CleanupReapedReport.Protected` adds typed PID, reason, and optional runtime
   detail. `ProtectedPIDs` remains present for compatibility, empty slices still
   marshal as `[]`, and `CleanupSchemaVersion` remains
   `gc.dolt.cleanup.v1`. Human output uses the recorded specific reason.
5. Revalidation carries the same structured protection when process identity
   or ownership changes before a signal. The reaper still fails closed for
   unknown processes and independently verifies a data directory before
   removal.
6. The complete modified test file passed, including
   `TestContainerDoltServerIsClassified`,
   `TestGotmpdirTestConfigIsOnAllowlist`, and
   `TestClassifyDoltProcess_ProtectsRealManagedConfig`.

## Criterion 3: full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true"
LOCAL_TEST_LOG_DIR=/var/tmp/ga-hlp7ut-gate.UObEsE/jobs
LOCAL_TEST_JOBS=2
CMD_GC_PROCESS_TOTAL=6
GO_TEST_TIMEOUT=30m
make test-local-full-parallel
```

The rootless Podman socket and the pinned Dolt SQL Server image were present
before the run. Complete runner log:
`/var/tmp/ga-hlp7ut-gate.UObEsE/full-suite.log`.

- `test_cmd_scope: full-suite`
- job counts: 36 PASS, 4 raw FAIL, 0 skipped/omitted
- `diff_tests_executed`: 34/34 top-level tests PASS, 0 FAIL, 0 SKIP;
  12 nested subtests also PASS
- supplemental diff-owned log:
  `/var/tmp/ga-hlp7ut-gate.UObEsE/diff-owned-tests.log`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

The required command covered the full local CI union, including these
load-bearing process/integration jobs:

- all 6 non-short `cmd/gc` process shards: PASS
- all 6 integration `cmd/gc` shards: PASS
- all 3 runtime/tmux shards: PASS
- both formula-basic and both formula-retry shards: PASS
- both REST smoke and all 8 REST full shards: PASS
- native filesystem cross-compile and test-harness self-tests: PASS

`skip_justification`: zero full-suite jobs and zero diff-owned tests skipped.
The full job logs were not run with verbose per-test output, so the aggregate
above reports the runner's authoritative job counts. The exact-name
supplement records every test in the modified test file individually.

### Attributed raw test failures

Each pre-existing tracker was opened before citation, received this run's
sighting, and was read back. None of the failing test files overlaps the
candidate diff.

- `TestResidencyBoundaryGrepRatchet` -> `ga-zohuic` (clause 3(a), mechanism).
  The candidate cannot modify the residency baseline or migration inventory;
  the reviewed SHA predates the fix already on current `origin/main`. This
  occurred in `unit-core` and `integration-packages-core-1-of-4`.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo` (clause 3(a), mechanism). The test
  compares the independently installed host `bd` command with the checked-in
  flag manifest. The candidate changes neither that binary nor the manifest.
- `TestProviderLiveClaudeKindPath` -> `ga-iepsvr` (clause 3(a), mechanism).
  The failure is the tracker's exact live herdr pane-contention condition;
  `internal/runtime/herdr` cannot import or execute the changed `cmd/gc`
  cleanup implementation.
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` ->
  `ga-esyijp` (clause 3(a), mechanism). The test failed during `gc init` on
  the tracked Beads dirty-schema migration condition, before its recovery
  behavior or any cleanup command path executed.

`failure_attribution: TestResidencyBoundaryGrepRatchet -> ga-zohuic;
TestBdFlagManifestCurrent -> ga-f0uceo; TestProviderLiveClaudeKindPath ->
ga-iepsvr; TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash ->
ga-esyijp (all clause 3(a) mechanism proofs)`.

## Criterion 3b: policy and lint evidence

- `go build ./...` — PASS.
- `make vet` (`go vet ./...`) — PASS.
- `LINT_CHANGED_REF=fc7a23a292dc50b9a2251e90b76ab4d467c799f1
  LINT_CHANGED_SCOPE=tracked make fmt-check-changed` — PASS.
- `make test-ci-policy` — PASS.
- `make check-docs` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_REF=fc7a23a292dc50b9a2251e90b76ab4d467c799f1
  LINT_CHANGED_SCOPE=tracked make lint-affected` — RAW FAIL, attributed below.

The affected-package linter correctly selected `./cmd/gc`, then reported
three `gofumpt` findings and three `SA1019` findings in files outside the
candidate diff:

- Unchanged-file `gofumpt` findings -> `ga-d3m213` (clause 3(a), mechanism).
  `controller_stop_client_test.go`, `dolt_cleanup_human_test.go`, and
  `error_store.go` have identical blobs at the merge base and reviewed source.
  Formatting in byte-identical files cannot have been introduced by this
  candidate. The tracker was created under the protocol's same-run escape only
  after this proof and the no-file-overlap check landed.
- Unchanged `ResolvedProvider.Kind` `SA1019` uses -> `ga-t88402` (clause 3(a),
  mechanism). `session_beads_test.go`,
  `session_lifecycle_parallel_test.go`, and the deprecated declaration in
  `internal/config/config.go` have identical base/source blobs. The candidate
  neither adds the uses nor changes the declaration. The same-run tracker was
  created only after the mechanism and no-file-overlap proofs landed.

Complete static/policy log:
`/var/tmp/ga-hlp7ut-gate.UObEsE/policy.log`.

## Decision

**Gate PASS.** Cut the isolated `deploy/ga-hlp7ut-gate` branch from the exact
reviewed source, commit this checklist, push it to the fork, open a pull request
against `gastownhall/gascity:main`, publish deploy clearance on the exact gated
PR head, and route the merge request to the merge authority. The deployer does
not merge.
