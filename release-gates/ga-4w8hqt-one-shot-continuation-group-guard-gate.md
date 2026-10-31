# Release gate: one-shot continuation-group fail-loudly guard

- Deploy bead: `ga-4w8hqt`
- Review bead: `ga-ly3g2z` — PASS
- Build bead: `ga-sj2h8f`
- Reviewed source: `f30ba511a9a33bffccd2b1f720a35f564f180191`
- Base: `origin/main@af596339dacee2ac472b041fec546b15f2665e33`
- Deploy mode: `remote`; push remote: `fork`
- Pre-flight: the reviewed source has no associated pull request, so it has not already merged
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ly3g2z` is closed with verdict `pass` and pins the exact reviewed source `f30ba511a9a33bffccd2b1f720a35f564f180191`. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | `GraphRouteBinding.IndependentSteps` is a documented zero-value-safe field. `ApplyGraphRouteBinding` now refuses to erase a non-empty, non-`drain:` continuation group from an independent step, names both the step and group, and leaves the metadata intact on error. Router-owned `drain:` values and empty values still clear normally. `AssignGraphStepRoute` and all four production callers propagate the error. The new guard and propagation tests pass, and the mechanically updated existing tests retain their prior behavior. |
| 3 | Tests pass | **PASS with seven attributed raw failures** | The documented isolated full local CI union completed **33/40 jobs green**, with **46,999 PASS / 7 raw FAIL / 225 SKIP** top-level test executions. All seven failures are attributed below under criterion 3a to the pre-existing concurrent-initializer/schema-refusal condition tracked by `ga-e2z1zb`. All 10 diff-owned top-level tests passed in both selecting tiers: **20 PASS / 0 FAIL / 0 SKIP**; the new independent-step table's four cases passed in both tiers as well. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. The independent build, policy lane, vet, formatting, and hook checks pass. |
| 4 | No high-severity review findings open | **PASS** | The review records no style, security, or specification blockers and no unresolved HIGH finding. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain=v1` was empty at the reviewed source. `git diff --check origin/main...f30ba511a9a33bffccd2b1f720a35f564f180191` and `gofmt -l` on all four changed files were also empty. This checklist is committed on the isolated deploy branch in the next step. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main`, `git merge-tree --write-tree origin/main f30ba511a9a33bffccd2b1f720a35f564f180191` exited 0 and produced tree `eb3982328bf99d453e150f61e2aff1592f57cc6b`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two feature commits form one routing-safety theme: detecting a continuation-group contract that one-shot pool routing cannot honor and propagating that refusal. The ancestry scope guard passes for deploy bead `ga-4w8hqt` plus its confirmed build bead `ga-sj2h8f`; no unrelated commit or `.claude/**` path is present. |

## Acceptance checks

- `GraphRouteBinding.IndependentSteps` is documented and defaults to `false`, so existing callers retain their behavior until one-shot routing sets it.
- `ApplyGraphRouteBinding` returns an error only when all three conditions hold: `IndependentSteps` is true, the existing continuation group is non-empty, and the group is not router-owned via the `drain:` prefix.
- The refusal names `wf.pooled-step` and `review-chain` and preserves both `gc.continuation_group` and `gc.session_affinity` for diagnosis.
- Empty groups and both `drain:<id>` and `drain:<id>:<suffix>` values clear without error.
- `AssignGraphStepRoute`, `DecorateGraphWorkflowRecipe`, and `decorateDynamicFragmentRecipe` propagate the error rather than swallowing it.
- The full-suite run selected every changed top-level test twice:
  - `TestAssignGraphStepRoute_ControlBindingUsesRoutedQueueWithoutAssignee`
  - `TestAssignGraphStepRoute_ControlBindingPreservesDirectExecutionRoute`
  - `TestApplyGraphRouteBinding_PoolRouted_ContinuationGroupIsFormulaOptIn`
  - `TestApplyGraphRouteBinding_IndependentSteps`
  - `TestAssignGraphStepRoute_PropagatesIndependentStepsError`
  - `TestApplyGraphRouteBinding_SingleSession_NoAffinityKeys`
  - `TestApplyGraphRouteBinding_PoolRouted_DoesNotSetSessionName`
  - `TestApplyGraphRouteBinding_StampsSessionName`
  - `TestApplyGraphRouteBinding_PoolMetadataOnly_NoSessionName`
  - `TestApplyGraphRouteBinding_DirectSession_StampsSessionID`
- Candidate paths are exactly:
  - `cmd/gc/cmd_convoy_dispatch.go`
  - `internal/graphroute/graphroute.go`
  - `internal/graphroute/graphroute_test.go`
  - `internal/graphroute/session_stamp_test.go`

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-4w8hqt-test.KLFSMG/jobs make test-local-full-parallel`, invoked through `packs/actual/all/scripts/isolated-test-run.sh`.
- `test_cmd_scope: full-suite`
- `test_counts: 46,999 PASS / 7 raw FAIL / 225 SKIP`; job result `33 PASS / 7 raw FAIL / 0 omitted`.
- `required_job_coverage`: `unit-core` PASS; all six `cmd-gc-process` shards ran, with five green and the single failure attributed below; `TestTutorial01` itself PASSed in the failing process shard. `productmetrics-testhook` PASSed. All 13 integration-package core/`cmd/gc`/tmux shards PASSed. Review-formula coverage completed four green jobs plus one attributed failure; `integration-bdstore` PASSed; REST coverage completed five green jobs plus five attributed failures.
- `diff_tests_executed: 20 top-level PASS / 0 FAIL / 0 SKIP` — each of the 10 changed top-level tests passed in both `unit-core` and `integration-packages-core-3-of-4`; the four new table cases contributed eight additional passing subtest executions.
- `skip_justification`: the 225 skips are existing suite-controlled platform, live-provider, opt-in persistence, helper-process, or fast-tier exclusions. The full union also ran the process-backed and integration tiers that own those boundaries. None is diff-owned.
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change in this diff)`
- `policy_lane: make test-ci-policy — PASS`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `make check-hooks` — PASS (`core.hooksPath` is `.githooks`)
- `gofmt -l` on all changed Go files — empty/PASS
- Rootless Podman 5.8.4 was live before the run. The exact pinned images were refreshed successfully: `dolthub/dolt-sql-server:1.32.4@sha256:1aab3e333d8f9e3cf52a6ba7abf944865364adeef881c0550557019b87cee9d5` and `dolthub/dolt:2.1.7@sha256:22319531c51c2fb2ca3639ad284d0ff9a98b55c25c6ba4ebeefbf7769e663916`. Ryuk remained disabled as required on this host.
- Full log: `/var/tmp/ga-4w8hqt-test.KLFSMG/full-rerun.log`
- Per-job logs: `/var/tmp/ga-4w8hqt-test.KLFSMG/jobs`
- Focused same-package coverage proof: `/var/tmp/ga-4w8hqt-test.KLFSMG/fresh-managed-coverage.log` and `/var/tmp/ga-4w8hqt-test.KLFSMG/fresh-managed.cover`
- Build, policy, vet, and hook logs: `/var/tmp/ga-4w8hqt-test.KLFSMG/{build,policy,vet,hooks}.log`

### Criterion 3a failure attribution

All seven failures stopped during `gc init` / beads initialization because a second initializer observed a partially migrated shared `hq` database and `bd` refused to auto-apply the remaining migrations. Tracker/fix bead `ga-e2z1zb` was created on 2026-09-13, before this run, and contains the reproduced random-`vNN -> v66` root cause and fix specification. This run's seven sightings were appended to that bead and read back successfully.

| Failure | Attribution |
|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `failure_attribution: ... -> ga-e2z1zb | clause 3(c): COVERAGE — PASS`. This is the sole same-package case: the focused isolated test passed, and coverage reports the only changed `cmd/gc` function, `decorateDynamicFragmentRecipe`, at 0.0%. `clause-4-guard: same_package=yes proof=c added_test_load=no` — no resource-census or build-target change, and no new test file was added in `cmd/gc`. |
| `TestGraphWorkflowSuccessPath` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture failed in beads initialization before graph workflow dispatch. Its file is under `test/integration`, with no path overlap. |
| `TestHumaBinary_CityCreateAsync` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The asynchronous city request emitted `city_init_failed` from beads initialization before any graph-route decoration. Its file is under `test/integration`, with no path overlap. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. `setupReviewFormulaCity` failed during beads initialization before the retry formula ran. Its file is under `test/integration`, with no path overlap. |
| `TestGCLiveContract_BeadsAndEvents` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. City creation failed in beads initialization before the live contract began. Its file is under `test/integration`, with no path overlap. |
| `TestHumaBinary_SessionMessageAsync` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. City creation failed in beads initialization before session messaging began. Its file is under `test/integration`, with no path overlap. |
| `TestGraphWorkflowFailureRunsCleanup` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture failed in beads initialization before graph workflow failure handling or cleanup ran. Its file is under `test/integration`, with no path overlap. |

The seven raw failures therefore satisfy all four non-diff-owned failure clauses and do not weaken the independent evidence for the changed routing tests, which all executed and passed twice.
