# Release gate: session-reconciler trace fixture shard timeout

- Deploy bead: `ga-cxihfu`
- Originating bead: `ga-hgjlhi`
- Build bead: `ga-rh76sz`
- Review bead: `ga-zuw79a`
- Reviewed commit: `5367ed9f66df271c3620396dcd995336761bd09b`
- Provenance branch: `builder/ga-hgjlhi`
- Base: `origin/main@c60a563ea3645b507cc3b97041731ed483e1fc59`
- Merge base: `e10ddee75edadb9bb1a18cbb3083ca2e3ab1a474`
- Deploy mode: remote
- Intended deploy branch: `deploy/ga-zuw79a-gate`
- Evaluated: 2026-09-13
- Verdict: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-zuw79a` records PASS for exact commit `5367ed9f66df271c3620396dcd995336761bd09b`; the deploy bead description and metadata agree on that SHA. |
| 2 | Acceptance criteria met | **PASS** | All three GH-1654 trace fixtures that call `waitForAsyncStarts` now set an explicit 30-second shutdown timeout. The two pool fixtures are guarded by `TestPoolTraceConfigsToleratesShardParallelLoad`, complementing the named-session guard. The change is confined to test fixtures and does not alter production defaults or add retries/sleeps. |
| 3 | Tests pass | **PASS WITH ATTRIBUTED FAILURES** | The documented 40-job full suite completed: 34 jobs PASS, 6 jobs raw FAIL, 0 omitted. All three diff-owned top-level tests ran in green `cmd/gc` shards and then passed three race-enabled focused repetitions. The six raw failures are non-diff-owned and satisfy all four attribution clauses through the predating tracker and structural mechanism proof below. |
| 3a | Pre-existing failures may be attributed | **PASS** | All six raw failures cite open tracker `ga-vkhfnj`, created 2026-08-29, whose consolidated history covers whole-suite subprocess contention and shared-Dolt fixture initialization failures. The candidate modifies only a package-local `cmd/gc` `_test.go` file; it cannot be compiled into `internal/storebinding/sqlite`, the `gc` production binary, or `test/integration`. No failing package overlaps the diff. Each occurrence was appended to the tracker during this gate. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `go build ./...`, `gofmt -l` on the changed file, `git diff --check origin/main...HEAD`, and `make check-hooks` all exited 0. |
| 3c | CI-config lane | **PASS** | `n/a`: no workflow, job, matrix, timeout, or required-check configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no style, security, or spec findings after the follow-up completed all three affected fixtures. The diff is test-only. |
| 5 | Final feature commit is clean | **PASS** | The exact reviewed SHA was evaluated in a clean detached worktree; `git status --porcelain` was empty before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and refreshed after the suite. The candidate is 4 commits ahead and 3 behind current main. `git merge-tree --write-tree origin/main 5367ed9f66df271c3620396dcd995336761bd09b` exited 0 with tree `edb9e4b5993525f3630483d0f8b1a127eece87ce`. No existing PR names this deploy bead or intended deploy branch. |
| 7 | Single feature theme | **PASS** | Four TDD commits modify only `cmd/gc/session_reconciler_trace_integration_test.go`. Every commit cites originating bead `ga-hgjlhi`; the ancestry scope guard passed with the deploy, review, build, and originating bead IDs. |

## Criterion 3 evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m $GC_CITY_ROOT/packs/actual/all/scripts/isolated-test-run.sh -- make test-local-full-parallel
test_cmd_scope: full-suite
job_counts: PASS=34 FAIL=6 OMITTED=0 TOTAL=40
raw_failing_tests: 6
aggregate_log: /var/tmp/ga-cxihfu-full-suite.log
job_logs: /var/tmp/gc-local-tests.lcBcbG
skip_justification: none required for diff-owned tests; all ran and none skipped. The package-level full runner does not emit an exhaustive per-test skip tally.
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
```

The isolation wrapper ran in pass-through mode, as designed for Gas City, and
its live-workspace tripwire did not fire. The repository does not currently
compile testcontainers-backed tests; the full suite exercised its configured
shared-Dolt paths directly.

Focused independent re-verification:

```text
go test ./cmd/gc/... -run 'TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates|TestNamedSessionPostKillTraceConfigToleratesShardParallelLoad|TestPoolTraceConfigsToleratesShardParallelLoad' -race -count=3 -v

TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates PASS x3
  named_session_post-kill PASS x3
  pool_respawn_after_drain PASS x3
  pool_grows_past_min_active_sessions PASS x3
TestNamedSessionPostKillTraceConfigToleratesShardParallelLoad PASS x3
TestPoolTraceConfigsToleratesShardParallelLoad PASS x3
  poolRespawnAfterDrainTraceConfig PASS x3
  poolGrowsPastMinActiveSessionsTraceConfig PASS x3

focused_counts: 24 PASS, 0 FAIL, 0 SKIP
```

`diff_tests_executed`:

- `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates`: PASS in the full process and integration package shards; PASS x3 with `-race`.
- `TestNamedSessionPostKillTraceConfigToleratesShardParallelLoad`: PASS in the full process and integration package shards; PASS x3 with `-race`.
- `TestPoolTraceConfigsToleratesShardParallelLoad`: PASS in the full process and integration package shards; PASS x3 with `-race`.

### Raw failure attribution

| Raw failing test | Tracker | Attribution evidence |
|---|---|---|
| `TestSQLiteWriterFenceSIGKILLAtReservationBoundaries/WAL_with_SHM/reservation-lock-shm-writers` | `ga-vkhfnj` (consolidated from `ga-ggrykt`) | Child `boundary-holder` returned an immediate helper-process failure instead of the readiness line. The tracker predates this run and documents this exact SIGKILL/reservation-boundary family under whole-suite load. Clause 3(a) lands structurally: a package-local `cmd/gc` test file cannot compile into `internal/storebinding/sqlite`; clause 4 has no package overlap. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `ga-vkhfnj` (exact test history consolidated from `ga-x48epi`) | Fixture `gc init` failed before formula execution because shared database `hq` was at v47 and `bd` refused 19 pending migrations to v66. The diff's `_test.go` code is absent from both the production `gc` binary and `test/integration`; clauses 3(a) and 4 pass. |
| `TestPersonalWorkFormulaCompileAndRun` | `ga-vkhfnj` | Same pre-execution shared-Dolt refusal, v56 to v66 (10 migrations). Same structural mechanism proof and no-overlap finding. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-vkhfnj` | Same pre-execution shared-Dolt refusal, v49 to v66 (17 migrations). Same structural mechanism proof and no-overlap finding. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `ga-vkhfnj` | Same pre-execution shared-Dolt refusal, v55 to v66 (11 migrations). Same structural mechanism proof and no-overlap finding. |
| `TestGraphWorkflowFailureRunsCleanup` | `ga-vkhfnj` | Same pre-execution shared-Dolt refusal, v7 to v66 (59 migrations). Same structural mechanism proof and no-overlap finding. |

```text
failure_attribution: TestSQLiteWriterFenceSIGKILLAtReservationBoundaries/WAL_with_SHM/reservation-lock-shm-writers -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestPersonalWorkFormulaCompileAndRun -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestGraphWorkflowFailureRunsCleanup -> ga-vkhfnj | clause 3(a) MECHANISM; clause 4 no overlap
```

The five shared-Dolt failures are one root condition rather than five new
defects. All failed during fixture initialization, before the behavior under
test. Tracker comments preserve each shard, schema-version pair, and log path.

## Static and policy evidence

Passed on exact reviewed commit `5367ed9f66df271c3620396dcd995336761bd09b`:

```text
make test-ci-policy
go vet ./...
go build ./...
gofmt -l cmd/gc/session_reconciler_trace_integration_test.go
git diff --check origin/main...HEAD
make check-hooks
```

`gofmt -l` and `git diff --check` produced no output. No API, OpenAPI,
dashboard, generated type, documentation, or CI workflow file changes, so the
dashboard, docs, and CI-lane-specific gates do not apply.

## Decision

Gate **PASS**. The reviewed change satisfies the stated scope and every
diff-owned test ran green in the full suite and the independent race-enabled
repetition. The six raw failures are retained rather than hidden, and each is
attributed under the repository's four-clause protocol to the predating
whole-suite/shared-Dolt condition tracker.

Cut isolated branch `deploy/ga-zuw79a-gate` from the exact reviewed SHA, commit
this evidence record, push only that isolated branch, open a PR against `main`,
and route the merge request to mayor/mpr. The deployer does not merge.
