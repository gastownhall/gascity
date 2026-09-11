# Release gate: eventfeed hang-detector budget

- Deploy bead: `ga-9c55c6`
- Reviewed source: `041e4406a88bd139f9a73fb95bfb88e95fbb428a`
- Base: `origin/main@fa5bdd0cc31bcf9df75790d849b737a414051c38`
- Deploy mode: `remote`; push remote: `origin`
- Overall verdict: **PASS**

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-3l4wmn` records `REVIEW VERDICT: PASS` on the exact reviewed source. The reviewer independently reconciled all converted sites, ran the package under the race detector, and found no style, security, or specification blocker. |
| 2 | Acceptance criteria met | **PASS** | `internal/eventfeed/muxsource_test.go` defines one package `hangBudget` as `6 * testutil.GoroutineRaceTimeout` and uses it at all five pure hang-detector constructs. The nine changed references are exactly the timer/message pairs in `requireFloorAt`, `requireWatchAfter`, and `requireTaggedEvent`, the rebuild/Next context, and the consumer-drain cleanup wait. No fed timeout, negative assertion window, production code, shared timeout floor, or dependency changed. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full CI-equivalent union completed all 40 jobs: **32 green / 8 red jobs**, with **45,549 top-level PASS / 8 FAIL / 187 SKIP**. Every red test is non-diff-owned, covered by a predating tracker, has no path overlap, and is structurally unreachable from this `_test.go`-only change; full attribution is below. Both diff-owned behavioral tests passed twice in the union. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `git diff --check origin/main...HEAD`, fresh-cache `make lint-affected`, and `make fmt-check-changed` all pass; golangci-lint reports 0 issues for `./cmd/gc ./internal/eventfeed`. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-3l4wmn` records PASS with no requested changes and no HIGH/security findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after the gate commands and before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` exits 0 with tree `9a67eaee3b9d95f9aab2ed265f78181a1b4ec536`. The current base is `origin/main@fa5bdd0cc31bcf9df75790d849b737a414051c38`; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The single reviewed commit changes only `internal/eventfeed/muxsource_test.go`: one test-reliability theme, deriving eventfeed's pure hang-detector waits from a shared package budget. |

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-9c55c6-full-gate make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 45,549 PASS / 8 FAIL / 187 SKIP across all 40 jobs; 32 jobs green and 8 jobs red with attributed failures below.
- `diff_tests_executed`: `TestMuxSource_PreservesInitializedZeroFloorAcrossRebuild` PASS twice; `TestMuxSource_YieldsAndPicksUpNewCity` PASS twice. No diff-owned test failed or skipped.
- `skip_justification`: the 187 skips are pre-existing platform, optional-tool, and feature-gated tests outside the modified eventfeed file; the two tests exercising every changed wait both executed and passed.
- `waiver_ref`: none. Criterion 3 relies on the standing non-diff-owned failure-attribution policy, not a self-granted waiver.
- Environment: rootless Podman 5.8.4 was live before the run at `unix:///run/user/1000/podman/podman.sock`; cached pinned images included `dolthub/dolt:2.1.7`, `dolthub/dolt-sql-server:1.32.4`, and `testcontainers/ryuk:0.14.0`.

Raw failures and attribution:

- `TestCatalogMatchesProductionWiringAndDocumentation` in `unit-core` and `integration-packages-core-4-of-4` -> `ga-1s16pf`. Clause 3(a), mechanism: the provider-ledger entries fail because a waiver owned by nonexistent `ga-80po0c.3` expired on 2026-08-26. An eventfeed `_test.go` timeout constant cannot reach or alter provider-ledger production wiring. Tracker created 2026-07-26, before this run.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and `TestHumaBinary_CityCreateAsync` -> `ga-lpfjhc`, with the existing Beads #4566 standing disposition recorded on `ga-6bnc42`. Clause 3(a), mechanism: both fail during fixture bootstrap with `pending ... schema migrations alter pre-existing dirty tables`, before candidate behavior can execute. Tracker created 2026-08-15, before this run.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. Clause 3(a), mechanism: host tmux 3.7b returns empty default bindings. The candidate imports no tmux path and changes no runtime code. Tracker created 2026-08-15 and explicitly names both tests.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. Clause 3(a), mechanism: the installed `bd --help` exposes flags absent from the repository manifest. The candidate changes neither the installed binary nor `internal/bdflags`. Tracker created 2026-08-15, before this run.
- `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` -> root tracker `ga-vbyn8v`, whose reviewed fix remains pending deployment via `ga-yxgivi`. Clause 3(a), mechanism: the sweep removed a directory still held by a live Dolt server under parallel load. The candidate changes only an eventfeed test and cannot reach `internal/doltorphan` or the Gas Town integration test. Tracker created 2026-08-19, before this run.

For every attributed failure, clause 1 passes because the failing test file is not diff-owned; clause 2 passes because the cited tracker predates this run and covers the exact test or condition; clause 3(a) lands through structural unreachability; and clause 4 passes because the only changed path is `internal/eventfeed/muxsource_test.go`. `inconclusive-guard`: `reachable_production_code=no`, `added_test_load=no` — the diff changes no production file, resource census, build target, workflow, or package inclusion.
