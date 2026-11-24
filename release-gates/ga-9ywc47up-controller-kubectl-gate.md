# Controller kubectl security pin — release gate

**Verdict:** **PASS**

Bead: ga-9ywc47up. Build: ga-68213l47. Review: ga-3y5zdina.
Reviewed source: e786e86217bfd5cef7712aabeed4da6db2efcf0d.
Validation merge: e116477392ca551958f94b166ef73d7d6f4493a1.
gate_base: 42f005d69334630c326bb3eac685543a57f47ccf
gate_head: e786e86217bfd5cef7712aabeed4da6db2efcf0d
DEPLOY_MODE: remote. Push target: fork. Base branch: main.
merge_state: clean (origin/main@42f005d69334630c326bb3eac685543a57f47ccf)
gate_base_kind: current-tip
merge_validation: merge result built and vetted here; final merge belongs to MPR.
Evaluation order: preflight (no existing PR), 6, 1, 2, fast lane, 3, 4, 5, 7.
Draft PR: https://github.com/gastownhall/gascity/pull/9487, authorized by mayor mail gcg-16211969.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — reviewer ga-3y5zdina records PASS at the exact reviewed source; no carryover or reviewer-added test commit. |
| 2 | Acceptance | PASS — kubectl v1.37.2 pin; waiver IDs, scopes and expiry dates unchanged; no CVE-2026-39821 waiver. Actual four-image CI build/scan succeeds for gc-controller and all three companion images. |
| 3 | Tests | PASS — canonical full-scope gate, all five uncached remote phases and sixteen native jobs complete; 159157 PASS / 0 FAIL / 618 top-level SKIP results. Owned regression passes by name; complete skip proof retained. Six-leaf required-CI accounting complete below. |
| 4 | No open HIGH review findings | PASS — zero HIGH/major review findings. Minor comment wording findings remain non-blocking. |
| 5 | Clean final branch | PASS — source feature checkout and validation scratch clean before adding this gate-only record; post-commit cleanliness is checked before push. |
| 6 | Merge state and build | PASS — clean merge on the pinned base; bazel build --config=remote-exec --noremote_local_fallback //... succeeds for 1500 targets, including nogo/vet. |
| 7 | One feature theme | PASS — controller image kubectl advisory fix only: Dockerfile pin, matching waiver explanations, regression guard. |

## Test and environment evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: 159157 PASS, 0 FAIL, 618 SKIP
Counts are column-zero Go result lines, summed per retained job; they are not unique test identities.
diff_tests_executed: TestKubectlPinClearsCVE202639821 PASS (unit-9f907e0fea7ca680f21c.log)
test_log_dir: /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/logs (570 retained logs)
diff_owned_evidence: /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/owned-explain.txt
not_owned_skip_evidence: /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/not-owned-skip-evidence.json
skip_justification: All 676 SKIP events including subtests map to 319 unchanged source sites. No skipped scripts test, modified skip condition, shared/helper hunk, production Go input, dependency manifest, census row or suite target changed. Remaining skips cover unchanged platform, optional service, live-provider and explicitly gated paths; no owned SKIP. The controller image is independently exercised by actual CI.
failure_attribution: none (no failed tests)
waiver_ref: none
heavy_mode: none (classifier verified)
delta_gate: none (new deploy; off)
fallback_reason: none
farm_test_seconds: 14483.168
rbe_state: generated definitions present, kill switch absent, certificate valid, worker-env pins equal; compact record below.
admission: remote, floor25GiB, cap2, initial free40.39268493652344GiB, waited0.001s.
owned_topup: none (owned test already executed in the unit phase)
floor_topup: complete; all16 serial commands exit0.
latency_leg: both registered budget tests PASS after a clear probe; load1=19.24 load5=22.92 load15=16.57, cpus32, threshold50, waited0s.
load_threshold: 15
load_waited_seconds: 1680
load_wait_timed_out: 0
run_start_load: 14.56
run_max_load: 15.62
run_mean_load: 14.89
run_readings: 7
wait_first_load: 87.50
wait_max_load: 87.50
wait_mean_load: 39.53
wait_readings: 57
read_errors: 0

~~~text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1680 wait_timed_out=0 run_start_load=14.56 run_max_load=15.62 run_mean_load=14.89 run_readings=7 wait_first_load=87.50 wait_max_load=87.50 wait_mean_load=39.53 wait_readings=57 read_errors=0
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/merge/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
~~~

The first incomplete C6 build started locally because the temporary tree lacked generated remote definitions. Only that owned build service was stopped before any completed build/test verdict. Definitions were installed with the standard helper and the same tree built successfully on the farm; both logs remain retained. All fast lanes used independent fresh views.

fast_lane: PASS — go_format_test; seven shell guards; whole-tree make lint/nogo; CI's make bazel-sync followed by git diff --exit-code.
policy_lane: make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-gomod-replace check-core-boundary check-eventexport-isolation — exit0; make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback' — exit0.
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'; git diff --exit-code — exit0.
known_red_precheck: proceed, nothing-known-red; check-routed-test-rows green (stale tracker ga-1ptv4jey).
ci_lane_run: n/a (no CI job, matrix, timeout or required-check definition changed).

Actual phase flags, from this run:
~~~text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
~~~

Phase goals: unit make -k check; acceptance both documented acceptance targets; integration-packages //test:integration_packages; full integration //test/integration:integration_test with only the canonical latency registry split out; dedicated latency phase runs that registry alone. Each phase's BEP predicate confirms remote strategy, attempt1, no caches, no retries and an in-window terminal verdict: unit426 + acceptance25 + integration-packages87 + integration-full15 + latency1 =554 PASS results.

Native floor commands (actual argv, each exit0):
~~~text
go test -tags integration -count=1 -v -run '^(TestCollectInputConvoyRootsAgreesWithTheFrozenReference|TestConvoyAutocloseAgreesWithTheFrozenReference|TestMoleculeAutocloseAgreesWithTheFrozenReference|TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc ; exit0
go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown ; exit0
go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads ; exit0
go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest ; exit0
go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract ; exit0
go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec ; exit0
go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor ; exit0
go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec ; exit0
go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr ; exit0
go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest ; exit0
go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux ; exit0
go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers ; exit0
go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration ; exit0
make test-fsys-darwin-compile ; exit0
bash scripts/test-push-gate-lock.sh ; exit0
bash scripts/test-local-concurrency.sh ; exit0
~~~

## Required CI and image acceptance

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/merge 42f005d69334630c326bb3eac685543a57f47ccf e786e86217bfd5cef7712aabeed4da6db2efcf0d fork
ci_required_workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
ci_required_leaf_count: 6
ci_required_summary: 6 LEAF jobs need a status; the rest are rolled up through their needs.
ci_required_tool_exit: 0
ci_required_accounting:
~~~text
runner-policy | CI-PASS https://github.com/gastownhall/gascity/actions/runs/45461281279 | https://github.com/gastownhall/gascity/actions/runs/45461281279/job/138684200237
changes | CI-PASS https://github.com/gastownhall/gascity/actions/runs/45461281279 | https://github.com/gastownhall/gascity/actions/runs/45461281279/job/138684466765
credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
windows-lane | CI-PASS https://github.com/gastownhall/gascity/actions/runs/45461281279 | https://github.com/gastownhall/gascity/actions/runs/45461281279/job/138684467228
windows-lane-test | CI-PASS https://github.com/gastownhall/gascity/actions/runs/45461281279 | https://github.com/gastownhall/gascity/actions/runs/45461281279/job/138684466819
~~~
windows_reach: none for credential-provider-windows and windows-lane-test; both own closures recorded in windows-reach.json.
deferred_to_pr_ci: none
not_covered: none

Image acceptance: https://github.com/gastownhall/gascity/actions/runs/45461280882, success at the reviewed source. Exact original controller banner:
~~~text
https://github.com/gastownhall/gascity/actions/runs/45461280882
Image vulnerabilities	Enforce image vulnerability policy	2026-11-24T18:42:23.0826580Z PASS: gc-controller:e3e675a798aa39ba9d8b9defda4293d0d41aecff has no unwaived HIGH/CRITICAL findings
~~~
All four image policy checks report PASS. The image tag is CI's synthetic merge checkout tag.

Separate scanner advisory: Go module vulnerabilities run45461280598 fails on existing OpenTelemetry GO-2026-6974. Repo-wide fix tracking ga-7bn2fn5g remains open; companion own PR9464 was confirmed open/unlanded. This is outside the six-leaf required-CI closure and is handled under the existing scanner-advisory policy, not a criterion3 waiver. Sighting f0e5f59a-e9b6-5a58-8925-6d934b92c426 and mayor notification gcg-16218113 were verified.

## Retained execution record

Post-record generated-file sync and tracked drift check also exit0; active pre-commit docsync passes. Scratch output-base release and worktree cleanup complete (release rc0,9s); all raw logs remain.

Full evidence retained at /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD, with a durable copy at /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9ywc47up/gate-20261124-McyOQBAD. Scratch cleanup runs separately; raw per-job logs remain retained.
Pack at C3 start: cc4b57a701a30c4b0453c27aa9386965edbce5c4 (recovered after launch from the local reflog at the recorded start time; equal to the later live HEAD).

~~~json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote","gate_base":"42f005d69334630c326bb3eac685543a57f47ccf","gate_head":"e786e86217bfd5cef7712aabeed4da6db2efcf0d","admission":{"class":"remote","floor_gib":25.0,"free_gib":40.39268493652344,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14483.168,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":95.623,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=19.24 load5=22.92 load15=16.57 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 16.59 21.89 16.42 5/1006 55696"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40101}},"log_dir":"/var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":225265}}},"test_cmd":{"elided_array":{"items":5,"bytes":2129}},"test_counts":{"PASS":159157,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestKubectlPinClearsCVE202639821 PASS (unit-9f907e0fea7ca680f21c.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
~~~

Full execution.json retained: 6041 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/execution.json; SHA256 8c3f1c3c7865dd03a754771c4831a6723b0327194fb7e4a27a7c53b98097f441.

Full skip inventory retained: 4834 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/criterion3/evidence/remote/skip-inventory.json; SHA256 7e989b21b3fefe861271e16518193ebb6834863b9db7ca85b0f543c574e77a5a.

Full not-owned skip proof retained: 8790 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/not-owned-skip-evidence.json; SHA256 aca9ea25fdf73567f9f01bc7a711ed78c17788c0d41b7c52c2ad640d59a9a40f.

Full skip-site proof retained: 319 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/skip-site-audit.txt; SHA256 7f036baca7793858363ad2496263ad5e4056e02fc99261f6ce6bc32c2c15feab.

Full CI closure retained: 17 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/ci-required-closure.txt; SHA256 a558906373bc988cbf53d492e1faa8af32669a550cace41191a26cb8dab1bb2a.

Full Windows reach retained: 23 lines; /var/tmp/gc-gate.ga-9ywc47up-20261124.McyOQBAD/windows-reach.json; SHA256 ca39180190fc381c800fe412c60e00d946a171b1c58aebf9a61e3f896dd3bd2e.
