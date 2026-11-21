**Verdict:** **PASS**

# Schema migration readiness — release gate

Bead: `ga-6kommgh7`; build: `ga-zlmadr`; rework: `ga-7aej2dnn`; review: `ga-4rvm85nm`.
Reviewed source: `5a723e788d2874cbd931f7434efba8c86edcb0a6`. Tested merge: `8a9bb845cfba51e8db841c5aabf04993292e9823`; tree: `d5d4b8e754326c80da18282802b3da842e19ba0a`.
gate_base: `f932d43b1c70b14103687b1f3e0dd457b23babd0`
merge_state: clean (`origin/main@f932d43b1c70b14103687b1f3e0dd457b23babd0`)
gate_base_kind: current-tip
merge_validation: exact clean merge built and vetted; the merge authority validates the eventual merge.
deploy_mode: remote; push_remote: fork; isolated branch: `deploy/ga-6kommgh7-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Closed review PASS at the resolved source; reviewer-added test boundary independently verified below. |
| 2 | PASS | Shell target/cursor checks, the Go readiness wait, wrapper forwarding, and live-handle reads verified against code and the named full-suite results. |
| 3 | PASS | Full-suite 156932 PASS / 0 FAIL / 613 SKIP; all 29 diff-owned tests PASS, required-job accounting and skip measurements complete. |
| 4 | PASS | Zero unresolved HIGH findings. Documented non-blocking limits remain below; no waiver. |
| 5 | PASS | Tested merge and source checkout clean; publication adds only this record. Post-commit cleanliness checked before push. |
| 6 | PASS | Whole-tree Bazel build and nogo output-group build both exit 0; both BEPs report overallSuccess=true. |
| 7 | PASS | One feature: reject readiness while a schema migration is incomplete. Shell adapter, store capability, forwarding wrappers and tests implement the same contract. |

## Acceptance evidence

reviewer_tests_verified: 6b32715b7019ee11ae2591fff8d690f62c56bec3 -> 5a723e788d2874cbd931f7434efba8c86edcb0a6 files=internal/beads/schema_cursor_ready_test.go. The helper proved a single directly descending, labelled, additive test-only commit. The review metadata names the build input; its PASS notes explicitly bind to the reviewed source including those added tests.

- The shell learns the target once per wait invocation by migrating a fresh throwaway database. It never migrates the live store. `TestWaitForBdRuntimeSchemaIsNotReadyWhileCursorBehindTarget`, `TestWaitForBdRuntimeSchemaNeverCompletesTheMigrationItself`, `TestWaitForBdRuntimeSchemaSucceedsWhenCursorReachesTarget`, `TestWaitForBdRuntimeSchemaPassesAheadOfTargetCursorThrough`, and `TestWaitForBdRuntimeSchemaFailsLoudlyWhenTargetUnknown` all PASS.
- `waitForBeadsScopeReadyAfterRecovery` checks the schema after Ping, for fresh init and post-recovery. `TestWaitForBeadsScopeReadyAfterRecoveryRefusesAStalledScopeBehindThePolicyWrap` and the four policy/plain/graph wrapper variants PASS. Policy and emitting wrappers forward the capability; unavailable backends return the explicit unavailable sentinel.
- Behind-target errors name the store/current/target and designated-migrator recovery after all clients are upgraded. Ahead-of-target cursors retain existing skew handling. The five `TestCheckSchemaAtTarget*` cases PASS.
- The reviewer-added `TestNativeDoltStoreSchemaCursorReadsTheCursorOfALiveHandle` passes stalled/at-target/ahead cases; `TestNativeDoltStoreSchemaCursorSurfacesAFailedRead` and `TestNativeDoltStoreSchemaCursorIsUnavailableWithoutAPool` PASS. These exercise the live database handle rather than accepting an unavailable cursor for every store.
- Subprocess-only BdStore has no live handle. Its documented residual gap is explicitly permitted by the architecture ruling; this gate does not claim it is closed.

Limits carried from the PASS review: the Go wait has no short stall budget and uses its full deadline; the throwaway shell probe costs approximately 3–5 seconds, has no trap, and rehomes HOME without scrubbing all XDG/BD/BEADS inputs. Its version regex has no trailing boundary. Since that review, head `7739841` bounds the probe (`GC_BD_SCHEMA_PROBE_TIMEOUT_S`, default 60s, through `timeout(1)` when it is present) and keeps the last five lines of a failed probe's output in the error; where `timeout(1)` is absent the probe still has no bound. It also adds `keepSchemaErrPastDeadline`, so a cursor read that runs into the shared deadline no longer hides the stalled-migration error seen before it. The Go poll's unclosed store handles predate the diff. Proxied-Dolt returns early here and has a separate admission cursor comparison, read by review but not exercised by this gate. These were non-blocking review observations; no waiver or new fix is claimed.

## Full-suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
heavy_mode: none
failure_attribution: none
waiver_ref: none
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/c3-run/evidence/remote/logs` (all per-job logs retained).

Actual invocation:
```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-6kommgh7 DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-6kommgh7.9nRjMB --base f932d43b1c70b14103687b1f3e0dd457b23babd0 --bead-json /var/tmp/deploy-ga-6kommgh7.ub8f4pkp/bead-c3.json --owned /var/tmp/deploy-ga-6kommgh7.ub8f4pkp/owned.tsv --run-dir /var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence
```
Five forced-uncached remote phases followed by the computed native floor. Exact phase commands:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
diff_tests_executed:
  TestBeadPolicyStoreReachesTheSchemaCursorThroughEveryWrap PASS
  TestBeadPolicyStoreSchemaCursorPassesAStoreAtTarget PASS
  TestBeadPolicyStoreSchemaCursorReportsAnUnsupportedBacking PASS
  TestEmittingClassStoreOverClaimsArePinnedPerEngine PASS
  TestForceReinitGuardWaitGivesUpOnStalledCursor PASS
  TestForceReinitGuardWaitOutlastsCursorAdvancing PASS
  TestForceReinitGuardWaitOutlastsSettleTimeoutWhileCursorAdvances PASS
  TestGcBeadsBdInitKeepsMetadataAndReseedsForcedOnRetryAfterForcedFallback PASS
  TestGcBeadsBdInitMetadataOnlyFallsThroughToForcedBdInitWithPinnedDatabaseWhenSchemaMissing PASS
  TestGcBeadsBdInitNeverHealsWhileAnotherInitializerHoldsTheLock PASS
  TestGcBeadsBdInitPinsManagedDoltEnvForBdSubcommands PASS
  TestGcBeadsBdInitRetriesPlainInitWhenSchemaStillMissingAfterSuccess PASS
  TestGcBeadsBdInitVerifiedEmptyStoreInitializesWithoutForce PASS
  TestNativeDoltStoreSchemaCursorEmbeddedAndClosed PASS
  TestSchemaCursorExistsOnTheSourceEngineAndBothWrappers PASS
  TestWaitForBdRuntimeSchemaFailsLoudlyWhenTargetUnknown PASS
  TestWaitForBdRuntimeSchemaIsNotReadyWhileCursorBehindTarget PASS
  TestWaitForBdRuntimeSchemaNeverCompletesTheMigrationItself PASS
  TestWaitForBdRuntimeSchemaPassesAheadOfTargetCursorThrough PASS
  TestWaitForBdRuntimeSchemaSucceedsWhenCursorReachesTarget PASS
  TestWaitForBeadsScopeReadyAfterRecoveryRefusesAStalledScopeBehindThePolicyWrap PASS
  TestCheckSchemaAtTargetAcceptsACursorAtTarget PASS
  TestCheckSchemaAtTargetLeavesSchemaSkewToTheOpenPath PASS
  TestCheckSchemaAtTargetRefusesAStalledMigration PASS
  TestCheckSchemaAtTargetSurfacesAFailedCursorRead PASS
  TestCheckSchemaAtTargetWithoutACursorCapabilityIsANoOp PASS
  TestNativeDoltStoreSchemaCursorIsUnavailableWithoutAPool PASS
  TestNativeDoltStoreSchemaCursorReadsTheCursorOfALiveHandle PASS
  TestNativeDoltStoreSchemaCursorSurfacesAFailedRead PASS
GATE_TEST_EVIDENCE_RESULT ok: 29 diff-owned tests verified by name

Native floor: 16 steps, all exit 0. ICU 74 check OK with recorded provenance; rootless podman ping OK. Pinned test bd, wrapper behavior, BEPs and all raw logs are retained. The reviewer's suite is not used as deploy evidence.

Compact execution record (command and per-test prose already shown above); large arrays and the complete original record are retained by pointer:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":66.95933151245117,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13682.507,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":71.934,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=38.63 load5=33.34 load15=18.61 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 26.47 30.90 18.26 9/1010 56128"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-6kommgh7.ub8f4pkp/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214980}}},"test_counts":{"PASS":156932,"FAIL":0,"SKIP":613}}
```

## Skip integrity

skip_justification: no diff-owned SKIP. Ownership recomputed at the tested merge equals the reviewed-source ownership. Every skip maps to an unchanged source site with line_in_hunk=no and its printed reason retained. Shared/helper hunks and their test-binary import reach were computed; every reached skip was measured by name at this gate's pinned base, under the head job's environment/tags/flags and wrappers. These are skip measurements only; the full-suite result above remains the gate evidence.

remote: 599 skip events, 316 unchanged sites; no unmapped or changed skip sites.

native: 72 skip events, 45 unchanged sites; no unmapped or changed skip sites.

SHARED-HUNKS entries:
- `cmd/gc/bead_policy_store_schema_cursor_test.go`
- `cmd/gc/beads_provider_lifecycle_test.go`
- `cmd/gc/class_store_cache_test.go`
- `cmd/gc/gc_beads_bd_force_reinit_guard_test.go`
- `cmd/gc/gc_beads_bd_schema_target_test.go`
- `cmd/gc/gc_beads_bd_trace_lint_test.go`
- `internal/beads/native_dolt_readonly_test.go`
- `internal/beads/schema_cursor_ready_test.go`

Computed closure pairs: 51; reached remote skip events: 491; unreached remote events: 108; native measurement jobs: 4.

COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a11, head rbe-a10)

COMPARE-SUMMARY head_skips=104 judged=104 pre-existing=104 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 3248532948, head 3612793672)

COMPARE-SUMMARY head_skips=359 judged=359 pre-existing=359 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

The shared hunks are fixture-only schema reporters/wrapper constructors, fake-bd cursor/target responses, capability census assertions, the trace fork allowlist, and imports supporting those tests. Each is named in the retained ownership explanation and per-package closure record. Existing OS, privilege, opt-in, process-tier and fixture predicates remain unchanged; the base measurements prove their same reasons. Changed production code checks or forwards cursors without changing these skip predicates. Unreached packages have no shared/helper input in their computed test-binary closure. The first base driver stopped after 21 passing remote results because its artifact reader received a Path instead of text. Its original script and terminal error are retained. The continuation adopted those exact results for post-processing and ran only the unspent measurements. No suite or failed test was repeated into PASS. Original logs and inventories remain unchanged.

## Fast lane and host load

policy_lane: Bash syntax of the changed bd adapter; shell guards and 14 Bazel format/policy targets, exit 0 through gate-base.sh run in a fresh view.
drift_lane: seven generated-artifact targets, OpenAPI compatibility and make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec plus git diff --exit-code, all exit 0 in independent fresh views. Fast lane finished before suite environment setup or launch.

known_red_precheck:
```text
known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)
```
Native-floor load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=7.15 run_max_load=9.79 run_mean_load=8.36 run_readings=6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.15 run_max_load=9.79 run_mean_load=8.36 run_readings=6 wait_first_load=7.15 wait_max_load=7.15 wait_mean_load=7.15 wait_readings=1 read_errors=0
```
Skip-measurement load:
```text
base-skips
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=9.54 run_max_load=8.93 run_mean_load=8.69 run_readings=2
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.54 run_max_load=8.93 run_mean_load=8.69 run_readings=2 wait_first_load=9.54 wait_max_load=9.54 wait_mean_load=9.54 wait_readings=1 read_errors=0

base-skips-continue
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=8.02 run_max_load=11.26 run_mean_load=8.91 run_readings=8
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.02 run_max_load=11.26 run_mean_load=8.91 run_readings=8 wait_first_load=8.02 wait_max_load=8.02 wait_mean_load=8.02 wait_readings=1 read_errors=0
```
Own CI compile-job load:
```text
windows-lane
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=10.69 run_max_load=13.96 run_mean_load=12.97 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.69 run_max_load=13.96 run_mean_load=12.97 run_readings=3 wait_first_load=10.69 wait_max_load=10.69 wait_mean_load=10.69 wait_readings=1 read_errors=0

pack-gate
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=13.20 run_max_load=13.48 run_mean_load=12.73 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.20 run_max_load=13.48 run_mean_load=12.73 run_readings=3 wait_first_load=13.20 wait_max_load=13.20 wait_mean_load=13.20 wait_readings=1 read_errors=0
```

## Required-job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-ga-6kommgh7.9nRjMB", "f932d43b1c70b14103687b1f3e0dd457b23babd0", "5a723e788d2874cbd931f7434efba8c86edcb0a6", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff f932d43b1c70b14103687b1f3e0dd457b23babd0...5a723e788d2874cbd931f7434efba8c86edcb0a6: 17 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               MATCH internal/beads/BUILD.bazel, internal/beads/native_dolt_readonly_test.go, internal/beads/schema_cursor_ready_test.go
suite_mode: full  (shared filter matched)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```
ci_required_accounting:

| Job | Status | Command / evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/closure.txt"} |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"proof": "ci-required/accounting.json and per-job reach files (hashed below)", "reach_rc": 0, "resolved_runs_on": "blacksmith-4vcpu-windows-2025", "windows_reach": []} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"proof": "ci-required/accounting.json and per-job reach files (hashed below)", "reach_rc": 0, "resolved_runs_on": "blacksmith-4vcpu-windows-2025", "windows_reach": []} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/windows-lane.sh", "rc": 0, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/windows-lane-terminal.stdout"} |
| pack-gate | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/pack-gate.sh", "precondition_probe_rc": 0, "rc": 0, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/pack-gate", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/pack-gate-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/pack-gate-terminal.stdout"} |

windows_reach: credential-provider-windows: []; windows-lane-test: [].
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list change).

Exact CI compile-job commands:
```bash
#!/usr/bin/env bash
set -euo pipefail
# Build the Windows allowlist
GOOS=windows go build $(cat .github/windows-lane/build.txt)
# Compile the Windows allowlist's tests
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
# Build the nogo analyzers and their tests for Windows
(
cd tools/nogo
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
# Build every package and its tests for macOS
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
# Pack compatibility job
#!/usr/bin/env bash
set -euo pipefail
for attempt in 1 2 3; do
  if scripts/update-bundled-gastown-pack --check; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
# Live smoke step environment
export GC_TEST_GASCITY_PACKS_REGISTRY=main
#!/usr/bin/env bash
set -euo pipefail
for attempt in 1 2 3; do
  if make test-pack-registry-live; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
```
This PASS authorizes PR publication. Exact-head CI / required and every deferred job must succeed before MPR merges.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs`; original run root: `/var/tmp/deploy-ga-6kommgh7.ub8f4pkp`. The exact tested merge is retained in a verified Git bundle.

Full execution retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/c3-run/evidence/execution.json; SHA256 9703042ce813b3e5542d540fe20fd364d3449521e241a3778ac6bc40c7ce4bc1.

Full execution projection retained: 5991 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/execution-record-summary.json; SHA256 509ff675068a91c2d8a08ae8f1fa813345b648d7a3fb0f616797a188356d4ce4.

Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/c3-run/evidence/remote/skip-inventory.json; SHA256 456393ccbf8b36942424054a9bc42c2f6e5a180dc1e241b941bb114cfc0d007e.

Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/native-skip-inventory.json; SHA256 ebac181f9650f9a71058f85e15816c412b87da2629c1be305fa488862b667a3b.

Full remote sites retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/remote-skip-site-results.json; SHA256 98085faef2ca2602e90612c829edeff75a63507fd0ff5e5502f2fb2e7d53e59b.

Full native sites retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.

Full shared hunks and closures retained: 13466 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/skip-measure-plan.json; SHA256 ce01e3ba7a38cb40124d41439703364e5501476fbe6ab841e5e6c8a61d43956a.

Full base comparison verdicts retained: 805 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/skip-comparisons.json; SHA256 509c89e56d0da90fd526b6c45161f6684e2f76de0c58602d71341790e29b0ef9.

Full base remote commands and results retained: 379 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/base-skips/remote-results.json; SHA256 ea44c61de5ecb0b5eea1a9179a91886e327b5161f3e0c77345828eace2bcd924.

Full CI accounting retained: 113 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/accounting.json; SHA256 ca370dbae75801f8a7af8a426376a9b2c034e2ea3715a9012f922a81b93f1712.

Full Windows native-job reach retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/ci-required/windows-lane-test-reach.json; SHA256 0f91e97f6a0f70bccbef1de2cd3c20f048ee42dd81166e2a125dca095f214ea5.

Full reviewer tests retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-6kommgh7/gate-20261118T212927Z-d3p02fxs/reviewer-tests-check.stdout; SHA256 6d6b24f0c634b059356929faa7c371246830c434516dc0cd559addf06561004a.
