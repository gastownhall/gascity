**Verdict:** **PASS**

# Compact Dolt command wiring release gate

Date: 2026-11-27T05:43:04Z

Deploy bead: ga-2k6ia0vt; build bead: ga-b4o1g4; review bead: ga-2hd3vxya.

Reviewed source: 8ce5c5e6c9bca47dec07b6533d02e6bd87d937d8

Gated merge: 0731009b9c14baeff5635a8975254341d45d894c

Gate base: 6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004

Deploy branch: deploy/ga-2k6ia0vt-gate; base branch: main; mode: remote; push remote: fork.

Criteria source: the mandatory release-gate-criteria protocol and TESTING.md. docs/PROJECT_MANIFEST.md is absent from the gated tree. Preflight found no PR carrying the reviewed source. Aging check: clean, 17 days; round cap: proceed, gate round 1.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. ga-2hd3vxya records verdict: pass at the exact resolved reviewed source. No review carryover or reviewer-added test commit. |
| 2 | Acceptance criteria | PASS. Independent control passes; M3a/M3b/M3c fixed-timeout mutants, M6 zero-default mutant, and M7 omitted-WaitDelay mutant each build and fail the intended test. Restored source hash and logs below. The diff adds no exec/sleep call site, census bump, or new test target. |
| 3 | Tests and required-job accounting | PASS. Valid full-suite remote criterion-3 result; all five phases and all 16 local floor checks pass. Both owned tests pass by name. Every CI leaf is accounted below; native Windows execution is deferred to PR CI. |
| 4 | No unresolved HIGH findings | PASS. Reviewer security findings: none. No unresolved HIGH finding. The recorded QF1001 style observation is non-blocking and confined to the integration-tagged test. |
| 5 | Clean final branch | PASS. The isolated branch was cut at the exact reviewed source and git status --porcelain was empty before this audit record was added. Publication requires a clean committed tree and a record-only child commit. |
| 6 | Merge state and build/vet | PASS. Clean merge against the pinned base; both CLI binaries build and whole-tree Bazel nogo lint/vet succeeds (1505 targets). No rebase. |
| 7 | Single feature theme | PASS. One source commit: examples/bd/dolt/compact_real_dolt_wiring_test.go plus its BUILD.bazel srcs entry, +158/-0. The record concerns the same wiring coverage. |

merge_state: clean (origin/main@6aee6c8c1046)
merge_validation: merge result built and vetted here; merge authority validates the final merge at merge time
gate_base_kind: current-tip
gate_base: 6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004
gate_head: 8ce5c5e6c9bca47dec07b6533d02e6bd87d937d8
merge_head: 0731009b9c14baeff5635a8975254341d45d894c
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
heavy_mode: none
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
owned_topup: none needed; both owned tests ran in integration-packages
fast_lane: PASS before suite launch
known_red_precheck: proceed, nothing-known-red; check-routed-test-rows green; tracker ga-1ptv4jey reported stale

## Fast lane and acceptance evidence

Build/vet command (exit 0):

```sh
bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501
make lint NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback"
```

Build/vet logs: /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion6-rbe/units/gate.w1.stdout.log and gate.w1.stderr.log.

policy_lane: PASS; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; bazel test --config=remote-exec --noremote_local_fallback //:go_format_test. Whole-tree make lint above uses Bazel nogo; no shared golangci-lint binary is used.

drift_lane: PASS; make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code, in a separate fresh view.

FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.bDxU5c head=0731009b9c14 tree=8938dcbbe41f dir=/var/tmp/gc-fresh-tree.HrLJcZ modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.bDxU5c,bazel-out
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.bDxU5c head=0731009b9c14 tree=8938dcbbe41f dir=/var/tmp/gc-fresh-tree.a9uLcD modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.bDxU5c,bazel-out

Full policy and drift logs: /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/policy.log; /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/drift.log.

Mutation results (exit 0 for the verification harness; each mutant itself exits 3 with the named executed test failing):

```text
control VERIFIED rc=0 expected=None
M3a VERIFIED rc=3 expected=TestCompactRealDoltCallWiring/runDoltForCompactTest
M3b VERIFIED rc=3 expected=TestCompactRealDoltCallWiring/doltServerQueryDBForCompactTest
M3c VERIFIED rc=3 expected=TestCompactRealDoltCallWiring/runCompactScriptForRealDoltTest
M6 VERIFIED rc=3 expected=TestCompactRealDoltDefaultWaitDelay
M7 VERIFIED rc=3 expected=TestCompactRealDoltCallWiring/waitForDoltServerQueryForCompactTest
RESTORED sha256=207071248680edaa4853d6728d0cbef6a4d4e3c42b8e6d267a90816d9bc2cdb5
```

Mutation logs: /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/mutations/{control,M3a,M3b,M3c,M6,M7}.log. Each mutation ran in a temporary fresh checkout and was restored; no mutation changed the deployed source.

## Full-suite execution

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.bDxU5c --base 6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004 --bead-json /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/bead.json --owned /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/owned.tsv --run-dir /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence; detached through gate-detached-run.sh. GC_GATE_REMOTE_ENABLED=1; GC_GATE_DELTA=shadow; full decision, no PR record.

Phase scopes: unit make -k check (//...); acceptance including solo targets; all integration-packages; full test/integration with only the protocol latency names split out; those latency names alone in integration-latency.

Actual phase flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

test_counts: PASS=159728 FAIL=0 SKIP=618 (top-level result lines across 571 retained logs; repeated executions count per job).

diff_tests_executed: TestCompactRealDoltCallWiring PASS, including runDoltForCompactTest, doltServerQueryDBForCompactTest, runCompactScriptForRealDoltTest and waitForDoltServerQueryForCompactTest; TestCompactRealDoltDefaultWaitDelay PASS.

skip_justification: no owned SKIP or FAIL. Every changed test is identified by diff-owned-tests.py and passes by name. Existing SKIP sites are unchanged; the only shared hunks introduce helpers used solely by these new tests, so they cannot alter older tests or skip conditions. The farm skip inventory has 604 rows (including subtests): 514 other and 90 environment-absence. Every environment-absence parent selected by the runner was re-run locally; all 16 floor steps exit 0. Remaining skips are non-owned opt-in, topology, platform, or environment conditions; full reasons remain in the inventory.

Local floor commands (all exit 0):

```text
go test -tags integration -count=1 -v -run '^(TestCollectInputConvoyRootsAgreesWithTheFrozenReference|TestConvoyAutocloseAgreesWithTheFrozenReference|TestMoleculeAutocloseAgreesWithTheFrozenReference|TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-0.log
go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-1.log
go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-2.log
go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-3.log
go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-4.log
go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-5.log
go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-6.log
go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-7.log
go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-8.log
go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-9.log
go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-10.log
go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-11.log
go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-12.log
make test-fsys-darwin-compile -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-13.log
bash scripts/test-push-gate-lock.sh -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-14.log
bash scripts/test-local-concurrency.sh -> exit 0; log=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs/floor-15.log
```

load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 6.91
run_max_load: 12.38
run_mean_load: 10.01
run_readings: 7
These are local floor readings. The remote phases use remote admission and a separate farm load probe for latency.

latency_leg: measured; TestGcBdLatencyBudget PASS; TestGcSeatHookLatencyBudget PASS; clear probe after 30s, threshold 50, max load1 49.65, zero over-threshold readings.

ICU check:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

Compact execution.json:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote","gate_base":"6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004","gate_head":"8ce5c5e6c9bca47dec07b6533d02e6bd87d937d8","admission":{"class":"remote","floor_gib":25.0,"free_gib":42.13275146484375,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004","head":"8ce5c5e6c9bca47dec07b6533d02e6bd87d937d8","merge":"0731009b9c14baeff5635a8975254341d45d894c","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"897ac5ae0c3232be6e2fb11dd81533fead592d693cf3e7b35416e88d01df2c88"},"test_exec_bep":{"unit":{"results":427,"remote_executed":427,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14187.305,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":92.254,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=78.46 load5=100.68 load15=60.45 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=49.65 load5=91.57 load15=58.70 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=8 max_load1=49.65 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 40.13 83.90 57.51 6/1031 55650"],"window":{"readings":8,"max_load1":"49.65","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38935}},"log_dir":"/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/criterion3/evidence/remote/floor-logs","load":{"threshold":15,"waited_seconds":0,"wait_timed_out":0,"run_start_load":6.91,"run_max_load":12.38,"run_mean_load":10.01,"run_readings":7,"wait_first_load":6.91,"wait_max_load":6.91,"wait_mean_load":6.91,"wait_readings":1,"read_errors":0},"remote_slot":{"released":true,"outcome":"released"}},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218622}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159728,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestCompactRealDoltCallWiring PASS (integration-packages-2c974992a3e988636c94.log)\n  TestCompactRealDoltDefaultWaitDelay PASS (integration-packages-17007f3ad20cb33b8d6a.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```

Full execution.json retained: 6084 lines; /var/tmp/gc-heavy-gate/runs/ga-2k6ia0vt.c3/evidence/execution.json; SHA256 de3e1c6fe4f35ac4ae8a5e097d498a68804e3b2098b28b39194bd95c400f04bb.

Full skip inventory retained: 4834 lines; /var/tmp/gc-heavy-gate/runs/ga-2k6ia0vt.c3/evidence/remote/skip-inventory.json; SHA256 3ac3f55e908e2313ac951ae9f2bfbd7a6778d4c6ddb03aa80efe1ec6854184b3.

Full floor-topup steps retained: 1074 lines; /var/tmp/gc-heavy-gate/runs/ga-2k6ia0vt.c3/evidence/remote/floor-topup.json; SHA256 4c3743c01edb1491be78359216e16c25d14021e116ac14f0fa882ea631e2c628.

Full by-name results retained: 8 lines; /var/tmp/gc-heavy-gate/runs/ga-2k6ia0vt.c3/evidence/remote/by-name.txt; SHA256 dba11678e118a905aa172627d7d0066421b927412881139cdeb9aa31b7f2ce0d.

## CI-required closure

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-validate.bDxU5c 6aee6c8c10465cc0a40c5f5b66ecf3a3e8bca004 8ce5c5e6c9bca47dec07b6533d02e6bd87d937d8 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; runner-policy.log. runner_16vcpu resolves to Ubuntu Linux. |
| changes | LOCAL-PASS | The closure tool exits 0; /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: credential_provider filter no match; if needs.changes.outputs.credential_provider == true. |
| pack-gate | NOT-TRIGGERED | Tool row: packs filter no match; if needs.changes.outputs.packs == true. |
| windows-lane | LOCAL-PASS | The workflow run commands below exit 0 through fresh-tree-run, load-gate-run and isolated-test-run, detached; /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/windows-lane-output.txt. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner_windows resolves to blacksmith-4vcpu-windows-2025. Separate build-closure analysis for this job: GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps over .github/windows-lane/test.txt; exit 0, 10 module directories, no changed non-test input in the closure. /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/windows-native-reach.json. |

Windows/Darwin compile job, actual run commands:

```sh
GOOS=windows go build $(cat .github/windows-lane/build.txt)
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
(
    cd tools/nogo
    GOOS=windows go build ./analyzers/...
    GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
GOOS=darwin go build ./...
GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.bDxU5c head=0731009b9c14 tree=8938dcbbe41f dir=/var/tmp/gc-fresh-tree.gdKVEu modified=0 untracked=0 ignored=2 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.39 run_max_load=14.77 run_mean_load=13.21 run_readings=3 wait_first_load=11.39 wait_max_load=11.39 wait_mean_load=11.39 wait_readings=1 read_errors=0
GATE_RUN_EXIT rc=0 state=complete

windows_reach: windows-lane-test=none (its own three-package test closure; the credential-provider-only helper was not substituted).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Gate PASS authorizes PR-open. Merge requires CI / required green on the PR head and the deferred native Windows job successful (or skipped only by its own false path filter).

Full CI-required closure retained: 17 lines; /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/closure.txt; SHA256 9adc93f2e27e9c71b745547fd258d5a1ce42af909f5c868b51f0a6556c3bb6db.

Full native Windows reach retained: 23 lines; /var/tmp/gc-gate-ga-2k6ia0vt.qBF0jR/ci-required/windows-native-reach.json; SHA256 0d8b9e78f67e9876a16628b842d2ca018c7ccd387ea268046336b5cc857383af.

prepush_suite: skipped
prepush_suite_c3_evidence: /var/tmp/gc-heavy-gate/runs/ga-2k6ia0vt.c3/evidence
