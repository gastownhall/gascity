# Release gate: PR #5517 refresh onto main

- Refresh task: `ga-ahmbow`
- Original deploy bead: `ga-ox9nm0`
- Build bead: `ga-42mt5x.5`
- Review bead: `ga-i9pvzc`
- Reviewed source commit: `093b95a7b0a770706c4879cfb43d59cbd42dd637`
- Previous PR head: `0c316b9c6c103a26cdf7d30dbd454936320f2b7b`
- Refresh base: `fc6b10df1756d3a58fe144a6eb28ed83e9b18411`
- Evaluated head: `8c9ea3d9c368a1ccfcb41088e69b89afc0a199b3`
- Deploy branch: `deploy/ga-ox9nm0-gate`
- Pull request: `#5517`
- Deploy mode: remote
- Evaluated: 2026-09-06
- Verdict: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-i9pvzc` is closed with an explicit PASS verdict for source commit `093b95a7b0a770706c4879cfb43d59cbd42dd637`. The refresh is a GitHub merge of that unchanged reviewed source and its existing gate artifact onto `main@fc6b10df1756d3a58fe144a6eb28ed83e9b18411`. |
| 2 | Acceptance criteria met | **PASS** | The source diff remains test-only: one package-local `hangBudget = 6 * testutil.GoroutineRaceTimeout` and ten pure hang-detector conversions in `internal/api`. The refreshed base also contains PR `#5490`'s independent `internal/credentialprovider/hangbudget_test.go`; the full suite exercised the resulting combined tree. No production implementation, shared timeout, scenario-input timeout, negative-assertion window, API wire type, OpenAPI artifact, dashboard source, CI configuration, or test-resource census changed. |
| 3 | Tests pass | **PASS (5 attributed failures)** | The documented CI-equivalent union completed all 40 jobs on refreshed head `8c9ea3d9c368a1ccfcb41088e69b89afc0a199b3`: **35 PASS and 5 raw FAIL jobs**. It ran the fast/unit baseline, all six `cmd/gc` process shards, all integration package shards, bd-store, review-formula suites, REST smoke, and all eight REST-full shards. GitHub CI on the same head is green, including `CI / required`, twelve `cmd/gc process` shards, four `packages-core` shards, `packages-cmd-gc-integration`, dashboard, static, generated artifacts, acceptance A, bd CLI contract, CodeQL, and govulncheck. `diff_tests_executed`: all four changed named tests passed in both `unit-core` and `integration-packages-core-3-of-4` (eight executions, zero failures or skips). `waiver_ref`: none consumed. The five tests named by the 2026-08-22 mayor waiver all passed, so the re-use preauthorization in `ga-ahmbow` was not invoked. |
| 3a | Pre-existing failures attributed | **PASS** | Every raw failure is outside the four-file candidate diff, has a pre-existing tracker with the same signature on unrelated candidates, and changes neither the failing test nor its mechanism. `failure_attribution`: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` -> `ga-esyijp` (beads issue #4566 leaves `child_counters` dirty during fixture initialization); `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` -> `ga-vkhfnj` (removed Dolt directory remains held open); `TestBdFlagManifestCurrent` -> `ga-f0uceo` (installed `bd` manifest drift); `TestRemoteContextProviderCachesAndForceRefreshes` -> `ga-vkhfnj` via consolidated tracker `ga-unukb3` (credential-provider deadline); `TestCustomTypesCheck_TableDrift` -> `ga-aik16g` (`eventsData` writer/cleanup race). Current sightings were appended to and read back from all four open trackers. |
| 3b | Policy, static, and dashboard lanes | **PASS (attributed baseline lint)** | `make dashboard-ci`, `go vet ./...`, `make test-ci-policy`, `make build`, binary version/help smoke, `git diff --check`, changed-file formatting, module-replace, native-dependency, event-export isolation, core-boundary, native DoltLite, and docs checks passed. A clean-worktree `lint-affected` run at the exact candidate found seven non-diff-owned findings; every finding is present on the exact refresh base's full lint run and tracked by `ga-d3m213`, `ga-t88402`, or `ga-egkp4x`. Three additional diagnostics seen only after `dashboard-ci` were under ignored, untracked `node_modules`; the clean candidate run excluded that generated dependency tree. |
| 3c | CI configuration lane | **N/A** | The candidate changes no workflow, CI script, Makefile, test-census, or runner-policy input. |
| 4 | No high-severity review findings open | **PASS** | Review `ga-i9pvzc` records no security blocker or unresolved high-severity finding. GitHub's refreshed-head automated maintainer review also returned `auto-merge`. |
| 5 | Final branch is clean | **PASS** | The branch was clean after the full suite, dashboard generation, static checks, and ancestry guards and before this gate artifact was added. Generated API/dashboard checks left no tracked drift. |
| 6 | Branch diverges cleanly from main | **PASS** | GitHub refreshed the PR from old head `0c316b9c6c103a26cdf7d30dbd454936320f2b7b` onto `main@fc6b10df1756d3a58fe144a6eb28ed83e9b18411`, producing head `8c9ea3d9c368a1ccfcb41088e69b89afc0a199b3`. `assert_deploy_ancestry_scope` and `assert_reviewed_sha_present` passed. A final merge-tree check against the subsequently advanced `origin/main@006b5fd7bf` produced tree `63b96505b3ffba0d502ce773af54fd65ad57c486` without conflict; GitHub reports the PR `MERGEABLE` and `CLEAN`. |
| 7 | Single feature theme | **PASS** | One test-reliability theme in one subsystem: consolidating pure hang-detector ceilings in `internal/api` tests. The only additional branch content is release-gate evidence and the mechanical refresh merge from main. |

## Test evidence

- Required union: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/gc-deploy-ga-ahmbow-full-20260906 make test-local-full-parallel` — **35 PASS and 5 raw FAIL jobs**; logs are in `/var/tmp/gc-deploy-ga-ahmbow-full-20260906/`.
- Diff-owned tests passed twice each: `TestStructuredPeekEmitsSameRequestPendingUpdates`, `TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect`, `TestHandleSessionStreamStructuredResumesFromPaginatedRESTSnapshot`, and `TestHandleSessionStreamStructuredResumesFromEmptyPaginatedRESTSnapshot`.
- Raw failure logs: `cmd-gc-process-2-of-6`, `integration-packages-core-1-of-4`, `integration-packages-core-2-of-4`, `integration-packages-core-4-of-4`, and `integration-packages-cmd-gc-6-of-6`.
- Top-level Go test accounting across the job logs: 47,929 PASS, 5 FAIL, and 209 SKIP; nested/subtest accounting: 80,456 PASS, 10 FAIL, and 304 SKIP.
- `skip_justification`: skips are the suite's platform/privilege/live-provider/opt-in/helper and repeated-package shard controls. No diff-owned test skipped, and all path-required CI jobs executed locally or passed on GitHub at the evaluated head.
- Dashboard preview: not applicable. The candidate changes only Go test files and a release-gate artifact; it changes neither SPA source nor API schema/wire behavior. The mandatory production build, type checks, dashboard Go tests, and generated-client refresh all passed without tracked drift.

## Disposition

The stale PR was refreshed onto main and all seven criteria pass. The candidate-owned tests and GitHub required checks are green; the five local runner failures are fully attributed to pre-existing conditions outside the diff. Commit this refresh gate on `deploy/ga-ox9nm0-gate`, push the exact gated head, publish `release-gate/deploy-clearance=success`, and send a machine-actionable merge request to the mayor.
