# Owned beads lifecycle HOME isolation — ga-8ilufx

**Verdict:** **PASS**

Criterion 3 passes with explicitly attributed failures under the non-diff-owned gate-failure protocol. The raw full-suite command returned nonzero; this record does not claim zero failing tests. No waiver is used.

- Deploy bead: ga-8ilufx; build: ga-kobz2r; review: ga-rnw1aq.
- Reviewed source: `a304b5a3599dcd65e3e9669ea8b38fa6ca31681a` (resolved against the object store).
- Captured base: `0771f14410f4130922c457bf6dd03fc5ea5b6754` (`origin/main`, re-fetched unchanged before branch preparation).
- Tested materialized merge: `6724764cebfc2cf3522bb4e72c0fcb0aa405db08`.
- Computed and actual merge tree: `53a4b27d7b675565ac210e72c0cd8d229d021e56` (exact match).
- Deploy mode: remote; push remote: fork; branch: `deploy/ga-8ilufx-gate`.
- Evidence directory: `/var/tmp/deploy-ga-8ilufx.Fitehu` (raw logs, scripts, exit codes, `results.json`).
- Historical interrupted HOLD run: `/var/tmp/ga-8ilufx-gate._uj0pqeq`, local gate commit `2de40e9ca04e0c6ccbab098e49406d11b6fdf968`. Its partial counts are excluded from this run.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-rnw1aq records PASS on the resolved source, with style/security findings empty. Single reviewer policy applies while second pass is disabled. |
| 2 | PASS | Fixture HOME is created and passed to direct status inspection, matching lifecycle-command isolation. Full owning process lane executes parent/direct/proxied successfully. Shared host Dolt PID 3709422 on 127.0.0.1:3308 remains running and untouched. |
| 3 | PASS | Full 40-job command completed; all changed tests passed. Unrelated failures are attributed individually below, with four clauses and first-discovery/repeat eligibility recorded. Required supplemental attempts, counts, policy and generated checks are listed honestly. |
| 4 | PASS | Unresolved HIGH review findings: 0. |
| 5 | PASS | Reviewed source and tested merge have clean tracked trees; generated scratch has no drift. The only deploy addition is this committed gate record. Active repository hooks are verified. |
| 6 | PASS | Textual merge is conflict-free; exact merge-tree matches the materialized checkout. Full go build ./... and go vet ./... both exit 0. No self-rebase. |
| 7 | PASS | One feature: isolate HOME during inspection of provider-owned processes in one existing lifecycle test. Diff is one file, +7/-4; no production/import/new-target/census changes. Ancestry scope guard accepts only ga-8ilufx/ga-kobz2r/ga-rnw1aq; no live stack. |

## Full-scope test evidence

`test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`

`test_cmd_scope: full-suite`

`waiver_ref: none`

The documented complete local union ran from the materialized merge through `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh`. All 40 jobs finished: 34 passed, 6 failed, none incomplete. Outer exit 2 (Make inner job failure 123). Completion rows: **96,737 PASS / 12 FAIL / 330 SKIP**. Counts include parent/subtest rows and repeated suite profiles; they are not unique test counts. The fast unit baseline and every command-process shard passed.

Pinned environment: deps.env bd 1.3.0 at f45b249ce6b40ba62aecc03949e6371e8f7c79d8, Dolt 2.1.7; checksum-verified minimum bd 1.0.4 for its dedicated contract; rootless Podman socket verified before tests; Ryuk disabled with host sweep policy. Real host HOME preserved for this regression; test harness isolates its own runtime state. GOCACHE unchanged; scratch uses /var/tmp. No isolation TRIPWIRE.

`diff_tests_executed` from `shards/cmd-gc-process-5-of-6.log`:

| Test | Result | Elapsed |
|---|---|---|
| TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses | PASS | 10.28s |
| TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses/direct | PASS | 4.39s |
| TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses/proxied | PASS | 3.55s |

Every one of 234 parent tests defined in the changed lifecycle test file has a real PASS in the full output; no diff-owned failure or unexercised parent remains. The additional fast integration profile routes this process-heavy parent to `make test-cmd-gc-process` under GC_FAST_UNIT=1, as TESTING.md documents. Its duplicate SKIP is paired with the real owning-lane PASS above, not counted as execution.

`skip_justification`: the remaining completion rows cover platform-specific Darwin/unsupported-OS cases on Linux, subprocess helper entry points, explicitly opt-in live registry/runtime/provider canaries, capability-specific tests, host subreaper limitations, disabled legacy conformance profiles, and the documented fast/process profile partition. Raw named reasons are preserved in `results.json:skip_context`; none removes execution of the changed test. Test counts and skips are retained rather than silently filtered.

## Failure attribution

For every failure below, clause 1 is clear: no failing test file is changed. Clause 4 is clear: internal/beads, test/integration, test/acceptance, and frontend browser packages do not overlap the changed cmd/gc test file. Clause 3 proof **(a), MECHANISM**, lands: the diff changes only a function-local closure and fixture directory in an existing `_test.go`; other test binaries and production CLI/fakesupervisor builds cannot import or execute that closure. It changes no production code, imports, test target, new test file, resource census or suite parallelism. This proves the candidate cannot cause these failures; it does not invent their root cause.

Existing trackers ga-1037rg (created 2026-08-07) and ga-4w6d2r (2026-09-26) were opened in full and predate this run. Exact named sightings were appended and read back. This deploy's build_bead ga-kobz2r is gc.fixes_tracker=ga-j16tpf stamped. For repeat attribution, ga-1037rg has designated fix ga-wapfnm stamped gc.fixes_tracker=ga-1037rg, and ga-4w6d2r has ga-c2atlu stamped gc.fixes_tracker=ga-4w6d2r. Both fixes are closed with blocked work records; live work-record-check returns NOT-LANDED for their commits 93496f512368bac2077119ecddd3b0bbcf56420b and 66b9671a38491c723479ee56d13fc3cb3bfb8732. Thus the documented fix-carrying repeat exception applies separately to each condition.

| Failing completion row | Tracker | Raw shard log |
|---|---|---|
| TestBdStoreMailWispInsert (12.65s) | ga-1037rg; proof a | shards/integration-bdstore.log |
| TestBdStoreConditionalWriterConformance (6.99s) | ga-1037rg; proof a | shards/integration-packages-core-3-of-4.log |
| TestBdStoreConditionalWriterConformance/scaffold_roundtrip_any_bd (2.93s) | ga-1037rg; proof a | shards/integration-packages-core-3-of-4.log |
| TestBdStoreReleaseIfCurrentAgainstRealBd (2.72s) | ga-1037rg; proof a | shards/integration-packages-core-3-of-4.log |
| TestBdStoreDeleteBatchOrphansExternalDependents (7.94s) | ga-1037rg; proof a | shards/integration-rest-full-1-of-8.log |
| TestGastown_MailArchive (2.52s) | ga-4w6d2r; proof a | shards/integration-rest-full-1-of-8.log |
| TestGastown_PipelineMailChain (17.03s) | ga-4w6d2r; proof a | shards/integration-rest-full-1-of-8.log |
| TestDoltConfigWiringExternalHost (9.27s) | ga-1037rg; proof a | shards/integration-rest-full-2-of-8.log |
| TestMail_BashAgent (17.25s) | ga-4w6d2r; proof a | shards/integration-rest-full-2-of-8.log |
| TestGastown_MailRoundTrip (17.78s) | ga-4w6d2r; proof a | shards/integration-rest-full-6-of-8.log |
| TestGastown_PipelineMailAndWork (19.70s) | ga-4w6d2r; proof a | shards/integration-rest-full-6-of-8.log |
| TestGastown_PipelineConvoyTracking (2.22s) | ga-4w6d2r; proof a | shards/integration-rest-full-8-of-8.log |

The ga-1037rg rows report inherited host shared-server routing and database-not-found on 127.0.0.1:3308. Database names are beads (conditional/release), mc (MailWispInsert), **bd** (DeleteBatchOrphansExternalDependents), dc (ExternalHost). The prefix tracker covers the named valid-ID rejections (sf-16/gz-19) and mail/work timeouts (including valid zk-5), as its existing record proves. No different signature is folded into either family.

Two supplemental conditions were first discovered and tracked **during this run**: ga-yoxtux (minimum contract scope initialization) and ga-rteiln (local parallel browser health sample warming). Both records were opened/read back. They use the documented tracked-during-discovering-run escape with landed mechanism proof (a) and clear clauses 1/4, as explicitly permitted by the full non-diff-owned failure protocol. They are not claimed to predate the run and are not waivers. Each tracker remains open without intake label, assignee or route.

## Required supplemental lanes

| Command / lane | Raw result | Evidence / attribution |
|---|---|---|
| make test-acceptance ACCEPTANCE_GO_TEST_FLAGS=-v | exit 2; helpers 61 PASS / 0 FAIL / 0 SKIP; root tests do not reach m.Run | acceptance.log; RequireBdSchemaParity panics on bd migrate schema, database beads missing on 3308 -> ga-1037rg, proof a and stamped unlanded ga-wapfnm repeat exception |
| make test-bd-cli-contract, minimum bd v1.0.4 first on PATH | exit 2; 1 PASS / 36 FAIL / 0 SKIP | complement-bd-minimum.log -> ga-yoxtux, proof a, first discovering run |
| make test-worker-core-phase2-all PROFILE=claude/tmux-cli | exit 0; 51 PASS / 0 FAIL / 0 SKIP | complement-worker-phase2-claude.log |
| make test-worker-core-phase2-all PROFILE=codex/tmux-cli | exit 0; 51 PASS / 0 FAIL / 0 SKIP | complement-worker-phase2-codex.log |
| make test-worker-core-phase2-all PROFILE=cursor/tmux-cli | exit 0; 51 PASS / 0 FAIL / 0 SKIP | complement-worker-phase2-cursor.log |
| make test-worker-core-phase2-all PROFILE=gemini/tmux-cli | exit 0; 51 PASS / 0 FAIL / 0 SKIP | complement-worker-phase2-gemini.log |
| CI proxied-default, user-shared-server, migrations, topology M1/M5, proxied-native lifecycle/safety invocations | each exit 1 before m.Run; zero root test completion rows | complement-{proxied-default,user-shared-server,migrations,topology,proxied-native}.log; identical RequireBdSchemaParity 3308 panic -> ga-1037rg, proof a and unlanded designated fix repeat exception |
| goreleaser check | exit 0 | complement-release-config.log |
| CI=1 make dashboard-e2e-play | exit 0; 19 PASS / 0 FAIL / 0 SKIP; no flaky retry shown | browser-ci.log; full CI single-worker/fresh-server target; dashboard actually served and exercised |
| initial make dashboard-e2e-play, CI unset | exit 2; 18 PASS / 1 FAIL / 0 SKIP | generated-dashboard-e2e.log -> ga-rteiln, proof a, first discovering run. Health assertion line 195 sees samplers warming. Later CI-profile PASS establishes that run's outcome, not a cause or tracker closure. |

The minimum contract fails TestBdBasicCRUD, TestBdDependencies, TestBdDestructive, TestBdWorkflow and cascading children after bd init returns successfully but config.yaml/issue_prefix are missing. Its exact cause remains unproven. Separate private-HOME init/config probes pass; a HOME containing only no-db:true also passes. Those diagnostics are **not** a passing contract run and do not establish the shared-server field as the cause. Full signatures and all 36 rows remain in the tracker/log.

The five supplemental go invocations use the exact filters, tags and timeouts of the CI lanes, recorded in run-complements.sh. They supplement the full-scope primary command; no filtered run substitutes for it. The pre-main failures are attributed failures, not exercised/passing root coverage.

## Policy, generated artifacts, and load

`policy_lane`: pinned run-pinned-lint.sh -- make lint-affected (pin/resolved golangci-lint 2.12.0, 0 issues); make fmt-check-changed; make test-ci-policy; make check-gomod-replace; make check-native-dependency-surface; make check-eventexport-isolation; make check-core-boundary; make test-native-doltlite-beads; make check-docs — **all exit 0**. Corresponding policy-*.log and .rc files retained. Native binary 178,082,565 bytes, modules 737, within budgets.

Generated checks: make dashboard-ci; GC_REQUIRE_OAPI_CODEGEN=1 make spec-ci; bash scripts/check-generated-docs-drift.sh; frontend typecheck:test; typecheck:e2e; Vitest — **all exit 0**. Final git diff --exit-code and git status in generated scratch are clean. No imports/packages changed, so Bazel sync is not required. make check-hooks verifies .githooks ownership; the materialized merge's active pre-commit hook ran without changing its exact computed tree.

`ci_lane_run: n/a — candidate changes no CI configuration`

| Run | load_threshold | load_waited_seconds | load_wait_timed_out | load_start | load_max | load_mean | Samples / read errors |
|---|---:|---:|---:|---:|---:|---:|---|
| Full 40-job union | 15 | 0 | 0 | 13.48 | 39.84 | 24.66 | 67 / 0 |
| Tier A acceptance | 15 | 1710 | 0 | 24.59 | 40.14 | 25.46 | 58 / 0 |
| CI complements | 15 | 1050 | 0 | 35.70 | 40.10 | 25.50 | 40 / 0 |
| Correct CI browser profile | 15 | 300 | 0 | 22.84 | 22.84 | 18.68 | 12 / 0 |

All runs waited before testing through the load/isolation wrappers; no bounded wait timed out. Initial local browser run has no separate load telemetry; no values are invented. The captured source and base were unchanged throughout the fresh gate. Merge remains the merge authority's responsibility; gate PASS does not mean shipped.
