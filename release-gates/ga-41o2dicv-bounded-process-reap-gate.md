# Bounded managed-process reaping release gate

**Verdict:** **PASS**

Bead: ga-41o2dicv
Deploy target: `deploy/ga-41o2dicv-gate`; publication follows the completed gate.
Gated source: `79f31667c460150c3ba76fd425ea75b5f177e949`
Base: `6bd609f5bfb41f43f444843b1220d0cde4473446`
Synthetic merge: `6beef4afc1781a687080678fc94c0b42b2feec0d`; tree `da4d7a267a32d891e1fa0e4df27caacec39bb240`.
Evaluated: 2026-11-11T09:56:39.243740+00:00

The immutable reviewed source was validated on a fresh synthetic merge with the frozen base. Publication cuts the mechanically named isolated branch at that exact source and adds only this gate record. No source rebase, shared-branch push, live install or merge is performed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Original style/spec/security PASS in ga-3xtka4il binds this exact resolved source; no review carryover or reviewer-added-test exception. |
| 2 | Acceptance criteria met | PASS | Post-SIGKILL wait bounded by the 3s reap grace; typed non-gone error survives both socket protocols and wrapped/joined causes; both client call sites use derived timeout helpers; bookkeeping remains for retry. Sixteen owned functions pass by package/name. Required Stop-callers table and Refs #7170/separate followup constraints verified in the final PR draft. |
| 3 | Tests pass | PASS | Full canonical five-phase uncached remote suite, all native floor checks, policy/generated drift passed; every required job is accounted for, with native Windows tests deferred to PR CI. Unchanged non-owned skips verified below. |
| 4 | No high-severity findings open | PASS | No unresolved HIGH finding in the exact-source reviewer PASS. Original low timing-guard and dial-timeout-mutation advisories are retained; actual helper call sites independently checked. |
| 5 | Final branch clean | PASS | Reviewed source and synthetic validation tree are clean; publication adds only this record on the isolated deploy branch and verifies clean status after its normal commit hooks. |
| 6 | Merge state recorded; build/vet pass | PASS | Clean frozen merge; normal hooks, full Bazel build and nogo vet pass, with successful terminal BEPs and exact parent/tree identity. |
| 7 | Single feature theme | PASS | One managed-process termination and stop-error propagation theme across runtime, acp and subprocess; owning requirements/docs and regression tests are part of that same boundary. |

merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446)
gate_base_kind: current-tip
gate_base_sha: 6bd609f5bfb41f43f444843b1220d0cde4473446
merge_validation: synthetic clean merge tested; MPR retains merge-time validation and exact-head authority.
deploy_mode: remote; push_remote: fork; new isolated deploy branch required; builder source branch is provenance only.
project_manifest: docs/PROJECT_MANIFEST.md is absent; supplied release-gate criteria and current TESTING.md/Makefile/CI determine the checks.

## Criterion 3 evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ga-41o2dicv.json --owned /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/owned.tsv --run-dir /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence — exit 0
test_cmd_scope: full-suite
test_counts: PASS=150721 FAIL=0 SKIP=515
Counts are top-level Go result lines, once per job. Separate skip inventories include subtests: 501 farm events and 39 native events. A configured Python skip is recorded separately.
test_log_dir: /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 408, "results": 408, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 76.06547546386719, "waited_seconds": 0.0}
farm_test_seconds: 11984.407
owned_topup: none; all 16 owned package/test roots passed in their own full-suite package artifacts (15 distinct names; same-named tests in both providers bound separately).
floor_topup: complete, 16 commands exit 0; environment-absence packages, beadstest, Darwin compile and runner self-tests.
skip_inventory: 501 farm events; 39 native events; retained with per-test reasons and sites.
latency_leg: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS; first attempt; probe clear; no latency test ran in the main integration leg.
heavy_mode: none (computed classifier).
waiver_ref: none (no gascity waiver path).
failure_attribution: none; no failing named test, target or floor step.
ci_lane_run: n/a (no CI-config change).

diff_tests_executed:
  TestIsSessionGoneRefusesProcessNotConfirmedDead PASS (unit-12382fa48a81def0b895.log)
  TestManagedProcessReapGraceIsBounded PASS (unit-12382fa48a81def0b895.log)
  TestProcessNotConfirmedDeadIsNeitherGoneNorRefused PASS (unit-12382fa48a81def0b895.log)
  TestStopReplyNotConfirmedDeadIsADistinctSingleToken PASS (unit-12382fa48a81def0b895.log)
  TestTerminateManagedProcessDoneInsideReapGraceIsSuccess PASS (unit-12382fa48a81def0b895.log)
  TestTerminateManagedProcessNotConfirmedDeadWhenDoneNeverCloses PASS (unit-12382fa48a81def0b895.log)
  TestConfigStopSocketTimeoutOutlastsStopGraceAndReapGrace PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)
  TestHandleControlConnStopRepliesNotConfirmedDead PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)
  TestStopBySocketKeepsUnexpectedResponseForOtherReplies PASS (integration-packages-74db6ddfc6b7303645d2.log, integration-packages-cc430839a756656daa1e.log +2 more)
  TestStopBySocketNotConfirmedDeadKeepsSidecars PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)
  TestStopInMemoryNotConfirmedDeadKeepsOwnMeta PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)
  TestHandleSessionConnStopRepliesNotConfirmedDead PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)
  TestStopBySocketMapsNotConfirmedDeadReply PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)
  TestStopNotConfirmedDeadDropsEntryAndIsRunningFollowsSocket PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)
  TestStopSocketTimeoutOutlastsStopGraceAndReapGrace PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)
GATE_TEST_EVIDENCE_RESULT ok: 15 diff-owned tests verified by name

### Full-scope phase commands

```text
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
bazel test --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/unit.bep.json --keep_going //...
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1794385029237 end_ms=1794385189765
```

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1794385190405 end_ms=1794385411779
```

```text
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1794385412247 end_ms=1794385667563
```

```text
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1794385668249 end_ms=1794386088788
```

```text
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1794386089475 end_ms=1794386183574
```

### Native floor commands

- cmd/gc: `go test -tags integration -count=1 -v -run '^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-0.log
- examples/gastown: `go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-1.log
- internal/beads: `go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-2.log
- internal/beads/beadstest: `go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-3.log
- internal/beads/contract: `go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-4.log
- internal/beads/exec: `go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-5.log
- internal/doctor: `go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-6.log
- internal/mail/exec: `go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-7.log
- internal/runtime/herdr: `go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-8.log
- internal/runtime/runtimetest: `go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-9.log
- internal/runtime/tmux: `go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-10.log
- test/acceptance/helpers: `go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-11.log
- test/integration: `go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-12.log
- fsys-darwin-compile: `make test-fsys-darwin-compile`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-13.log
- push-gate-lock-selftest: `bash scripts/test-push-gate-lock.sh`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-14.log
- local-concurrency-selftest: `bash scripts/test-local-concurrency.sh`; exit 0; log /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs/floor-15.log

### Fast lanes and build

policy_lane: fresh-tree + gate-base pinned execution of fmt-check-changed, full nogo lint, shell guards, hook verification and 13 Bazel policy targets — PASS.
drift_lane: fresh-tree + gate-base pinned make bazel-sync, git diff --exit-code and clean untracked inventory, seven generated/client/lockfile targets — PASS.
openapi_lane: fresh-tree pinned openapi-breaking-check — PASS.
storeclass_policy: strict trusted build, baseline and zero-growth checks — PASS.
criterion6: bazel build --config=remote-exec --noremote_local_fallback --keep_going //...; make vet with full nogo flags and remote execution — both exit 0.

- fast policy: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- bash /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/fast-lane.sh policy; cwd=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge; GATE_FAST_TRANSPORT=remote-exec; exit=0; commands retained in fast-lane.sh and fast-output.txt.
- fast drift: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- bash /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/fast-lane.sh drift; cwd=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge; GATE_FAST_TRANSPORT=remote-exec; exit=0; commands retained in fast-lane.sh and fast-output.txt.
- fast openapi: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- bash /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/fast-lane.sh openapi; cwd=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge; GATE_FAST_TRANSPORT=remote-exec; exit=0; commands retained in fast-lane.sh and fast-output.txt.
- strict storeclass: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/storeclass-policy-body.py; exit=0; trusted-build/baseline/growth all exit=0; storeclass-proof/verified.json.

### Skip justification

Reviewed the five changed production files and all changed runtime/acp/subprocess shared hunks. The bounded reap timer, sentinel classification and socket response/timeout changes are covered by all 16 owned package/test roots. Skip inventories preserve every event, reason and original log; no owned root skips. All 328 farm/native skip sites have identical entire source blobs at the frozen base and gated merge. Actual default/integration/acceptance test-binary closures select every package reaching any of the three changed runtime packages, including transitive helper imports. All 472 reached farm events and 38 reached native events reproduce the same reason on the frozen base under the corresponding phase/tag/environment. Remaining events have unchanged sites and no shared/helper reach. Reviewed reasons cover intentional unit-lane process exclusions, platform restrictions, absent optional infrastructure, fixture/capability restrictions, existing tracked pending behavior and helper-only tests; none is credited as runtime coverage. Every environment-absence package was included in the mandatory native floor, retaining its non-owned skips. The configured non-Go lockfile skip is separately source/macro/input verified.
Same-reason BASE_REF comparisons: 472 farm events and 38 native events; all pre-existing, no new/reason-changed/diff-owned/unjudged result.
Changed shared/helper hunks, exact source sites and actual test-binary import closures under their original tag sets were independently reviewed in manual-final-review.json and skip proof. Baseline comparisons cover every reached skip. Unreached skips have unchanged sites and no shared/helper reach. No owned test skipped.
Non-Go skip: LockfilesInSyncTest.test_rules_ts_typescript_matches_lock is configured inapplicable for openclaw-bridge (no --rules-ts-typescript); its source, macro and Bazel inputs match base. The opted-in dashboard counterpart passed all three tests.

### Required-job accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 79f31667c460150c3ba76fd425ea75b5f177e949 fork; exit 0; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
```text
diff 6bd609f5bfb41f43f444843b1220d0cde4473446...79f31667c460150c3ba76fd425ea75b5f177e949: 15 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:

- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 79f31667c460150c3ba76fd425ea75b5f177e949 fork; exit=0; log=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/runner-policy-output.txt python3 .github/workflows/scripts/runner_policy.py; exit=0; log=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/runner-policy.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-native-reach.json
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/run-ci-job.py windows-lane; exit=0; logs=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane.stdout, /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane.stderr
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane run (.): `GOOS=windows go build $(cat .github/windows-lane/build.txt)`; exit=0; logs=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-3.stdout, /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-3.stderr
- windows-lane run (.): `GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)`; exit=0; logs=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-4.stdout, /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-4.stderr
- windows-lane run (tools/nogo): `GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...`; exit=0; logs=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-5.stdout, /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-5.stderr
- windows-lane run (.): `GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...`; exit=0; logs=/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-6.stdout, /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/windows-lane-6.stderr

windows_reach: {"credential": [], "native": []}; each job closure computed separately.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Gate PASS authorizes opening the PR. Merge requires current-head CI / required and every deferred job to be successful; the deployer does not merge or claim CI is green before the PR exists.

### Observed load

storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.48 run_max_load=12.87 run_mean_load=12.82 run_readings=3 wait_first_load=12.48 wait_max_load=12.48 wait_mean_load=12.48 wait_readings=1 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.00 run_max_load=41.15 run_mean_load=14.47 run_readings=48 wait_first_load=11.00 wait_max_load=11.00 wait_mean_load=11.00 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.94 run_max_load=41.29 run_mean_load=28.86 run_readings=9 wait_first_load=11.94 wait_max_load=11.94 wait_mean_load=11.94 wait_readings=1 read_errors=0
ci-required/windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=55.74 run_max_load=54.07 run_mean_load=51.00 run_readings=3 wait_first_load=40.15 wait_max_load=72.63 wait_mean_load=45.89 wait_readings=61 read_errors=0
skip-base-evidence/native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=15.07 run_max_load=14.35 run_mean_load=13.84 run_readings=2 wait_first_load=52.27 wait_max_load=77.90 wait_mean_load=56.14 wait_readings=61 read_errors=0

### Retained audit evidence

Full full execution retained: 5069 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/execution.json; SHA256 91227c5690fe6b6131c9065996ac1103ba684e5d19df979b0fc8e8b05562dba9.
Full raw suite audit retained: 122 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/resume-c3-audit.json; SHA256 3e6844340fad590ec6dc3cb73ffb47da48991fe466204eff99444b729636a547.
Full owned inventory retained: 61 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/owned-explain.txt; SHA256 9bc93310c2c631e0b217f30f9e85d04f6785c3c6e08650906dbfed45e110a2de.
Full farm skip inventory retained: 4010 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/skip-audit-inventory.json; SHA256 b29e46d1e3941faef3542ae069c0c71a177e3cbf6d044ce8f015888a3748a1cd.
Full native skip inventory retained: 314 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/floor-skip-inventory.json; SHA256 1642737c907ec30259ec5dc391d150a783b3b26b537a5c9944c80d44c516b5c8.
Full farm skip sites retained: 292 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/skip-sites.txt; SHA256 efd65f2684509763b9cdd5c599d65855aa22faa533bf058972978b12ad709f44.
Full native skip sites retained: 36 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/native-skip-sites.txt; SHA256 486897452a262755460103db1d969e39db17196a9f7eb13d1cc4b2a08f1bae52.
Full skip comparison proof retained: 2210 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/skip-proof-automated.json; SHA256 7a43af2a2cfc41a01e0bcdf7da45c752dbeb58c13c6c59b655550df07a8e674f.
Full manual source and skip review retained: 61 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/manual-final-review.json; SHA256 c47b1c3357a8a61ac62cb54c70bee32df5fa44fd63e92a4bc17ce5299ff1c54d.
Full required-job accounting retained: 69 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/ci-required/accounting-verified.json; SHA256 381cd49cb52232dab44150910c3e4f858f39feb9516682845810e3a5c093d2f7.
Full C6 raw proof retained: 38 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c6-verified.json; SHA256 4b7b97dd57fe50d86b6efeec0b8263e38d1f942e1192664833d83280186020aa.
Full non-Go skip proof retained: 34 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/non-go-skip-reviewed.json; SHA256 cdfdf3cffdcfe600e641f7f3ced47871fbdcc94f6aa1304a2bb1d07d18ddf18e.
Full owned package proof retained: 219 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/owned-package-verified.json; SHA256 d35ede9f3cfe0d974fbc7c079aa8340e950a42deb868b0b7d4287180ea6ff8ac.
Full source/caller acceptance retained: 46 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/acceptance-precheck.json; SHA256 e76760eb47f7e52aba05e79d06cbb0c3ff6ee85b7220a783b79dc85fa499856e.
Full Stop-callers source audit retained: 481 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/stop-callers-source-audit.json; SHA256 eb44a418bcfb7e42f25c4da55b468faa8bc81480f5e7b8d7bbb973ef8eec7b9f.
Full final PR body constraints retained: 13 lines; /var/tmp/deploy-ga-41o2dicv.yuvwdeqo/pr-description-verified.json; SHA256 553bd9cfa3d1f9945e5dab010733a23fcbd68511bfc80c52197f8cd6d665febf.

Compact execution:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":76.06547546386719,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":408,"remote_executed":408,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":11984.407,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":91.577,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=86.79 load5=87.15 load15=45.80 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=40s load1=44.51 load5=76.23 load15=43.86 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.66 69.83 42.76 7/1056 55664"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30988}},"log_dir":"/var/tmp/deploy-ga-41o2dicv.yuvwdeqo/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":429,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":501,"bytes":178579}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":150721,"FAIL":0,"SKIP":515},"diff_tests_executed":"diff_tests_executed:\n  TestIsSessionGoneRefusesProcessNotConfirmedDead PASS (unit-12382fa48a81def0b895.log)\n  TestManagedProcessReapGraceIsBounded PASS (unit-12382fa48a81def0b895.log)\n  TestProcessNotConfirmedDeadIsNeitherGoneNorRefused PASS (unit-12382fa48a81def0b895.log)\n  TestStopReplyNotConfirmedDeadIsADistinctSingleToken PASS (unit-12382fa48a81def0b895.log)\n  TestTerminateManagedProcessDoneInsideReapGraceIsSuccess PASS (unit-12382fa48a81def0b895.log)\n  TestTerminateManagedProcessNotConfirmedDeadWhenDoneNeverCloses PASS (unit-12382fa48a81def0b895.log)\n  TestConfigStopSocketTimeoutOutlastsStopGraceAndReapGrace PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)\n  TestHandleControlConnStopRepliesNotConfirmedDead PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)\n  TestStopBySocketKeepsUnexpectedResponseForOtherReplies PASS (integration-packages-74db6ddfc6b7303645d2.log, integration-packages-cc430839a756656daa1e.log +2 more)\n  TestStopBySocketNotConfirmedDeadKeepsSidecars PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)\n  TestStopInMemoryNotConfirmedDeadKeepsOwnMeta PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)\n  TestHandleSessionConnStopRepliesNotConfirmedDead PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)\n  TestStopBySocketMapsNotConfirmedDeadReply PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)\n  TestStopNotConfirmedDeadDropsEntryAndIsRunningFollowsSocket PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)\n  TestStopSocketTimeoutOutlastsStopGraceAndReapGrace PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)\nGATE_TEST_EVIDENCE_RESULT ok: 15 diff-owned tests verified by name\n"}
```
