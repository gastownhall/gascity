**Verdict:** **PASS**

# Release gate: surface unhealthy order signals in `gc status`

- Deploy bead: `ga-uhbj9p`
- Review bead: `ga-eua7hl`
- Build bead: `ga-k6ofaz.1`
- Reviewed commit: `ae230ccf052808bb0e9f1bd2145cf92e9fbf5847`
- Base checked: `origin/main@9cfb391f6f379a626a34748aa7dbcaa2f67f5cf2`
- Deploy mode: `remote` (push remote: `fork`)
- Gate date: 2026-09-20

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-eua7hl` records an unambiguous round-2 PASS for the exact reviewed commit. The reviewer independently reproduced the round-2 red/green transition, ran the full `cmd/gc` package, and found no security or style blocker. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `gc status` now reuses the existing doctor firing-current and outcome-health checks, shows an `Orders:` block only when a signal is unhealthy, deduplicates the bounded tail of `OrderSuppressed` events by order name, and uses the same order-run-history fallback as `gc doctor` when a firing lies outside the event tail. The five acceptance tests cover a healthy city, stale firing, repeated failures, gate suppression, and the authoritative last-run fallback; all passed by name. The deploy-point safeguard was also live: `gc config show` reported `GC_REAPER_SESSION_PURGE_AGE = "876000h"`, and safety commit `56d8e6aa47` is an ancestor of the reviewed commit. |
| 3 | Tests pass | PASS | The documented full local runner, `make test-local-full-parallel`, ran all 40 jobs through `isolated-test-run.sh`: 37 jobs PASS / 3 jobs raw FAIL / 0 jobs skipped. The four raw failing test occurrences are attributed under 3a and are not diff-owned. Every `cmd/gc` shard completed `ok`, including both slices that selected each changed test. A supplemental verbose whole-package run produced 10,309 PASS / 0 FAIL / 108 SKIP and resolved all five diff-owned tests as PASS by name. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | Two shared-Dolt schema-refusal occurrences map to pre-existing tracker `ga-lejnse`; the stale integration Beads-version assertion maps to `ga-rnwg5u`; and the lifecycle startup timeout under the 40-job host load maps to `ga-vkhfnj`. Every tracker predates this run and now contains this run's sighting. The mechanism and path proofs are recorded below. |
| 3b | Policy/lint lane | PASS | `make fmt-check-changed`, `make test-ci-policy`, `go vet ./...`, and `go build ./...` passed at the exact reviewed commit; `make check-hooks` confirmed `.githooks` owns `core.hooksPath`. The standard `make lint-affected` widened to a full-repository scan and replayed 19 `errcheck` diagnostics from a different live worktree path. The same exact candidate with a brand-new `GOLANGCI_LINT_CACHE` passed with `0 issues`; pre-existing tracker `ga-u8z8j6` covers the full-fallback/cache-path leak and records this occurrence. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a` — the candidate changes no workflow, matrix, timeout, required-check, Makefile, or test-runner configuration. |
| 4 | No high-severity review findings open | PASS | The exact-head round-2 review records no unresolved HIGH finding, no security finding, and no style blocker. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed commit before this gate record was created. `make check-hooks` passed. The gate record is the only deploy-branch addition. |
| 6 | Branch diverges cleanly from main | PASS | Remote preflight found no PR carrying the reviewed commit. After the test and policy runs, `origin/main` remained `9cfb391f6f379a626a34748aa7dbcaa2f67f5cf2`; `git merge-tree --write-tree origin/main ae230ccf...` exited 0 with tree `098077fb9c567a7843532df0513582c6512fc8de`, and `git diff --check origin/main...ae230ccf...` exited 0. No self-rebase was needed. |
| 7 | Single feature theme | PASS | All four commits and both changed files serve one operator-facing feature: expose the order-liveness signals already available through doctor checks and suppression events in `gc status`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs; the range introduces no `.claude/**` path or unrelated theme. |

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'
test_cmd_scope: full-suite
runner_jobs: 37 PASS / 3 raw FAIL / 0 SKIP
raw_test_results: 4 FAIL / 0 explicit SKIP (the runner is non-verbose for passing tests)
diff_package_in_full_suite:
  github.com/gastownhall/gascity/cmd/gc ok in every package and process shard
full_suite_log: /var/tmp/ga-uhbj9p-test-local-full-parallel.log
full_suite_job_logs: /var/tmp/gc-local-tests.D49TAR
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The rootless Podman socket was healthy before the run, Ryuk was disabled for
this host's external reaper, and the required cached Dolt SQL Server image was
present before the suite started. All 40 runner jobs executed.

The mandatory full suite is non-verbose for ordinary passing tests, but each
shard prints its selected test names. Every diff-owned test appeared in both a
green `cmd-gc-process` shard and a green `integration-packages-cmd-gc` shard.
The follow-up whole-package verbose run resolves exact counts and names without
narrowing the criterion-3 command:

```text
supplemental_cmd: GC_FAST_UNIT=1 isolated-test-run.sh -- bash -lc 'go test -count=1 -v ./cmd/gc/...'
supplemental_counts: 10,309 PASS / 0 FAIL / 108 SKIP
supplemental_log: /var/tmp/ga-uhbj9p-cmd-gc-verbose.log
diff_tests_executed:
  TestCityStatusOrdersEmptyOnHealthyCity PASS
  TestCityStatusOrdersSurfacesStaleFiring PASS
  TestCityStatusOrdersSurfacesRepeatedFailures PASS
  TestCityStatusOrdersSurfacesGateSuppression PASS
  TestCityStatusOrdersFallsBackToOrderRunHistoryOutsideEventTail PASS
```

The 108 skips are the package's declared fast-unit/environment guards. None is
diff-owned, and every acceptance test executed and passed.

### Failure attribution

- `TestGraphWorkflowSuccessPath` and `TestCleanInstallTutorialPath` -> `ga-lejnse`.
  - Clause 1: both failing integration tests are outside the two-file candidate diff.
  - Clause 2: `ga-lejnse` predates this run, covers the exact shared-server pending-migration refusal, and now records both sightings.
  - Clause 3(a), mechanism: each test failed inside fixture `gc init`, before its scenario ran, because the inherited shared Dolt database refused automatic schema promotion (`v39 -> v66` and `v50 -> v66`). Neither failure invoked `gc status` or reached the changed order collection/rendering path.
  - Clause 4: `test/integration/**` and the shared-Dolt setup have no path overlap with `cmd/gc/city_status_snapshot.go` or `cmd/gc/city_status_snapshot_orders_test.go`.
- `TestPinnedIntegrationBeadsModuleVersion` -> `ga-rnwg5u`.
  - Clause 1: neither the integration assertion nor the dependency version pin is diff-owned.
  - Clause 2: `ga-rnwg5u` predates this run, covers the exact `v1.3.0` versus `v1.3.0-rc.2` mismatch, and now records this sighting.
  - Clause 3(a), mechanism: the test reads the module version and compares it with an untouched hard-coded expectation; order-health collection and rendering cannot affect either input.
  - Clause 4: the assertion and module-version paths do not overlap the candidate diff.
- `TestGastown_ControllerStartStop` -> `ga-vkhfnj`.
  - Clause 1: the lifecycle integration test is not diff-owned.
  - Clause 2: `ga-vkhfnj` predates this run and tracks whole-suite/host-load contention failures; this run's deacon-startup timeout is recorded there.
  - Clause 3(a), mechanism: under the 40-job runner load, the test timed out after 20 seconds with mayor active, deacon still `creating`, and only the mayor tmux session present. The test exercises controller/session lifecycle commands and never invokes `gc status`; the candidate adds no session creation, tmux, provider, or controller path.
  - Clause 4: `test/integration/gastown_controller_test.go` and the session/runtime paths have no overlap with the candidate diff.

No inconclusive attribution path and no waiver were used.

### Policy attribution

The standard changed-scope command widened to a full-repository scan because
the reviewed head predates the current-main dashboard asset
`internal/api/dashboardspa/dist/assets/Activity-D6jRsYAd.js`. Its 19
`errcheck` diagnostics were reported against the separate live role worktree
`/home/jaword/projects/gc-management/.gc/worktrees/gascity/deployer`, not the
disposable exact-SHA checkout. An identical exact-candidate run with a fresh
lint cache returned `0 issues` and exit 0. This establishes cache/worktree-path
contamination, not candidate content. Tracker `ga-u8z8j6`, created before this
run, covers the full-fallback scope/cache leak and now records this sighting.
The candidate changes no lint configuration, dashboard asset, test target, or
build runner.

```text
standard_policy_log: /var/tmp/ga-uhbj9p-policy-lanes.log
fresh_cache_lint_log: /var/tmp/ga-uhbj9p-lint-affected-fresh-cache.log
```

## Acceptance and scope evidence

- `cityStatusSnapshot` gains an `Orders` collection populated during normal `gc status` snapshot assembly.
- Firing freshness and outcome health reuse the existing doctor checks and omit healthy results.
- The firing-current check receives the same authoritative last-run lookup used by `gc doctor`, preventing command disagreement when an event has scrolled outside the bounded tail.
- A bounded 2,000-event tail collects `OrderSuppressed` signals, ignores malformed payloads, and keeps only the newest signal for each order.
- Text output prints `Orders:` only when at least one unhealthy signal exists.
- The candidate is confined to the snapshot implementation and its focused tests: 481 inserted lines across two `cmd/gc` files.
