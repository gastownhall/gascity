# Native open: prompt ambient environment readers and cancellable open windows

**Verdict:** **PASS**

bead: ga-7x72j106; build: ga-zyfet7; review: ga-0z004zz6.
DEPLOY_MODE: remote; push_remote: fork; BASE_REF: origin/main.
reviewed_commit: dfdf1026e9376460a044f3de6591e0788cf9653e
pinned_base: 13bb101b8fb3bdcae9f7135ad344665af9e7bbd9
canonical_merge_head: 84e727da6d80c6b1456d279a5ee672a20f11ff42
canonical_merge_tree: 841cff8d9424d2c2cbdf1905e4962749b1105b81
merge_state: clean (origin/main@13bb101b8fb3bdcae9f7135ad344665af9e7bbd9)
gate_base_kind: current-tip
merge_validation: exact reviewed source plus pinned base independently materialized; later base changes are checked by the merge authority.
reviewer_tests_verified: b9afe110e14a6f1f4a53736972cf1210685b23e0 -> dfdf1026e9376460a044f3de6591e0788cf9653e files=internal/execenv/process_env_test.go (reviewer-added-tests.sh check).

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Reviews PASS | PASS. Closed review ga-0z004zz6 binds exact source; independently verified additive reviewer-only test extension. No HIGH/security finding. |
| 2 | Acceptance | PASS. All12 specified behavior groups map to actual fresh tests, including context cancellation, token give-back, panic/idempotent exit, ambient readers during every open path, journal rechecks, read-budget reconnect and runtime cancellation/env isolation. Every40 package/test pair PASS; see acceptance-final.json and owned-pairs-final.json. Separate fresh churn race PASS below. |
| 3 | Full tests and CI | PASS. Five standard fresh remote phases plus required native floor; all owned tests PASS, zero FAIL, every non-owned SKIP site/reach/reason answered with pinned-base proof. All6 required CI leaf accounts recorded below. |
| 4 | Open HIGH findings | PASS. No unresolved HIGH or security finding. Minor wording/style follow-up ga-astff4fk and pre-existing partial-restore quirk ga-72fz4r7v remain separate, nonblocking. |
| 5 | Clean final tree | PASS. Canonical head/tree re-resolve and match independent merge-tree; no tracked/untracked scratch delta. Gate record is the sole deployer addition to source. |
| 6 | Merge/build/vet | PASS. Clean exact canonical merge at pinned main; whole-tree build and nogo/vet end SUCCESS with normal validation active. |
| 7 | Single theme | PASS. One process-env projection/read isolation behavior across execenv, beads open paths and runtime consumers. No independent feature or internal .claude path. |

## Command and runtime evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/merge-zYe752 --base 13bb101b8fb3bdcae9f7135ad344665af9e7bbd9 --bead-json /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/bead.json --owned /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/owned.tsv --run-dir /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence
test_cmd_scope: full-suite
heavy_mode: none
test_exec_mode: remote-exec
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI job/matrix/timeout/required-list change)
test_counts: 154516 PASS,0 FAIL,606 SKIP (top-level result lines, repeated execution per job retained)
test_log_dir: /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence/remote/logs

GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence/remote/logs logs=557 logs_with_results=489
test_counts: PASS=154516 FAIL=0 SKIP=606
  (top-level result lines, column 0, summed over the 557 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+56 more)
diff_tests_executed:
  TestAmbientNativeDoltOpenEnvAnswersDuringNativeDoltOpenEnv PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-363a36f2662d541bc75e.log)
  TestAmbientReadersAnswerPromptlyDuringEveryOpenWindow PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-7f2c404ca64cd2da0395.log)
  TestBdStoreCommandRunsDuringAnOpenWindow PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-b398920224b769833cd5.log)
  TestBdStorePurgeRunsDuringNativeDoltOpenEnv PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-0b74cd4d9bd61207e1fd.log)
  TestNativeJournalRecheckResolvesTheAmbientEnvInAnOpenWindow PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-8c9c804909a3bb579837.log)
  TestOpenWindowIsReleasedAfterEveryWayOut PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-9a6a09bab414814cab53.log)
  TestOrdinaryNativeOpenWaitsForWholeBeadsNamespaceRestore PASS (integration-packages-ceb32a3c147a95796597.log, unit-3ee1ae2eaa26dfd63a88.log)
  TestProcessEnvSnapshotSeesAmbientDuringProxiedWindow PASS (integration-packages-a1f48694841e1859d61a.log, unit-0b74cd4d9bd61207e1fd.log)
  TestProcessEnvSnapshotShowsTheAmbientValueDuringNativeDoltOpenEnv PASS (integration-packages-8272c8197ebafcf5385f.log, unit-b398920224b769833cd5.log)
  TestProcessEnvSnapshotShowsTheWholeBeadsNamespaceDuringAWindow PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-ea4511196531bb4b8c55.log)
  TestProxiedEventsJournalResolutionAnswersFromTheAmbientEnvInAnOpenWindow PASS (integration-packages-86f8e1ae5bca4d4a5e22.log, unit-a93729df64a652fced43.log)
  TestProxiedNativeOpenWindowDoesNotBlockChildEnvSnapshots PASS (integration-packages-8272c8197ebafcf5385f.log, unit-27371dea3b130a4c231f.log)
  TestProxiedStoreEventsJournalRefusalOrder PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-b398920224b769833cd5.log)
  TestReadReconnectReturnsTheBudgetErrorWhileAnotherOpenHoldsTheWindow PASS (integration-packages-8d820cd4c52192de043e.log, unit-ea4511196531bb4b8c55.log)
  TestScopedNativeDoltOpenEnvIgnoresKeysOutsideItsList PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-9a6a09bab414814cab53.log)
  TestSecondOpenWindowIsBoundByItsOwnContext PASS (integration-packages-ceb32a3c147a95796597.log, unit-364160de6f2f808aa619.log)
  TestAWaitingWindowTakesItsSnapshotOnlyOnceItHoldsTheToken PASS (unit-15fb50316016cb1ea060.log)
  TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiredAsItWasTaken PASS (unit-15fb50316016cb1ea060.log)
  TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiresAfterTheTokenIsTaken PASS (unit-15fb50316016cb1ea060.log)
  TestEnterProjectionWindowReturnsTheContextErrorWhileAnotherWindowIsHeld PASS (unit-15fb50316016cb1ea060.log)
  TestEnterProjectionWindowWithAnExpiredContextTouchesNothing PASS (unit-15fb50316016cb1ea060.log)
  TestEnvironReturnsAFreshSliceInAndOutOfAWindow PASS (unit-15fb50316016cb1ea060.log)
  TestLookupEnvInAWindowKeepsWhatTheWindowWithheldAndAdded PASS (unit-15fb50316016cb1ea060.log)
  TestLookupSnapshot PASS (unit-15fb50316016cb1ea060.log)
  TestLookupSnapshotAgreesWithOSLookupEnv PASS (unit-15fb50316016cb1ea060.log)
  TestLookupSnapshotNamesFollowTheHostsCase PASS (unit-15fb50316016cb1ea060.log)
  TestProjectionExitIsIdempotent PASS (unit-15fb50316016cb1ea060.log)
  TestProjectionTokenIsReleasedWhenTheWindowBodyPanics PASS (unit-15fb50316016cb1ea060.log)
  TestProjectionWindowsAreNotReentrant PASS (unit-15fb50316016cb1ea060.log)
  TestReadersReturnPromptlyWithTheAmbientValueDuringAProjection PASS (unit-15fb50316016cb1ea060.log)
  TestSecondWindowRunsOnceTheFirstExits PASS (unit-15fb50316016cb1ea060.log)
  TestWindowChurnNeverLeaksAProjectionToReaders PASS (unit-15fb50316016cb1ea060.log)
  TestStartCanceledDuringTheParentEnvReadSpawnsNothing PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)
  TestStartDuringAProcessEnvProjectionLeavesTheProviderServing PASS (integration-packages-74db6ddfc6b7303645d2.log, integration-packages-cc430839a756656daa1e.log +2 more)
  TestCityOpRunsAtOnceDuringAProcessEnvProjection PASS (integration-packages-02ff57e430fd7218601d.log, unit-d1f6abca8430b335d6c5.log)
  TestDeadlineEnvIsUnaffectedByAProcessEnvProjection PASS (integration-packages-02ff57e430fd7218601d.log, unit-d1f6abca8430b335d6c5.log)
  TestRunSetupCommandRunsAtOnceDuringAProcessEnvProjection PASS (integration-packages-0ba2c1cd80ef2ec12177.log, integration-packages-15c091b746137e5c2921.log +2 more)
  TestStartDuringAProcessEnvProjectionDoesNotInheritIt PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)
GATE_TEST_EVIDENCE_RESULT ok: 38 diff-owned tests verified by name


The name-only reader deduplicates two names shared across distinct runtime packages. Independent manifest-label-qualified owned-pairs-final.json proves40 package/test pairs,38 unique names, every pair PASS with no owned SKIP/FAIL. Reviewer-added give-back and contended-snapshot regressions both actually run.

541 result actions: unit415, acceptance25, integration-packages86, full integration14, registered latency1. Independent raw BEP audit verifies all PASSED, successful terminal events and no abort. Every result remote, uncached locally/remotely, attempt1 and started inside its own phase window. Both latency budgets PASS after clear farm probe. Mandatory native floor has16 exit0 jobs: beadstest, Darwin compile, runner self-tests and every environment-absence root. owned_topup: [].

policy_lane: PASS (13 results; whole-tree C6 nogo/vet also PASS).
drift_lane: PASS (fresh-tree make bazel-sync, empty tracked/untracked generated delta,7 generated targets).
OpenAPI compatibility: PASS in a separate fresh tree,1 target.
C6/FAST build/test caches give no runtime-suite credit. The isolation wrapper reports PASS-THROUGH; no clean-environment claim is made from its banner. Remote environments are declared by Bazel. Native floor explicitly scrubs GC_/BD_/BEADS_/DOLT_ selectors and uses the already installed pinned bd. ICU74 host setup/provenance retained in icu74.txt; no live gc replacement or bd reinstall.

Compact execution record (large arrays elided only by official compactor; full bytes retained by hash):

{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":69.98249435424805,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13176.387,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":107.976,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=70.73 load5=78.23 load15=40.98 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=42.86 load5=70.76 load15=39.68 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 28.45 64.56 38.60 20/1211 54599"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40312}},"log_dir":"/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":502,"environment-absence":90},"top_level_counts_by_reason_class":{"other":244,"environment-absence":43},"tests":{"elided_array":{"items":592,"bytes":221568}}},"test_cmd":{"elided_array":{"items":5,"bytes":2139}},"test_counts":{"PASS":154516,"FAIL":0,"SKIP":606},"diff_tests_executed":"diff_tests_executed:\n  TestAmbientNativeDoltOpenEnvAnswersDuringNativeDoltOpenEnv PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-363a36f2662d541bc75e.log)\n  TestAmbientReadersAnswerPromptlyDuringEveryOpenWindow PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-7f2c404ca64cd2da0395.log)\n  TestBdStoreCommandRunsDuringAnOpenWindow PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-b398920224b769833cd5.log)\n  TestBdStorePurgeRunsDuringNativeDoltOpenEnv PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-0b74cd4d9bd61207e1fd.log)\n  TestNativeJournalRecheckResolvesTheAmbientEnvInAnOpenWindow PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-8c9c804909a3bb579837.log)\n  TestOpenWindowIsReleasedAfterEveryWayOut PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-9a6a09bab414814cab53.log)\n  TestOrdinaryNativeOpenWaitsForWholeBeadsNamespaceRestore PASS (integration-packages-ceb32a3c147a95796597.log, unit-3ee1ae2eaa26dfd63a88.log)\n  TestProcessEnvSnapshotSeesAmbientDuringProxiedWindow PASS (integration-packages-a1f48694841e1859d61a.log, unit-0b74cd4d9bd61207e1fd.log)\n  TestProcessEnvSnapshotShowsTheAmbientValueDuringNativeDoltOpenEnv PASS (integration-packages-8272c8197ebafcf5385f.log, unit-b398920224b769833cd5.log)\n  TestProcessEnvSnapshotShowsTheWholeBeadsNamespaceDuringAWindow PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-ea4511196531bb4b8c55.log)\n  TestProxiedEventsJournalResolutionAnswersFromTheAmbientEnvInAnOpenWindow PASS (integration-packages-86f8e1ae5bca4d4a5e22.log, unit-a93729df64a652fced43.log)\n  TestProxiedNativeOpenWindowDoesNotBlockChildEnvSnapshots PASS (integration-packages-8272c8197ebafcf5385f.log, unit-27371dea3b130a4c231f.log)\n  TestProxiedStoreEventsJournalRefusalOrder PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-b398920224b769833cd5.log)\n  TestReadReconnectReturnsTheBudgetErrorWhileAnotherOpenHoldsTheWindow PASS (integration-packages-8d820cd4c52192de043e.log, unit-ea4511196531bb4b8c55.log)\n  TestScopedNativeDoltOpenEnvIgnoresKeysOutsideItsList PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-9a6a09bab414814cab53.log)\n  TestSecondOpenWindowIsBoundByItsOwnContext PASS (integration-packages-ceb32a3c147a95796597.log, unit-364160de6f2f808aa619.log)\n  TestAWaitingWindowTakesItsSnapshotOnlyOnceItHoldsTheToken PASS (unit-15fb50316016cb1ea060.log)\n  TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiredAsItWasTaken PASS (unit-15fb50316016cb1ea060.log)\n  TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiresAfterTheTokenIsTaken PASS (unit-15fb50316016cb1ea060.log)\n  TestEnterProjectionWindowReturnsTheContextErrorWhileAnotherWindowIsHeld PASS (unit-15fb50316016cb1ea060.log)\n  TestEnterProjectionWindowWithAnExpiredContextTouchesNothing PASS (unit-15fb50316016cb1ea060.log)\n  TestEnvironReturnsAFreshSliceInAndOutOfAWindow PASS (unit-15fb50316016cb1ea060.log)\n  TestLookupEnvInAWindowKeepsWhatTheWindowWithheldAndAdded PASS (unit-15fb50316016cb1ea060.log)\n  TestLookupSnapshot PASS (unit-15fb50316016cb1ea060.log)\n  TestLookupSnapshotAgreesWithOSLookupEnv PASS (unit-15fb50316016cb1ea060.log)\n  TestLookupSnapshotNamesFollowTheHostsCase PASS (unit-15fb50316016cb1ea060.log)\n  TestProjectionExitIsIdempotent PASS (unit-15fb50316016cb1ea060.log)\n  TestProjectionTokenIsReleasedWhenTheWindowBodyPanics PASS (unit-15fb50316016cb1ea060.log)\n  TestProjectionWindowsAreNotReentrant PASS (unit-15fb50316016cb1ea060.log)\n  TestReadersReturnPromptlyWithTheAmbientValueDuringAProjection PASS (unit-15fb50316016cb1ea060.log)\n  TestSecondWindowRunsOnceTheFirstExits PASS (unit-15fb50316016cb1ea060.log)\n  TestWindowChurnNeverLeaksAProjectionToReaders PASS (unit-15fb50316016cb1ea060.log)\n  TestStartCanceledDuringTheParentEnvReadSpawnsNothing PASS (integration-packages-cc430839a756656daa1e.log, unit-b286b5fdbb07f0525a4e.log)\n  TestStartDuringAProcessEnvProjectionLeavesTheProviderServing PASS (integration-packages-74db6ddfc6b7303645d2.log, integration-packages-cc430839a756656daa1e.log +2 more)\n  TestCityOpRunsAtOnceDuringAProcessEnvProjection PASS (integration-packages-02ff57e430fd7218601d.log, unit-d1f6abca8430b335d6c5.log)\n  TestDeadlineEnvIsUnaffectedByAProcessEnvProjection PASS (integration-packages-02ff57e430fd7218601d.log, unit-d1f6abca8430b335d6c5.log)\n  TestRunSetupCommandRunsAtOnceDuringAProcessEnvProjection PASS (integration-packages-0ba2c1cd80ef2ec12177.log, integration-packages-15c091b746137e5c2921.log +2 more)\n  TestStartDuringAProcessEnvProjectionDoesNotInheritIt PASS (integration-packages-74db6ddfc6b7303645d2.log, unit-101fea1c9a18bf0def46.log)\nGATE_TEST_EVIDENCE_RESULT ok: 38 diff-owned tests verified by name\n"}

Full execution record retained: 5944 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/criterion3-run/evidence/execution.json; SHA256 d3f1f56937822b8d9ab1a99200fbff6c2a57450c64494fc7f3d7a0d505b60e9e.
Full package-qualified owned results retained: 632 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/owned-pairs-final.json; SHA256 db9c433af13a75027c2b9f09ae198ee67ca69affdb294c1211d8a29bd6b0bdb6.
Full raw phase audit retained: 111 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/remote-phase-audit.json; SHA256 a4a025b608a2e29848d86a97e2e220010d54e3391ce6ed5ac1df3528da91d373.

## Supplementary race method and result

method_ruling: ga-6jj7nysx, relayed by mayor gcg-8443905; ruling SHA256 63e2f03ef21b85a39c125f6a6f8bcf7649625ae4ec9860a9c02f8817abe4fd7f.
Exact fresh inner command: gate-rbe-phase.sh owned-churn-race <fresh-evidence-dir> --@rules_go//go/config:race --norun_validations --test_sharding_strategy=disabled '--test_filter=^TestWindowChurnNeverLeaksAProjectionToReaders$' //internal/execenv:execenv_test

Result: target rc0; TestWindowChurnNeverLeaksAProjectionToReaders PASS,1 PASS/0 FAIL/0 SKIP, no DATA RACE. Independent predicate: one remote uncached attempt1 result, valid phase window, successful terminal BEP. Separate admitted/isolation-wrapper fresh tree; focused supplementary acceptance only, never full-suite credit. The sole method correction is --norun_validations on this supplementary race invocation. Normal C6/nogo/policy/default-suite validations all independently PASS. No omission/inapplicability claim, no waiver, no fix-carrying exemption, no self-stamp of ga-zyfet7. The prior failed race target/body is not credited.

Full fresh race result retained: 28 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/churn-race/result.json; SHA256 dc2fb43262ca85b4e8aae76bbd059b90589dd81c22746a32e93cd356e7d68637.
Full fresh race by-name evidence retained: 6 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/churn-race/by-name.txt; SHA256 83351d9271e75da06db6e215d5156410a14c3702f4e7bab36f03f025c3fd313a.

## Acceptance and unchanged boundaries

Code review confirms clean snapshot before odd sequence, context-aware capacity-one acquisition and post-acquire ctx check, even sequence before token release, idempotent exit, fresh Environ slices and os.LookupEnv-compatible lookup. All three production native-open paths pass their own ctx and restore before exit; ctx-less test helpers are test-only. Journal rechecks resolve ambient values and advance checkedAt. Runtime/session production diff ignoring comment-only changes is empty; providers keep the same Environ signature and read seams. Closing symbol grep has no matches.

Settled R2 behavior: non-window os.Setenv/Unsetenv during an open window is invisible to ambient readers until it closes. This is stated in the public PR. Storage lease/CloseStore changes are a separate feature. Public reference is Refs #7170; not Closes.

The fresh integration-packages run actually PASSes TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore against its real Dolt server/native store in36.93s. TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors still SKIPs at its unchanged direct upstream Open guard because its external server endpoint is unconfigured (127.0.0.1:0). It is not owned, never credited PASS; the identical guard/reason is measured on pinned base. No review-era result is reused.

## SKIP evidence

skip_inventory: {"environment-absence": 90, "other": 502} farm rows by reason class.
Final raw farm/native inventory: 664 rows including subtests,72 native,309 unique sites. All sites line_in_hunk=no and UNCHANGED; zero unresolved/diff-owned skipped result. Full site/source-line/reason/log rows retained by pointers below. Reasons are unchanged platform/capability, explicit provider/live-fixture and intentional conformance guards; none is a PASS.

Correct-tag acceptance_a closure and all14 observed acceptance SKIPs are additionally proven in acceptance-skip-comparison. The initial discovery helper queried acceptance instead; this corrected closure/proof overrides that initial acceptance reach classification. The head suite always used the correct acceptance_a configuration. All SHARED/HELPER-HUNKS in eight package directories (cmd/gc, beads, execenv, runtime/acp, exec, herdr, subprocess, tmux) are answered by actual go list -test -deps closure and same-target/same-tags pinned-base comparison. Every reached observed farm SKIP and every native SKIP is measured; no import reach is inferred from file suffix. Per-hunk entries remain in owned.explain.txt. Ambiguous conformance.go sites resolve from their FencedInput subtest names and exact fencedStart caller lines (T.Helper), retained explicitly.

Each reached SHARED/HELPER-HUNKS block is named below. No TestMain or init changed. The shared test changes are comments/imports or projection-window fixtures/assertions, not the unchanged platform/provider/capability SKIP guards. The skipped direct-upstream beads Open test calls the external library directly, outside the changed native-open entry helpers; runtime conformance skips report their unchanged capability/platform guards in runtimetest, and other rows report unchanged explicit environment/provider preconditions. Test-only projection helpers do not enter an importing package's test binary. Every observed reached skipped case was also actually measured at pinned base, under the same target/tags or exact native command, with identical SKIP reason; all comparison groups, including every corrected-tag acceptance_a profile return ALL PRE-EXISTING and zero new/reason-changed/owned/unjudged rows. Production reach is computed with full go list -test -deps, not assumed absent.

| Hunk entry | Count | Shared surface and SKIP answer |
|---|---|---|
| SHARED cmd/gc/bd_env_test.go | 2 | File-level comments/imports only; no executable helper changed. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED cmd/gc/beads_proxied_native_test.go | 1 | File-level comments/imports only; no executable helper changed. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/beads/native_dolt_events_journal_read_test.go | 12 | Projection fixture/assertion helpers: holdOpenWindow. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/beads/native_dolt_open_env_window_test.go | 1 | Projection fixture/assertion helpers: assertWindowInvisible. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/beads/native_dolt_projection_window_test.go | 1 | Projection fixture/assertion helpers: healthyOpenSpy, holdProjectionWindow, installOpenSeam, nativeOpenEntries, parkFirstOpen, within. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/beads/native_dolt_proxied_open_test.go | 3 | File-level comments/imports only; no executable helper changed. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/beads/native_dolt_store_test.go | 1 | File-level comments/imports only; no executable helper changed. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/execenv/process_env_test.go | 5 | Projection fixture/assertion helpers: assertGateIdle, countEntries, enterWindow, probeEntries, waitForWaitingWindows, windowsWaiting, within. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/acp/process_env_projection_test.go | 8 | Projection fixture/assertion helpers: holdProcessEnvProjection, probeValues, startReadingTheParentEnv. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/exec/deadline_env_test.go | 7 | Projection fixture/assertion helpers: assertDeadlineIgnoresProjection. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/exec/process_env_projection_test.go | 2 | Projection fixture/assertion helpers: holdProcessEnvProjection. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/herdr/process_env_setup_test.go | 2 | Projection fixture/assertion helpers: holdProcessEnvProjection. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/subprocess/process_env_projection_test.go | 7 | Projection fixture/assertion helpers: holdProcessEnvProjection. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |
| SHARED internal/runtime/tmux/process_env_setup_test.go | 2 | Projection fixture/assertion helpers: holdProcessEnvProjection. Unchanged SKIP guard/reason independently matches pinned base in the recorded package/importer and native groups. |

shared-skip-comparison integration-full //test/integration:integration_test: COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/runtime/ssh:ssh_test: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/beads/exec:exec_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/runtime/herdr:herdr_test: COMPARE-SUMMARY head_skips=6 judged=6 pre-existing=6 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/runtime/subprocess:subprocess_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/beads:beads_test: COMPARE-SUMMARY head_skips=17 judged=17 pre-existing=17 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //examples/gastown:gastown_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/runtime/exec:exec_test: COMPARE-SUMMARY head_skips=12 judged=12 pre-existing=12 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //test/containerhost:containerhost_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/session:session_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //internal/runtime/tmux:tmux_test: COMPARE-SUMMARY head_skips=13 judged=13 pre-existing=13 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=66 judged=66 pre-existing=66 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison integration-packages //test/conversationlane:conversationlane_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/runtimetest:runtimetest_test: COMPARE-SUMMARY head_skips=39 judged=39 pre-existing=39 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/convergence:convergence_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/orders:orders_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runproj:runproj_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/beads/beadstest:beadstest_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/connect/tokenrender:tokenrender_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/beads:beads_test: COMPARE-SUMMARY head_skips=18 judged=18 pre-existing=18 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/molecule:molecule_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/worker/workertest:workertest_test: COMPARE-SUMMARY head_skips=108 judged=108 pre-existing=108 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/config:config_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/beads/splittest:splittest_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/subprocess:subprocess_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime:runtime_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/formula:formula_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/herdr:herdr_test: COMPARE-SUMMARY head_skips=6 judged=6 pre-existing=6 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //test/acceptance/helpers:helpers_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/api:api_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/doctor:doctor_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/workspacesvc:workspacesvc_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/t3bridge:t3bridge_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //examples/gastown:gastown_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/dispatch:dispatch_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/tmux:tmux_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/session:session_test: COMPARE-SUMMARY head_skips=3 judged=3 pre-existing=3 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //internal/runtime/exec:exec_test: COMPARE-SUMMARY head_skips=12 judged=12 pre-existing=12 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //cmd/gc:gc_sim_exec_test: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
shared-skip-comparison unit //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=187 judged=187 pre-existing=187 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native cmd/gc: COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native internal/beads: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native internal/beads/exec: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native internal/runtime/herdr: COMPARE-SUMMARY head_skips=6 judged=6 pre-existing=6 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native internal/runtime/runtimetest: COMPARE-SUMMARY head_skips=21 judged=21 pre-existing=21 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-skip-comparison native internal/runtime/tmux: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
acceptance-skip-comparison acceptance //test/acceptance:acceptance_test: COMPARE-SUMMARY head_skips=11 judged=11 pre-existing=11 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
acceptance-skip-comparison acceptance //test/acceptance:beads_topology_matrix_doctor_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
acceptance-skip-comparison acceptance //test/acceptance:beads_topology_matrix_native_lane_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
acceptance-skip-comparison acceptance //test/acceptance:beads_topology_matrix_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

Full full skip site/reason inventory retained: 6642 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/skip-rows-final.json; SHA256 3672b7ac828f6df7edb8f138af9e2bd6b177cecfa08bc0e40dab0202de5f5b2d.
Full full skip site proof retained: 312 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/skip-site-proof-final.txt; SHA256 dbc7f4a91beddbd5cc5d19053e24194cd16c4936df32f963778f886ae5f19dc3.
Full shared hunks and ownership retained: 550 lines; /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/owned.explain.txt; SHA256 ffc2b9cecf310651a662af1822dd6100005c350af86cd6697cbea18210e46ed0.

## Load/admission observations

Ordinary native commands retain local-heavy admission40GiB and threshold15/maxwait1800 bounded soft load waits. Remote phases use admission25GiB/cap2, no host heavy/load slot. Wait observations and actual run observations are recorded separately, including any timeout:

native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=450 wait_timed_out=0 run_start_load=14.79 run_max_load=15.78 run_mean_load=15.14 run_readings=6 wait_first_load=37.83 wait_max_load=37.83 wait_mean_load=24.19 wait_readings=16 read_errors=0
native-floor normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "450", "run_max_load": "15.78", "run_mean_load": "15.14", "run_readings": "6", "run_start_load": "14.79"}
native-skip-base: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.79 run_max_load=20.41 run_mean_load=18.24 run_readings=4 wait_first_load=13.79 wait_max_load=13.79 wait_mean_load=13.79 wait_readings=2 read_errors=0 prewait_s=0
native-skip-base normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "0", "run_max_load": "20.41", "run_mean_load": "18.24", "run_readings": "4", "run_start_load": "13.79"}
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.24 run_max_load=20.77 run_mean_load=19.12 run_readings=4 wait_first_load=14.24 wait_max_load=14.24 wait_mean_load=14.24 wait_readings=2 read_errors=0 prewait_s=0
windows-lane normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "0", "run_max_load": "20.77", "run_mean_load": "19.12", "run_readings": "4", "run_start_load": "14.24"}
pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=270 wait_timed_out=0 run_start_load=14.59 run_max_load=13.95 run_mean_load=13.71 run_readings=3 wait_first_load=20.31 wait_max_load=20.31 wait_mean_load=16.73 wait_readings=11 read_errors=0 prewait_s=270
pack-gate normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "270", "run_max_load": "13.95", "run_mean_load": "13.71", "run_readings": "3", "run_start_load": "14.59"}

## Required CI leaf accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/merge-zYe752 13bb101b8fb3bdcae9f7135ad344665af9e7bbd9 dfdf1026e9376460a044f3de6591e0788cf9653e fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
leaf_count: 6 LEAF jobs need a status; check is an aggregator.

ci_required_accounting:
runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit0; log=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/runner-policy.log
changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/merge-zYe752 13bb101b8fb3bdcae9f7135ad344665af9e7bbd9 dfdf1026e9376460a044f3de6591e0788cf9653e fork; exit0; log=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/closure.txt
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"reach": "none", "reach_stderr": "/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/credential-windows-reach.stderr", "reach_stdout": "/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/credential-windows-reach.stdout", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI", "tool_row": "  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"}
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"reach": "none", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI"}
windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/merge-zYe752 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/windows-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-windows-lane.sh; exit=0; log=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/windows-lane-output.log
pack-gate | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/merge-zYe752 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/pack-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-pack-gate.sh; exit=0; log=/var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73/ci-required-v2/pack-gate-output.log

windows_reach: {"credential-provider-windows": "none", "packages": ["./internal/credentialprovider", "./internal/genshell/..."], "windows-lane-test": "none"}
deferred_to_pr_ci: windows-lane-test (non-linux-runner), credential-provider-windows (non-linux-runner)
not_covered: none

Exact merged-workflow Windows/Darwin cross-build and live pack jobs finish in separate admitted/load-gated fresh trees. Native Windows jobs require their own runner; both independent Windows build closures have empty diff reach. CI / required and deferred jobs must be green on the current head before merge. Gate PASS authorizes PR-open; merge belongs to MPR.

Source carries an older worker-env pin than main; gate runtime uses the canonical merge's current pin. Publication keeps source/pin unchanged and uses the hooks' documented local-cache mode; actual publication checks are recorded separately and never count toward remote criterion3.

Prior root /var/tmp/gc-gate-ga-7x72j106-wotcxg07 retains the stopped partial suite and failed race target without any PASS credit. Current root /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73 is a fresh complete gate under the recorded ruling.

Evidence root: /var/tmp/gc-gate-ga-7x72j106-ruling-43vufl73
UTC: 2026-11-14T09:50:40.978211+00:00
