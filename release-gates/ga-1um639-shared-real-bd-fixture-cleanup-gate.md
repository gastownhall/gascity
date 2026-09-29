**Verdict:** **FAIL**

# Shared real-bd fixture cleanup release gate

Publication is held by mayor instruction **gm-wisp-z9i24b** while operator decision **ga-yv9wqa** resolves the fork-only Mac CI trigger gap. This is a release-evidence hold, not an implementation rejection. No branch push, PR, deploy clearance, or merge-request has occurred. No waiver applies to Gas City.

- Deploy bead: `ga-1um639`; reviewer PASS: `ga-8k07n9`; build lineage: `ga-igobx3`.
- Reviewed source: `aae852a9b6aa49f3f2a377e494e4588ca314c7b8`; isolated branch: `deploy/ga-1um639-gate`.
- Base snapshot: `573c85464b01ceaa59fa4829a744fd8d1fb83821` (`origin/main` when evaluation began).
- Disposable merged commit: `8a95cd0fc2a526512c35e7c0a4bd709dfe19943a`; expected and actual merged tree: `6d348a69561a41145c23ccd3918db1a0c622e29c`.
- Mode: `remote`; authorized push remote: `fork`. Builder branch names are provenance only.
- Fix tracker: `ga-aik16g`. It stays open until landing proof; then `ga-9s0ftp` can re-gate.
- Every source/base/merged commit was resolved through git. Already-merged preflight found no associated PR and the source was not reachable from the base snapshot.

## Numbered criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Exact-source review ga-8k07n9 records verdict: pass for the resolved reviewed commit. |
| 2 | PASS | One shared bounded cleanup/HOME helper replaces duplicated fixture logic. The four known fixture sites were exercised in the fresh full sweep; all 20 added/changed test bodies passed. No verified bd shutdown hook exists for the supported CLI, so bounded RemoveAll is the documented fallback; BEADS_TEST_MODE is forward compatible rather than claimed proof of deterministic shutdown. |
| 3 | FAIL | Local full-scope tests and policy passed, but criterion 3c is not decidable: changed CI jobs have no first real Actions run. Hold for ga-yv9wqa; no publication under the mayor STOP. |
| 4 | PASS | Exact-source review has no unresolved high-severity diff finding. |
| 5 | PASS | Reviewed branch was clean before this record; only this record is staged for the gate commit. Post-commit clean status is recorded on the bead before handoff. |
| 6 | PASS | git merge-tree returned 0. materialize_merge_tree with normal hooks matched its tree exactly; merged checkout was clean. Fresh go build ./... and go vet ./... exited 0. No self-rebase was needed. |
| 7 | PASS | Shared real-bd fixture cleanup and HOME isolation are one theme. Tests, resource census, TESTING.md, BUILD files and CI steps support it. Scope guard accepted confirmed lineage IDs ga-1um639, ga-igobx3, ga-zq8iwb, ga-8t1yid, ga-wapfnm and ga-nr9epw; no stack or forbidden .claude paths. |

## Test and CI evidence

- `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`
- `test_cmd_scope: full-suite`; exit 0; 40/40 jobs completed.
- `test_counts: 97116 PASS, 0 FAIL, 331 SKIP`. These count actual test/subtest result lines across shard logs; repeated independent executions count separately. Duplicated tails in the launcher log are excluded.
- `failure_attribution: none` for the executed full sweep; no source failure was excused.
- `waiver_ref: none`.
- `policy_lane: run-pinned-lint.sh -- make lint fmt-check test-ci-policy test-native-doltlite-beads check-gomod-replace check-core-boundary check-native-dependency-surface check-eventexport-isolation check-routed-test-rows check-split-topology-rows check-residency-boundary check-docs — PASS`, exit 0, zero lint issues.
- `GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}`.
- `make dashboard-ci spec-ci bazel-sync` passed on the same merged tree; no tracked generated drift. Dashboard preview index and two referenced assets returned HTTP 200 on 127.0.0.1:42795; the owned preview process was stopped and its absence verified. `make check-hooks` confirmed .githooks ownership.
- Additional full acceptance bundle: `make test-acceptance test-acceptance-b test-bd-cli-contract test-bd-cli-contract-home-isolation ACCEPTANCE_GO_TEST_FLAGS="-count=1 -v"` exited 0. Tier A: 438 PASS / 0 FAIL / 12 SKIP; Tier B: 12 / 0 / 0; current bd contract: 37 / 0 / 0; HOME contract: 1 / 0 / 0.
- Required M1/M5 topology CI selector: `topology_ci_rc=0`; counts `{'PASS': 11, 'SKIP': 1}`. This additive focused lane reproduces CI coverage; it does not replace the full-scope sweep.
- Minimum-supported bd v1.0.4 contract and HOME targets: `minimum_contract_rc=0`; counts `{'PASS': 38}`. The repository archive installer verified its checksum; installation was in the evidence-only cache, leaving global bd unchanged.
- Linux tests used the pinned current bd binary whose Go metadata resolves BD_CURRENT_REF=f45b249ce6b40ba62aecc03949e6371e8f7c79d8 with vcs.modified=false; schema v66 matches the linked library.
- Rootless Podman socket, disabled Ryuk paired with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1, and cached Dolt 2.1.7 were verified before execution. TMPDIR=/var/tmp; no build cache was cleared or overridden.

### Required lane coverage and first-run gap

| CI coverage | Evidence in this session |
|---|---|
| Preflight static/policy, unit and process | Full sweep plus pinned policy. All six process shards passed, including TestTutorial01/01-hello-gas-city and /08-agent-pools with GC_FAST_UNIT=0. |
| Integration packages, bdstore, REST and review formulas | Full sweep: all core/cmd/tmux, bdstore, smoke, REST and formula partitions completed. |
| Worker core and phase 2 summaries | Full sweep executed phase-1/phase-2 profile conformance and TestPhase2WorkerCoreRealTransportProof, including claude and codex. |
| Acceptance and proxied native | Full Tier A and Tier B passed; TestBeadsProxiedNativeLifecycle/Safety and TestBeadsProxiedDefault passed. |
| Topology acceptance | M1-proxied-local/M5-legacy-gc-managed CI selector; see recorded result above. |
| CLI compatibility | Current and minimum-supported contracts/HOME checks; see recorded results above. |
| Generated artifacts/dashboard | dashboard-ci and spec-ci passed; preview served index and assets. |
| Changed CI jobs (3c) | ci_lane_run: not-yet-run. contract-acceptance-previous, contract-acceptance-current, contract-radar-bd-head and mac-acceptance have no first real run of the added HOME step. |

Linux pull_request triggers can run on a draft. Mac regression rejects fork PRs/drafts, and the fork default main does not contain the workflow. Dispatching base main would run its old definition without the new step. Mayor expressly retained fork-only publishing and stopped this deploy pending an operator ruling. Local equivalents and YAML meta-tests do not satisfy 3c. Other remote-only platform/service lanes are not claimed as completed by Linux local evidence.

### Load gate measurements

Every test lane ran through load-gate-run.sh (threshold 15, max wait 1800) and isolated-test-run.sh. A timed-out bounded wait proceeds transparently; it is not itself a test failure.

| Lane | load_threshold | load_waited_seconds | load_wait_timed_out | load_start | load_max | load_mean |
|---|---:|---:|---:|---:|---:|---:|
| Full sweep | 15 | 120 | 0 | 15.83 | 39.96 | 22.89 |
| Acceptance | 15 | 661 | 0 | 32.87 | 39.41 | 21.53 |
| Generated/dashboard | 15 | 60 | 0 | 16.59 | 16.59 | 15.43 |
| Topology CI rows | 15 | 1801 | 1 | 15.89 | 40.07 | 27.40 |
| Minimum bd | 15 | 1801 | 1 | 35.19 | 40.06 | 27.64 |

### Diff-owned tests by name

`diff_tests_executed`: the following 20 added/modified test bodies all passed. All were resolved against fresh full-scope output, including the complete acceptance package lanes for acceptance-tagged tests. The audit additionally checked all 374 functions in changed test files: 363 passed and 11 unchanged tests skipped for pre-existing explicit gates listed below.

| Test | Result | Fresh output |
|---|---|---|
| `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession` | PASS | `cmd-gc-process-6-of-6.log` |
| `TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork` | PASS | `cmd-gc-process-2-of-6.log` |
| `TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork` | PASS | `cmd-gc-process-3-of-6.log` |
| `TestCmdGCRealBDTestsUseTestOwnedDoltContext` | PASS | `cmd-gc-process-4-of-6.log` |
| `TestNewConditionalIntegrationRunnerIsolatesHOMEFromSharedServerConfig` | PASS | `integration-packages-core-4-of-4.log` |
| `TestRetryRemoveAllRetriesUntilRemovalSucceeds` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestRetryRemoveAllStopsAtItsAttemptBudget` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestGuardedTempDirRegistersTheRetryingRemoval` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestGuardedTempDirRemovesItsDirWhenTheTestEnds` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestTestOwnedHomePinsHOMEToAGuardedTempDir` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestGuardedTempDirRemovalRunsBeforeTempDirsOwnCleanup` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestBdSubprocessEnvSetsBeadsTestMode` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestBdSubprocessEnvOverrideCanDisableTestMode` | PASS | `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestRunBDIsolatesHOMEFromSharedServerConfig` | PASS | `acceptance-fresh.log` |
| `TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig` | PASS | `acceptance-fresh.log`, `integration-packages-core-1-of-4.log`, `unit-core.log` |
| `TestBdRunWithEnvIsolatesHOMEFromSharedServerConfig` | PASS | `acceptance-fresh.log` |
| `TestBdStoreMailWispInsertIsolatesHOMEFromSharedServerConfig` | PASS | `integration-rest-full-2-of-8.log` |
| `TestDoltConfigWiringExternalHost` | PASS | `integration-rest-full-6-of-8.log` |
| `TestDoltConfigWiringIsolatesHOMEFromSharedServerConfig` | PASS | `integration-rest-full-7-of-8.log` |
| `TestRealBdRunnerIsolatesHOMEFromSharedServerConfig` | PASS | `integration-rest-full-1-of-8.log` |

The API mixed-case inbound route test passed in unit-core and core integration output. The conditional fixture scaffold_roundtrip_any_bd subtest passed and exercised teardown before the unchanged capability skip. The doctor cleanup assertions moved into the shared helper suite, whose eight tests passed; no assertion was retired without a new owner.

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

## Run corrections and handoff

The initial host-linter run used 2.13.2 and reported 10 diagnostics. Repository-pinned 2.12.0 completed the full policy lane with zero issues, following the existing version-correction ruling; those diagnostics are not attributed as source failures. Initial full-suite log-directory and topology selector quoting attempts failed before any test execution, then were corrected without narrowing the primary full-scope command. Two earlier load waits and a queued origin push were terminated through captured/verified owned process groups before command execution; no test or push result is carried from those cancelled waits. The failed log-directory attempt observed threshold=15, waited=780, timed_out=0, start=25.92, max=39.26, mean=28.55; the failed topology quoting attempt observed waited=0, start/max/mean=13.52.

Exit: mayor relays a verifiable operator ruling on ga-yv9wqa, records and closes that decision, clears ga-1um639 hold and returns it to deployer. Re-read live phase and re-gate a fresh current-base merge before publishing. This record is local while STOP applies. Do not close ga-aik16g before its fix lands.

Raw evidence: `/var/tmp/deploy-ga-1um639.Idw47k`; original shard logs, result files, diff-test-results.json, full-suite-skips.json, pinned-policy.log, quality.log, preview-result.txt and acceptance-fresh.log.
