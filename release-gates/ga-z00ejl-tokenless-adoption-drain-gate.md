**Verdict:** **PASS**

# Release gate: adopted-runtime instance-token preservation and drain recovery

- Deploy bead: `ga-z00ejl`
- Originating bead: `ga-lfr06j`
- Build bead: `ga-3kfb6y`
- Review bead: `ga-22hdeo`
- Reviewed commit: `b7bf371557aad0f3dcc6e6d0fcfec40722dae91c`
- Provenance branch: `builder/ga-lfr06j`
- Base: `origin/main@af8844ad697961e6f9d0503b5a001fbb9adfc0cb`
- Merge base: `251318f5e7a280a9a0d0cf88da097863e3ddc982`
- Deploy mode: remote
- Intended deploy branch: `deploy/ga-z00ejl-gate`
- Evaluated: 2026-09-17

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-22hdeo` records a round-2 PASS for the exact deploy commit `b7bf371557aad0f3dcc6e6d0fcfec40722dae91c`. The deploy bead metadata, review notes, and provenance branch all resolve to that SHA. |
| 2 | Acceptance criteria met | **PASS** | `runAdoptionBarrier` now reads the running provider's `GC_INSTANCE_TOKEN` and records that exact value, leaving the field empty for a token-less runtime instead of minting an unrelated token. The regression coverage proves live-token preservation, token-less adoption, and successful later drain-ack stopping for both token shapes. No heuristic or time-based identity judgment was added, and the separate rollback leak remains untouched. |
| 3 | Tests pass | **PASS WITH ATTRIBUTED FAILURES** | The documented full 40-job local union completed: 32 jobs PASS, 8 jobs raw FAIL, 0 omitted. All six process-backed `cmd/gc` shards, all six integration `cmd/gc` shards, and all four candidate-added regression tests passed. The eight raw failures are non-diff-owned and satisfy the repository's attribution protocol through the two condition trackers detailed below. |
| 3a | Pre-existing failures may be attributed | **PASS** | One unit failure, `TestRunDetailStreamDeregistersOnDisconnect`, is tracked by `ga-nooc62`; it printed the deadline-edge signature `subscriber count = 0 ... want 0`, and `internal/api/dashboardbff` cannot import or reach the changed `cmd/gc` main package. Seven integration failures are tracked by predating condition tracker `ga-lejnse`; each stopped during fixture `gc init` on the known random-cursor shared-schema refusal before the test scenario or adoption barrier could run. No failing test file or package overlaps the diff. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `go build ./...`, `gofmt -l` on both changed Go files, `git diff --check origin/main...HEAD`, and `make check-hooks` all exited 0. `make lint-affected` and `make fmt-check-changed` reported no selected changed paths from the detached exact-SHA checkout, so they are not relied on as evidence. |
| 3c | CI-config lane | **PASS** | `n/a`: the diff changes no workflow, job, matrix, timeout, required-check list, or other CI configuration. |
| 4 | No high-severity review findings open | **PASS** | The reviewer records no security, style, or spec findings after the round-2 token-less-drain coverage was added. The only round-1 gap was that missing regression test, and round 2 closes it. |
| 5 | Final feature commit is clean | **PASS** | The exact reviewed SHA was evaluated in a clean detached checkout; `git status --porcelain=v2` reported no worktree changes before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and refreshed after the suite. `git merge-tree --write-tree --messages origin/main b7bf371557aad0f3dcc6e6d0fcfec40722dae91c` exited 0 with tree `207566398b1cde51e8e81bf685133408a7ec73ad`; no rebase was needed. |
| 7 | Single feature theme | **PASS** | The three-commit range changes only `cmd/gc/adoption_barrier.go` and `cmd/gc/adoption_barrier_test.go`, with one theme: preserve the actual runtime token (including token-less state) so an adopted survivor can later be drained. `assert_deploy_ancestry_scope` passed for `ga-z00ejl`, `ga-22hdeo`, `ga-3kfb6y`, and `ga-lfr06j`. |

## Criterion 3 evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m $GC_CITY_ROOT/packs/actual/all/scripts/isolated-test-run.sh -- make test-local-full-parallel
test_cmd_scope: full-suite
job_counts: PASS=32 FAIL=8 OMITTED=0 TOTAL=40
aggregate_log: /var/tmp/ga-z00ejl-full-suite.log
job_logs: /var/tmp/ga-z00ejl-shards
skip_justification: the sharded package runner does not emit an aggregate test-level SKIP tally; all diff-owned tests ran explicitly and none skipped
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
```

The isolation wrapper ran in Gas City's pass-through mode and its live-workspace
tripwire did not fire. The rootless podman socket was configured before the run;
Gas City's current suite exercised its configured shared-Dolt paths directly.

All six `cmd-gc-process-*` jobs and all six
`integration-packages-cmd-gc-*` jobs passed. Independent verbose
re-verification also passed all 26 top-level `TestAdoptionBarrier_*` groups:

```text
GC_FAST_UNIT=0 go test -count=1 -run '^TestAdoptionBarrier_' -v ./cmd/gc/...
focused_counts: 26 PASS, 0 FAIL, 0 SKIP
```

Diff-touched test results:

- `TestAdoptionBarrier_AdoptsRunning`: PASS
- `TestAdoptionBarrier_PreservesLiveInstanceToken`: PASS
- `TestAdoptionBarrier_TokenlessRuntimeAdoptsWithoutFabricatingToken`: PASS
- `TestAdoptionBarrier_AdoptedRuntimeCanLaterBeDrained`: PASS
- `TestAdoptionBarrier_TokenlessAdoptedRuntimeCanLaterBeDrained`: PASS

### Raw failure attribution

| Raw failing test | Tracker | Attribution evidence |
|---|---|---|
| `TestRunDetailStreamDeregistersOnDisconnect` | `ga-nooc62` | Clause 3(a), mechanism: `go list -deps ./internal/api/dashboardbff` contains no `cmd/gc` dependency, so the failing package cannot execute the changed production code. Clause 4 has no package/path overlap. The terminal count was already 0 after the fixed deadline, identifying the tracked poll-boundary race. The tracker was created from this discovering run under the `gm-sf3238` escape because clauses 1 and 4 are clear and the mechanism proof landed. |
| `TestPersonalWorkFormulaCompileAndRun` | `ga-lejnse` | Fixture `gc init` stopped on the tracked shared-schema refusal at v54→v66 before formula execution. Candidate adoption code was not reached. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `ga-lejnse` | Fixture `gc init` stopped on the same tracked refusal at v43→v66 before formula execution. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `ga-lejnse` | Fixture `gc init` stopped on the same tracked refusal at v43→v66 before formula execution. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-lejnse` | Fixture `gc init` stopped on the same tracked refusal at v61→v66 before supervisor/adoption behavior. |
| `TestGraphWorkflowSuccessPath` | `ga-lejnse` | Fixture `gc init` stopped on the same tracked refusal at v49→v66 before graph execution. |
| `TestGCLiveContract_BeadsAndEvents` | `ga-lejnse` | City initialization stopped on the same tracked refusal at v48→v66 before the live contract or adoption path. |
| `TestGraphWorkflowFailureRunsCleanup` | `ga-lejnse` | Fixture `gc init` stopped on the same tracked refusal at v48→v66 before graph execution. |

```text
failure_attribution: TestRunDetailStreamDeregistersOnDisconnect -> ga-nooc62 | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: seven shared-schema fixture-init failures -> ga-lejnse | clause 3(a) MECHANISM; clause 4 no overlap
```

The seven shared-schema failures are one root condition, not seven new
defects. Their test names, migration cursors, jobs, and log paths were appended
to `ga-lejnse`. The dashboard deadline race is retained separately as
`ga-nooc62`, which carries no routing label and must be promoted through one
reproduction-backed fix bead.

## Static and policy evidence

Passed on exact reviewed commit `b7bf371557aad0f3dcc6e6d0fcfec40722dae91c`:

```text
make test-ci-policy
go vet ./...
go build ./...
gofmt -l cmd/gc/adoption_barrier.go cmd/gc/adoption_barrier_test.go
git diff --check origin/main...HEAD
make check-hooks
```

`gofmt -l` and `git diff --check` produced no output. No API, OpenAPI,
dashboard, generated type, documentation, or CI workflow file changes are in
the reviewed range, so the dashboard, docs, and CI-lane-specific gates do not
apply.

## Decision

Gate **PASS**. The exact reviewed change satisfies the exit contract and every
diff-owned regression test ran green in both the required process-backed suite
and independent verbose re-verification. The eight raw failures remain visible
and are attributed under the repository's four-clause protocol to conditions
outside the candidate.

Cut isolated branch `deploy/ga-z00ejl-gate` from the exact reviewed SHA, commit
this gate record, push only that isolated branch, open a PR against `main`, and
route the merge request to mayor/mpr. The deployer does not merge.
