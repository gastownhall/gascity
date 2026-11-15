# Release gate: durable self-handoff with a positive legacy guard

**Verdict:** **PASS**

Gate bead: ga-10l3akq2; build ga-4wvx2jl0; independent review ga-1pkh1tml (single-pass policy).
DEPLOY_MODE: remote; isolated destination deploy/ga-10l3akq2-gate; push remote fork. Source branches are provenance only.
Reviewed source: ee1a9c94c74e0445fd8cdd8108b37318120c8bcd. Review chain c614e0047fa88302d97aaa7578fddcd575da709a -> 42d89489f2426b329afa4504b5982e21063c991c -> source; exact patch-id 33697fddbc4033e70be6edd34693ad00e050334e, then mechanical-conflict patch-id b51807ccac781166aba57494fe957bf0e459aca8 (only allowlisted generated artifacts). Both recomputed independently.
Gate base: origin/main@a1eb65702f57f3f994e9ea9fa1af007436d0901b; gate_base_kind: current-tip.
Canonical merge: 0ebac4cc35356db0c33401ac6f185090acebacd5; tree e3445936dbbd2ae0b864b7d3d516f1b13a15a7f8 (identical product tree to source).
merge_state: clean (origin/main@a1eb65702f57f3f994e9ea9fa1af007436d0901b). merge_validation: the clean merge result was built and vetted; MPR must recheck the eventual merge result/current head and CI.
Preflight: source not landed; no existing source PR. Replacement for #9182; mayor handles its supersession. One additional gate round authorized in mayor mail gcg-9217025; round-cap proceeded at round 3.
Evidence root: /var/tmp/gc-gate-ga-10l3akq2-14s46bfe. Retain all logs and artifacts. No prior runtime result credited. docs/PROJECT_MANIFEST.md is absent; the active deployer protocol supplies the seven criteria and the source TESTING.md/workflow supply commands.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review | PASS: reviewer PASS and both verified carryovers above; final-review-carryover.txt. |
| 2 | Acceptance | PASS: A1–A7 mapped below; all 40 owned package/test pairs executed without SKIP or FAIL. |
| 3 | Tests | PASS: full five-phase remote suite, mandatory native floor, skip comparisons and complete required-CI leaf accounting below. |
| 4 | Review findings | PASS: no unresolved HIGH finding; reviewer reports no security findings. Accepted nonblocking limits remain disclosed below. |
| 5 | Clean branch | PASS: source/canonical/assigned trees clean; the sole deploy addition is this record. Publication requires clean post-commit status, exact source parent and ordinary commit/push guards. |
| 6 | Merge/build/vet | PASS: clean canonical merge; bazel build //... and make vet/nogo exit 0 with successful BEPs; normal materialization hooks ran. c6-independent-audit.json. |
| 7 | One theme | PASS: stage, persist, hide and release the same self-handoff brief, plus its positive legacy guard, typed state/events and generated projections. All accepted ancestry beads belong to this feature; no .claude paths or unrelated source commits. |

Acceptance:

- A1: PASS — qualifying caller short-circuits eligibility; controller ok flag precedes config; missing/error mode never stages in city path; six truth/probe rows PASS.
- A2: PASS — ineligible and unset context deliver immediate ordinary mail; two zero-probe rows PASS.
- A3: PASS — production handoff born-split two incarnations and explicit legacy staging PASS; all lifecycle/visibility/supersession/separate-store/exactly-once roots independently covered in 40 owned pairs.
- A4: PASS — review ga-1pkh1tml retains RED/control evidence and nine named compiling mutant kills; fresh gate independently re-verifies preserved tests, not historical runtime results.
- A5: PASS — complete five-phase remote suite plus native floor, default nogo, shell guards and generated sync PASS.
- A6: PASS — dial500ms/write500ms/read2s, ping fallback same bounds; qualifying production legacy controller/offline supplemental measurements PASS; worker-site table no probes; live v2 boot not claimed.
- A7: PASS — zero growth at pinned base, complexity clause (ii); unchanged baseline.

Test evidence:
test_cmd: official gate-criterion3.sh --repo /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge --base a1eb65702f57f3f994e9ea9fa1af007436d0901b --bead-json /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/bead.json --owned /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/owned.tsv --run-dir /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/criterion3-run/evidence.
test_cmd_scope: full-suite. Unit //..., acceptance tier, integration-tagged packages, full integration excluding latency names, then only the declared latency names. Each phase forces --nocache_test_results and --noremote_local_fallback. Native floor is additional required coverage, never a stitched local replacement.
test_exec_mode: remote-exec (derived independently from BEP). test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}.
test_counts: 541 fresh remote test actions PASS; raw Go result rows 154829 PASS / 0 FAIL / 592 SKIP. Native floor 16 steps exit 0; raw rows 260 PASS / 0 FAIL / 72 SKIP. Raw counts include subtests and repeated phases, not unique tests.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}; fallback_reason: none; admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 72.1284065246582, "waited_seconds": 0.001}; farm_test_seconds: 13910.458.
latency_leg: measured; measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS. Dedicated lane under a clear farm probe; main integration excludes those names.
heavy_mode: none; ICU74 verdict OK. Runtime probe verified rootless podman, paired Ryuk opt-out, pinned cached Dolt and bd before suite. Isolation wrapper is PASS-THROUGH for this scratch; remote tests use the farm environment, native commands use the documented scrubbed floor environment. No claim of an extra namespace isolation.
owned_topup: []; floor_topup: complete, 16 steps, every step exit 0. Includes beadstest, Darwin compile, runner self-tests and every environment-absence SKIP by package/root.
test_log_dir: /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/criterion3-run/evidence/remote/logs; native logs /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/criterion3-run/evidence/remote/floor-logs. BEP hashes/windows and canonical no-cache/no-fallback options: farm-independent-audit.json; all retained artifact digest/size and flat-log bytes verified in artifact-digest-audit.json.
diff_tests_executed: 40/40 PASS, package-qualified; 0 owned SKIP/FAIL/MISSING. Full roots:

- cmd/gc/TestBornSplitSuccessorCanHandoffWithoutAStagedBriefWedge PASS
- cmd/gc/TestControllerLossLeavesSelfHandoffProvisional PASS
- cmd/gc/TestCreateHandoffMailRoutesThroughProviderSeam PASS
- cmd/gc/TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession PASS
- cmd/gc/TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder PASS
- cmd/gc/TestDoPrimeWithHook_JSONModeDoesNotArchiveAutoHandoff PASS
- cmd/gc/TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen PASS
- cmd/gc/TestEligibleSelfHandoffProbesControllerExactlyOnce PASS
- cmd/gc/TestExplicitRestartCommandsPokeControllerWithoutPeriodicTick PASS
- cmd/gc/TestFailedSelfHandoffRemainsProvisionalAndAuditable PASS
- cmd/gc/TestHandoffStagedArmLandsInTheBindingOnAMigratedCity PASS
- cmd/gc/TestIneligibleSelfHandoffDoesNotProbeReconciler PASS
- cmd/gc/TestLegacyPinnedCityKeepsSelfHandoffStaged PASS
- cmd/gc/TestLiveSuccessorReleasesStagedHandoffExactlyOnce PASS
- cmd/gc/TestNonRestartingHandoffModesRemainOrdinaryMail PASS
- cmd/gc/TestOriginatingIncarnationCannotReleaseStagedHandoff PASS
- cmd/gc/TestReconcileReleasesStagedHandoffAcrossDistinctStores PASS
- cmd/gc/TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore PASS
- cmd/gc/TestRestageAfterStagedTimeoutRestartsReleaseLifecycle PASS
- cmd/gc/TestRestageDefersToControllerWhenEarlierBriefIsReleasable PASS
- cmd/gc/TestRestageIgnoresDanglingStagedPointer PASS
- cmd/gc/TestRestageLeavesResolvedPriorBriefAlone PASS
- cmd/gc/TestRestageRoutesEachClassToItsOwnStore PASS
- cmd/gc/TestRestageSupersedesPendingStagedBrief PASS
- cmd/gc/TestRestageSupersedesWrittenOffBriefEvenAfterTokenChange PASS
- cmd/gc/TestRestartableSelfHandoffStagesBeforeRestartRequest PASS
- cmd/gc/TestRetryAfterFailedRestartSupersedesProvisionalBrief PASS
- cmd/gc/TestRowFirstRestartDurableWriteFailedPokeExitsZero PASS
- cmd/gc/TestSelfHandoffRestartNotRecordedWording PASS
- cmd/gc/TestSelfHandoffStagesOnlyForPositivelyLegacyReconciler PASS
- cmd/gc/TestSelfHandoffWithUnsetModeContextDeliversImmediately PASS
- cmd/gc/TestSessionReconcilePhaseTraceUsesDistinctSites PASS
- cmd/gc/TestSessionStartAutoHandoffUsesProvidedStoreWithoutOpeningCity PASS
- cmd/gc/TestStagedHandoffTimeoutNeverReleasesToOriginatingIncarnation PASS
- cmd/gc/TestWorkerSiteHandoffRecordsTheRestart PASS
- cmd/gc/TestWorkerSiteSelfHandoffStagesOnlyForLegacySiteConfig PASS
- internal/events/TestHandoffLifecycleEventsAreKnownAndTyped PASS
- internal/mail/beadmail/TestBulkArchiveLeavesAStagedHandoffOpen PASS
- internal/mail/beadmail/TestExistingOrdinaryAutoHandoffRemainsVisible PASS
- internal/mail/beadmail/TestStagedHandoffIsInvisibleAcrossEveryMailReadSurface PASS

skip_inventory: 664 raw rows, 72 native, 309 unique sites, 0 unresolved. All sites UNCHANGED/line_in_hunk=no; file ownership explain is retained. Full reasons/source sites/package/log mapping: skip-rows-final.json, skip-reasons-final.json, skip-site-proof-final.txt.
skip_justification: non-owned platform, optional-tool/credential/fixture, intentionally gated mode and environment-absence cases. Every environment-absence farm SKIP received the mandatory native package top-up. All 32 reachable shared/helper-hunk farm groups and 6 native SKIP groups were rerun by name at the pinned base with identical reasons (official --compare-skips); no SKIP was credited as a PASS. Reached SHARED/HELPER entries and complete test-binary import closures: shared-skip-comparison/closures.json; comparisons retained alongside their fresh BEPs/logs. Unreached sites retain the printed static skip reason and unchanged source proof.
waiver_ref: none (no gascity waiver path). failure_attribution: none. ci_lane_run: n/a (no changed CI workflow/job/matrix/timeout/required-check config).
policy_lane: PASS, make lint/nogo and shell release-dist/routed/split/residency/hook guards plus 13 Bazel policy targets. drift_lane: PASS, make bazel-sync then empty tracked/untracked status plus 7 generated-artifact tests; OpenAPI compatibility 1 target PASS. All fast checks ran first in separate fresh views through pinned gate-base.sh.
complexity: clause (ii) zero growth; literal complexity-check base exit 2, merge exit 2; 238 rows compared, no merge row worse; complexity-diff no threshold changes; baseline untouched. This is the prescribed comparison, not an ignored failed gate.

Native load observations (remote admission has no local load wait):

- criterion3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=60.50 run_max_load=95.08 run_mean_load=85.19 run_readings=12 wait_first_load=34.49 wait_max_load=82.91 wait_mean_load=57.46 wait_readings=61 read_errors=0
- native-skip-comparison/run.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=990 wait_timed_out=0 run_start_load=14.52 run_max_load=19.62 run_mean_load=16.69 run_readings=3 wait_first_load=90.18 wait_max_load=90.18 wait_mean_load=45.29 wait_readings=35 read_errors=0 prewait_s=990

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge a1eb65702f57f3f994e9ea9fa1af007436d0901b ee1a9c94c74e0445fd8cdd8108b37318120c8bcd fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs..
ci_required_accounting:

- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/runner-policy-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-runner-policy.sh; exit 0; log /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/runner-policy.log
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=990 wait_timed_out=0 run_start_load=14.93 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=90.96 wait_max_load=90.96 wait_mean_load=46.01 wait_readings=35 read_errors=0 prewait_s=990
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge a1eb65702f57f3f994e9ea9fa1af007436d0901b ee1a9c94c74e0445fd8cdd8108b37318120c8bcd fork; exit 0; log /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/triggers.txt
- credential-provider-windows | DEFERRED-TO-PR-CI | reason=non-linux-runner;   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/pack-gate-corrected-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-pack-gate.sh; exit 0; log /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/pack-gate-corrected.log
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.44 run_max_load=13.97 run_mean_load=13.43 run_readings=3 wait_first_load=18.01 wait_max_load=18.01 wait_mean_load=16.28 wait_readings=7 read_errors=0 prewait_s=150
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/windows-lane-corrected-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-windows-lane.sh; exit 0; log /var/tmp/gc-gate-ga-10l3akq2-14s46bfe/ci-required/windows-lane-corrected.log
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.61 run_max_load=14.89 run_mean_load=14.07 run_readings=3 wait_first_load=12.61 wait_max_load=12.61 wait_mean_load=12.61 wait_readings=2 read_errors=0 prewait_s=0
- windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner;   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
windows_reach: credential-provider-windows=none; Windows native package list/test-binary import closure=none (separate computation).
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner).
not_covered: none.
PR-open PASS only. Merge requires CI / required green on the exact current PR head and every deferred job successful or legitimately path-filter skipped.

Supplemental acceptance and limits: qualifying handoff measured 17.556 ms offline and 15.874 ms with production legacy controller/real identity socket, fake runtime and desired builder paused. Both briefs actually staged; remote fresh supplemental test only. Live v2 boot is not claimed. Local dashboard build/preview served app HTML and entry assets HTTP200; every byte hash matched canonical generated files; own preview process group exited.
- No live v2 city boot proof; guard truth table and born-split offline regression are executed.
- Legacy-to-v2 flip after staging and older-controller skew retain the documented outstanding-brief risk; v2 release arm remains separate.
- Review nonblocking N1 extra probe/diagnostic on replay or refusal and N3 worker-site refused-latch corner remain accepted by the independent review.

Setup correction: temporary pack-gate launcher exit127 referenced a nonexistent evidence-root path before either workflow command ran. The original script/log/accounting are retained; the corrected launcher executes both exact workflow commands in a new fresh view. This is not a product test attribution, rerun of failed tests, or waiver. Runner-policy and full runtime results were preserved; see ci-required/accounting-launcher-setup-failure.json and ci-required/pack-gate-setup-failure.log.

Size requested by mayor: source raw 22915 patch lines / 1013675 bytes; excluding the 14 MPR-declared generated paths 3411 patch lines / 155114 bytes. Measurement only: MPR requires exact-head generated proof before exclusion; diff-size.json records commands/policy paths.

Evidence digests (SHA256):

- context.json: 46d6e0910ad72679468ddf69169e93b7ef3ac99813b7385d00d44cc7c767a470
- final-evidence-audit.json: 079518e71483e8066c75378dd52cc42b5ebe653aba03f76b5dca7899a55f4e2f
- criterion3-run/evidence/execution.json: 342a50becf9384369ccf2b1c3561a6f69af42eed8339dde3b3d91f04556ee644
- c6-independent-audit.json: 187043679c501b7c25857cc4202b9dcd3f0248b2cef4997fcf1c4e56d63b4265
- fast-independent-audit.json: 07db8ccb90469f76d2c7d4277cd01ab24d5eb6a388b7c467609faa416fd5dfc2
- complexity-zero-growth.json: f4e2b00bb14960158f3ed879ad655816f2ebce4b4cbebe434e0ab0c82f9177af
- farm-independent-audit.json: 42327808e0a6db9474974604eab442ed322ffc038ee9f1756613efc59a716d8f
- artifact-digest-audit.json: cab4166ed0f15f3c54ab2622b6d78bb55031bf72d7b0550ac2425c9e03d30c40
- owned-pairs-final.json: 44f2d9b3f4e5ddbf3a02ea32e8937fc312f8d4f7a78bf99dc664e533549091aa
- acceptance-final-audit.json: 80b27a414d193e2dfbcd73c905acf64eeceb59ecfd95cc904c674b9a03a18864
- skip-final-audit.json: 3db06b34c1cc6f1582a15a0c2d92eb59f52339a17c4908d65b9cf514d2d3554b
- skip-site-proof-final.txt: f231f461d104fe8a826fc70dcedd8816675a4e65a16342676d7ac284c7991e8a
- shared-skip-comparison/comparison.json: 4d9701d69986bf92abbbecf915105daf167439648e39d32f895a09084517c10e
- native-skip-comparison/comparison.json: 26a64a9865b2b7373ced1271010db62f94fb9be8a75f000577b2c3b30606e183
- ci-required/accounting.json: 4ca6fb0727a450e7e0b58798afdad6b78f8666089c2692795cac85ecda4bba80
- ci-independent-audit.json: 7856ddc066b9fd79b0638303952cc38d1232cd49728b077bae3955a7a8a7857b
- latency-probe-verified.json: 5f20fddca24bb6ae634fcdc60b297a136e36ceea4534df807f8d5066455edc5e
- dashboard-preview-verified.json: 32418d0e82577030765fe513c92d40d975b2ae42eb3cef693835ae982e2a0650
