# Release gate: orders-feed and partial-result status correctness

- Deploy bead: `ga-xe7biz`
- Build bead: `ga-h8q2aj`
- Review bead: `ga-7wjddi`
- Reviewed commit: `5f11f2c076c6863df7fc21c75cd030236af3b0da`
- Base: `origin/main@09bae7ad1706aced67a72775d2b1d11549002cd0`
- Merge base: `09bae7ad1706aced67a72775d2b1d11549002cd0`
- Deploy mode: remote
- Gate verdict: **PASS**

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-7wjddi` is closed with a PASS verdict pinned to the exact reviewed commit; the independent review records no style, security, or specification finding. |
| 2 | Acceptance criteria met | **PASS** | Partial-result fixtures now use canonical `open` status; `ListTracking(limit)` includes closed runs while pushing a created-desc limit to the backing store; the feed threads its normalized limit and memoizes `LatestOpenRun` per scoped order and per store. `LatestOpenRun` remains open-only. No wire, event, OpenAPI, or generated-client shape changed. |
| 3 | Tests pass | **PASS** | Fifteen focused acceptance/regression tests passed. The required 40-job union completed 35 PASS / 5 FAIL / 0 SKIP; every failure is independently attributed below to a pre-run tracker with direct base/mechanism evidence, and no candidate-owned failure remains. Policy, lint, vet, formatting, docs, boundary, native DoltLite, dashboard, TypeScript, and preview-smoke lanes passed. |
| 4 | No unresolved HIGH findings | **PASS** | The reviewer found no security or HIGH-severity issue. The feed continues using typed `beads.ListQuery` values and keeps the cache inside each store iteration, preventing cross-store result reuse. A repository-wide affected lint completed with 0 issues. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty after the full union, static gates, dashboard generation/build, and live preview smoke, before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 5f11f2c076c6863df7fc21c75cd030236af3b0da` returned rc 0 and tree `97e157e1b6d2f01c34d68d4823129de27d804efc`. The candidate is a direct two-commit child of current `origin/main`. |
| 7 | Single feature theme | **PASS** | All nine changed files implement one behavior: stop relying on MemStore's former forced-open status while preserving complete, bounded orders-feed and partial-result behavior. |

## Acceptance evidence

- `internal/api/handler_beads_partial_test.go` uses canonical `Status: "open"` for both seeded rows.
- `internal/orders/store.go` exposes `ListTracking(limit int)` with `IncludeClosed: true`, caller `Limit`, and `AllowBackingCreatedLimit: true`; its comment distinguishes this from the deliberately open-only `LatestOpenRun` contract.
- `internal/api/huma_handlers_orders.go` passes the normalized request limit into `buildOrderRunFeedItems` instead of fetching an unbounded corpus and slicing afterward.
- `internal/api/orders_feed.go` scopes `latestOpenCache` inside each store iteration and keys it by scoped order name, preserving store isolation while eliminating repeated lookups.
- `internal/storebinding/storebinding.go` updates the typed `OrdersStore` interface consistently.
- The diff contains no OpenAPI JSON, generated client, dashboard, wire type, route registration, or event-payload change.

## Test evidence

- Focused named acceptance/regression command over `./internal/api/... ./internal/orders/...` — **15 PASS, 0 FAIL, 0 SKIP**.
- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel` — **35/40 jobs PASS, 5/40 jobs FAIL, 0 tests SKIP**. Logs: `/var/tmp/gc-local-tests.U5aDQ4`.
- `make test-ci-policy` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `GOLANGCI_LINT_CACHE=<isolated-on-disk-cache> LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues.
- `make fmt-check-changed` — PASS.
- `make check-docs` — PASS.
- `go vet ./...` — PASS.
- `make dashboard-ci` — PASS: dashboard build, shared/frontend/test/e2e TypeScript checks, dashboard Go tests, and generated-client projection all completed successfully.
- Dashboard preview smoke — PASS: the built frontend served valid HTML from `http://127.0.0.1:4175/` before the preview process was stopped.
- `make check-native-dependency-surface` — raw FAIL: candidate binary 270,246,136 bytes exceeds the 270,000,000-byte ceiling. Exact current base `origin/main@09bae7ad17` measured 270,291,592 bytes, 45,456 bytes larger than the candidate, and the diff adds no dependency. Tracked as `ga-5flk3r`; this occurrence is attributed rather than rewritten green.

`test_cmd_scope`: full-suite.

`diff_tests_executed`:

- `TestListTrackingIncludesClosedRuns` — PASS.
- `TestLatestOpenRunIgnoresClosedRuns` — PASS.
- `TestListTrackingPushesLimitToBacking` — PASS.
- `TestListTrackingUnlimitedStaysUnlimited` — PASS.
- `TestHandleOrdersFeedIncludesRigStoreTrackingBeads` — PASS.
- `TestBeadListSurfacesStoreErrorsAsPartial` — PASS.
- `TestBeadListPreservesPartialResultRows` — PASS.
- `TestBeadListReturns503OnTotalOutage` — PASS.
- `TestBeadListReturns503OnEmptyPartialTotalOutage` — PASS.
- `TestBeadReadyPreservesPartialResultRows` — PASS.
- `TestBeadReadySurfacesStoreErrorsAsPartial` — PASS.
- `TestBeadReadyReturns503OnEmptyPartialTotalOutage` — PASS.

`policy_lane`: `make test-ci-policy` — PASS.

`waiver_ref`: none; all raw failures are attributed under criterion 3a, not waived.

`added_test_load`: no. Tests were added to already-covered package targets; the diff adds no suite target and changes no resource-census baseline.

## Full-union failure attribution

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The installed-`bd` manifest skew predates this run and is independent of the candidate's API/orders/storebinding paths. The candidate cannot change the installed CLI or `internal/bdflags` manifest.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr` / `ga-k3fxvj`. Both exact empty-default-keytable signatures predate the run and arise from host tmux 3.7b behavior. The candidate does not touch or reach `internal/runtime/tmux`.
- `TestGraphWorkflowSuccessPath` -> `ga-lpfjhc`. It failed during `gc init` with the exact `gastownhall/beads#4566` pending-dirty-schema signature, before any workflow or orders-feed behavior executed. The candidate cannot alter schema migration or fixture store bootstrap.
- `TestE2E_SuspendResume_City` -> `ga-yc0e3a`. The tracker contains exact-base reproduction of the same missing `citysus.report` timeout. This test exercises city suspend/resume, session kill, and report production; it never invokes the changed orders-feed/ListTracking path.

All five failing tests are outside diff-owned test files, every tracker predates
this run, and none of the failing execution paths reaches a changed function.
The candidate adds no test target or declared resource load. Criterion 3
therefore passes with the raw failures preserved and attributed.
