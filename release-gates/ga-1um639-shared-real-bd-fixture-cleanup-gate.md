**Verdict:** **PASS**

# Shared real-bd fixture cleanup release gate

Fresh evaluation completed on 2026-09-29. The changed Linux CI workflow completed successfully on draft PR [#6821](https://github.com/gastownhall/gascity/pull/6821), including all three added HOME-isolation steps. The changed Mac acceptance lane is **HANDED ON** to its first real post-merge run under operator decision **ga-yv9wqa**, relayed in mayor mail **gm-wisp-ttt7u1**. Follow-up **ga-cdr5n7** records that obligation and depends on landing-proof closure of **ga-aik16g**. This supersedes the earlier held evaluation; no earlier run's counts are carried forward.

- Deploy bead: `ga-1um639`; exact-source reviewer PASS: `ga-8k07n9`; build lineage: `ga-igobx3`.
- Reviewed source: `aae852a9b6aa49f3f2a377e494e4588ca314c7b8`; isolated branch: `deploy/ga-1um639-gate`.
- Frozen full-suite base: `fd65129e3921cdd22d89b999f1a3fc7c69af2097`; disposable merged commit: `bc1e8a3bad688b06e0133e4de8c0b347651aa668`; expected/actual merged tree: `219450175d94f1de3b4520b018455ce8bd25bc4b`.
- Latest conflict-check base: `c777d07de22fda0a4c0edb5e8063f5238b605196`. It merges cleanly with the draft head `790b8a5aa3363d92a63d1bcdf9bced7e6767c895`, producing tree `9d17e12b877399cc98b034e30d80572337274721`. Local full-suite evidence remains explicitly tied to the frozen snapshot; the real PR CI run tests its own merge candidate.
- Mode: `remote`; mandated push remote: `fork`. No shared builder branch is a push target; no self-rebase was needed.
- Fix tracker `ga-aik16g` remains open until landing proof; then `ga-9s0ftp` re-gates fresh.
- Source/base/merged SHAs were resolved through git. Already-merged preflight found no landed target. Normal repository hooks ran during materialization and branch publication.

## Numbered criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-8k07n9 explicitly records PASS for the resolved reviewed source. Single reviewer pass is the active policy; no second-pass verdict is invented. |
| 2 | PASS | Shared bounded cleanup and test-owned HOME setup cover the CLI/API/doctor/acceptance fixture sites; all 20 changed bodies passed by name. Cleanup ordering, retry exhaustion, subprocess overrides and HOME isolation were exercised. Supported bd has no verified shutdown hook: bounded RemoveAll remains the fallback, and BEADS_TEST_MODE is forward compatible. |
| 3 | PASS | Fresh complete 40-job sweep, both acceptance tiers, current/minimum contracts and complete pinned policy/generated checks passed. Linux first-run evidence is complete; Mac 3c is HANDED ON under the specific operator ruling, with a tracked post-merge obligation. See commands/counts below. |
| 4 | PASS | Exact-source review has no unresolved high-severity finding. |
| 5 | PASS | Branch was clean before the gate record; only this record is staged. The post-commit clean-tree and final head proof are recorded on the bead before clearance/handoff. |
| 6 | PASS | Both frozen and latest-base merge-tree checks returned 0; frozen materialization matched exactly. Fresh go build ./... and go vet ./... passed on the materialized tree. No conflict or rebase. |
| 7 | PASS | Shared real-bd fixture teardown and HOME isolation form one feature theme. Tests, TESTING.md, resource census, BUILD inputs and CI steps support it. Scope guards accepted confirmed lineage IDs ga-1um639, ga-igobx3, ga-zq8iwb, ga-8t1yid, ga-wapfnm, ga-nr9epw; no stack or forbidden .claude paths. |

## Fresh test evidence

- `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`
- `test_cmd_scope: full-suite`
- `test_counts: 97126 PASS, 0 FAIL, 331 SKIP` — actual test/subtest result lines across shard logs, excluding duplicated launcher tails. Repeated independent executions count separately.
- `test_result: 40/40 shard ok markers and All full jobs passed`, followed by final load summary and no isolation TRIPWIRE. The runner emits that success marker only for status 0 and returns the same status; the isolation wrapper preserves it unless its guard fires. The outer launcher disappeared across the role refresh, so its exit status was not captured; no observed outer exit code is invented.
- `diff_tests_executed: all 20 added/modified bodies PASS` in the named table below. Four redundant fast-partition SKIPs are retained; each has actual PASS in the mandatory real-process shard. No changed body lacks its required executed coverage.
- `failure_attribution: none` for these executed local lanes; no source failure was excused.
- `waiver_ref: none`. Gas City has no waiver path. The Mac ruling is a specific hand-on of evidence, not a fabricated Mac PASS.
- `policy_lane: run-pinned-lint.sh -- make lint fmt-check test-ci-policy test-native-doltlite-beads check-gomod-replace check-core-boundary check-native-dependency-surface check-eventexport-isolation check-routed-test-rows check-split-topology-rows check-residency-boundary check-docs dashboard-ci spec-ci bazel-sync — PASS`, exit 0, zero lint issues.
- `GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}`.
- `make check-hooks`, `go build ./...`, `go vet ./...`: exit 0. Generated artifacts stayed clean after dashboard-ci, spec-ci and bazel-sync. Bazel sync is verified; no local Bazel test execution is claimed.
- Dashboard preview after dashboard-ci: http://127.0.0.1:53025/ index, referenced JS and CSS each HTTP 200. Owned preview process was stopped and its absence verified.
- Full acceptance bundle `make test-acceptance test-acceptance-b test-bd-cli-contract test-bd-cli-contract-home-isolation ACCEPTANCE_GO_TEST_FLAGS="-count=1 -v"`: exit 0. Tier A 438 PASS / 0 FAIL / 12 SKIP; Tier B 12 / 0 / 0. These contract targets do not inherit ACCEPTANCE_GO_TEST_FLAGS, so named contract counts come from the supplementary unchanged pair below.
- Current pinned bd: `env GOFLAGS=-v make test-bd-cli-contract test-bd-cli-contract-home-isolation`: exit 0, 38 PASS / 0 FAIL / 0 SKIP.
- Minimum supported bd v1.0.4: identical complete target pair, exit 0, 38 / 0 / 0. Repository archive installer verified the checksum into the evidence-only cache; host bd was unchanged.
- Additive topology CI selector: `make test-acceptance ACCEPTANCE_TIMEOUT=30m ACCEPTANCE_TOPOLOGY_MATRIX=1 ACCEPTANCE_REQUIRE_TOOLING=1 "ACCEPTANCE_GO_TEST_FLAGS=-count=1 -v -run 'TestBeadsInitTopologyMatrix$$/^(M1-proxied-local|M5-legacy-gc-managed)$$'"`: exit 0, 11 PASS / 0 FAIL / 1 SKIP. The doubled dollars are the Make escaping for the documented CI selector. This focused additive lane does not replace the full-scope sweep.
- Current bd Go metadata proves BD_CURRENT_REF=f45b249ce6b40ba62aecc03949e6371e8f7c79d8, vcs.modified=false, matching linked schema v66. Rootless Podman socket, cached Dolt 2.1.7 and TESTCONTAINERS_RYUK_DISABLED=true paired with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 were verified before tests. TMPDIR=/var/tmp; no Go build cache was cleared or overridden.

### Criterion 3c: actual CI runs

`ci_lane_run: 36597909139 DRAFT-PR-triggered pre-PASS — PASS`: [complete CI run](https://github.com/gastownhall/gascity/actions/runs/36597909139), status completed, conclusion success, draft head `790b8a5aa3363d92a63d1bcdf9bced7e6767c895`. Retrieved through PR #6821's check links; verified workflow/branch/head. GitHub returned empty pull_requests arrays for the fork runs, so a source-SHA or array-only lookup is not evidence. Canonical lookup repair is tracked as ga-picodf.

| Changed Linux job | Job result | Added step result |
|---|---|---|
| [Contract / bd CLI (minimum supported)](https://github.com/gastownhall/gascity/actions/runs/36597909139/job/109507604289) | PASS | bd CLI contract HOME isolation (minimum supported) — PASS |
| [Contract / acceptance A (bd current)](https://github.com/gastownhall/gascity/actions/runs/36597909139/job/109507777380) | PASS | bd CLI contract HOME isolation (current) — PASS |
| [Contract radar (gc HEAD x bd main HEAD)](https://github.com/gastownhall/gascity/actions/runs/36597909139/job/109507777192) | PASS | bd CLI contract HOME isolation (main HEAD) — PASS |

`ci_lane_run: Mac HANDED ON` — .github/workflows/mac-regression.yml changes ONE acceptance job. Its fork/draft skip and green summary are not execution evidence. Operator chose option C on ga-yv9wqa and explicitly retained fork-only publishing; mail gm-wisp-ttt7u1 relays the ruling. Follow-up ga-cdr5n7 must record the first actual post-merge Mac acceptance job and HOME step's completion/result. Its blocks dependency on tracker ga-aik16g waits for landing proof. This gate does not close that tracker.

At this gate's first-run read, all PR checks were green/skipped except the separate side-by-side Bazel test still running. No Bazel result is invented. The operator also stated on ga-yv9wqa: “merge; unless there is something that has fixed the bazel problem, just merge with that red.” That specific disposition is relayed to MPR if the known Bazel lane is red; it is not a blanket excuse for a changed Linux lane or local test failure. The final current-head CI state is recorded in the merge-request after the gate-record-only push settles.

### Load measurements

All criterion-3 test/policy lanes ran through load-gate-run.sh (threshold 15, bounded max wait 1800) and isolated-test-run.sh. Recovered empty waits preserved their original remaining bounds. No cancelled wait contributes test results.

| Lane | load_threshold | load_waited_seconds | load_wait_timed_out | load_start | load_max | load_mean |
|---|---:|---:|---:|---:|---:|---:|
| Full sweep | 15 | 810 | 0 | 18.99 | 51.44 | 25.42 |
| Acceptance | 15 | 796 | 0 | 18.48 | 51.44 | 27.09 |
| Topology CI rows | 15 | 240 | 0 | 18.99 | 41.82 | 26.76 |
| Current bd | 15 | 240 | 0 | 18.99 | 25.67 | 18.58 |
| Minimum bd | 15 | 240 | 0 | 18.99 | 41.82 | 28.41 |
| Policy/generated/dashboard | 15 | 240 | 0 | 18.99 | 41.82 | 26.77 |

### Diff-owned bodies by name

| Added or modified body | Result | Fresh log |
|---|---|---|
| `TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig` | PASS | `acceptance.log`, `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestBdRunWithEnvIsolatesHOMEFromSharedServerConfig` | PASS | `acceptance.log` |
| `TestBdStoreMailWispInsertIsolatesHOMEFromSharedServerConfig` | PASS | `shards/integration-rest-full-2-of-8.log` |
| `TestBdSubprocessEnvOverrideCanDisableTestMode` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestBdSubprocessEnvSetsBeadsTestMode` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession` | PASS | `shards/cmd-gc-process-6-of-6.log` |
| `TestCmdGCRealBDTestsUseTestOwnedDoltContext` | PASS | `shards/cmd-gc-process-3-of-6.log` |
| `TestDoltConfigWiringExternalHost` | PASS | `shards/integration-rest-full-6-of-8.log` |
| `TestDoltConfigWiringIsolatesHOMEFromSharedServerConfig` | PASS | `shards/integration-rest-full-7-of-8.log` |
| `TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork` | PASS | `shards/cmd-gc-process-1-of-6.log` |
| `TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork` | PASS | `shards/cmd-gc-process-2-of-6.log` |
| `TestGuardedTempDirRegistersTheRetryingRemoval` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestGuardedTempDirRemovesItsDirWhenTheTestEnds` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestNewConditionalIntegrationRunnerIsolatesHOMEFromSharedServerConfig` | PASS | `shards/integration-packages-core-3-of-4.log` |
| `TestRealBdRunnerIsolatesHOMEFromSharedServerConfig` | PASS | `shards/integration-rest-full-1-of-8.log` |
| `TestRetryRemoveAllRetriesUntilRemovalSucceeds` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestRetryRemoveAllStopsAtItsAttemptBudget` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |
| `TestRunBDIsolatesHOMEFromSharedServerConfig` | PASS | `current-cli-verbose.log` |
| `TestTestOwnedHomePinsHOMEToAGuardedTempDir` | PASS | `shards/integration-packages-core-4-of-4.log`, `shards/unit-core.log` |


### Skip justification


The 331 full-sweep skips include partition-only skips that ran and passed in the dedicated process lane, deliberately classified-provider test harness skips, unchanged conditional capability checks, child-only helpers, non-Linux paths, host-sub-reaper checks, live SSH/registry/service requirements, optional tmux dogfood/binding checks and historical explicit quarantine/opt-in gates. These unchanged checks are not evidence of newly skipped test bodies. Exact names/reasons are retained in full-suite-skips.json and original shard logs.

The 11 unchanged skipped functions in modified files are:
- `TestBdStoreConditionalWriterConformance`: installed bd lacks --if-revision; scaffold passed.
- `TestBdStoreConformance`: unchanged explicit ga-e7z613 fallback skip.
- `TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_MetadataSurvivesSubsequentBdWrites`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.
- `TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility`: unchanged GC_INTEGRATION_BD_PERSISTENCE opt-in gate.

Tier A’s 12 skips comprise two legacy migration tests without a pre-journal gc binary, the opt-in topology matrix (run separately), proxied backup support unavailable in pinned bd, seven existing SDK UX placeholders and an unconfigured external live pack registry. Topology M5 may skip when the legacy gc binary is absent, matching that CI job’s documented behavior. No added/modified test body skipped or failed.


## Corrections and handoff

The initial materialization using shared host lint 2.13.2 was cancelled before a certified result; repository-pinned 2.12.0 completed the fresh lane with zero issues. Empty load waits lost their launcher across the role refresh and were recovered without re-running executed tests, preserving remaining bounds. Details are retained in run-corrections.txt. No historical held-gate count is reused.

Only the gate record changes after the successful first CI run. Exact final head, clean branch, PR verification, deploy clearance and peek-verified MPR-request routing are recorded on ga-1um639. Merge authority remains MPR's. The deploy bead closes blocked pending that merge; ga-aik16g closes only with landing proof and ga-cdr5n7 verifies the handed-on Mac run.

Raw evidence: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-1um639/20260929T1425Z` — original shard logs, local-evidence.md, owned-test-table.md, diff-test-results.json, full-suite-skips.json, quality.log, preview-result.txt, lane-results.txt and ci-lane-evidence.json.
