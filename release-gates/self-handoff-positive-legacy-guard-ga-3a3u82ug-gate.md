# Release gate: durable self-handoff with positive legacy guard

**Verdict:** **PASS**

Gate scope: opening a PR from the reviewed source plus this record. Merge authority must verify current-head CI and settle the recorded conflicts before landing.

Bead: ga-3a3u82ug. Source: `42d89489f2426b329afa4504b5982e21063c991c`. Reviewed predecessor: `c614e0047fa88302d97aaa7578fddcd575da709a`.
Canonical scratch commit: `0daf650072dcea210140b0dfab310e24854107f3`; tree: `58f039af11eacdb5385724486823895e5f9426d8`.
gate_base_sha: 420b7bb65fff819301423a6e8631478254bbfae8
gate_base_kind: own-base
merge_state: conflicts (origin/main@b841cd14f9bc3ab7ee0671c33c61c92d55239533; files: internal/api/dashboardspa/dist/assets/Activity-B8-k8SO1.js,internal/api/dashboardspa/dist/assets/Activity-DhcI1PRO.js,internal/api/dashboardspa/dist/assets/Activity-PV7mTNyI.js,internal/api/dashboardspa/dist/assets/AgentDetail-BJji5GNS.js,internal/api/dashboardspa/dist/assets/AgentDetail-Cwi5BcG0.js,internal/api/dashboardspa/dist/assets/AgentDetail-Ra62xC3b.js,internal/api/dashboardspa/dist/assets/Agents-CC2otK8T.js,internal/api/dashboardspa/dist/assets/Agents-DJrnAZCR.js,internal/api/dashboardspa/dist/assets/Agents-aGMzKWm8.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-B2LGGWE2.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-CF_Txw5w.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-CicdwUqi.js,internal/api/dashboardspa/dist/assets/Beads-BCJnB4y7.js,internal/api/dashboardspa/dist/assets/Beads-DgE1ywG1.js,internal/api/dashboardspa/dist/assets/Beads-hm_IzjcQ.js,internal/api/dashboardspa/dist/assets/CockpitHome-CqeURPPb.js,internal/api/dashboardspa/dist/assets/CockpitHome-_XxuhRJy.js,internal/api/dashboardspa/dist/assets/CockpitHome-mbOt4kX3.js,internal/api/dashboardspa/dist/assets/Field-1ertNRCn.js,internal/api/dashboardspa/dist/assets/Field-Bi-JdwoJ.js,internal/api/dashboardspa/dist/assets/Field-CrJQtJP_.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-7E4JF8f6.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-dvujVqzB.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-hKRSW5eC.js,internal/api/dashboardspa/dist/assets/Health-BJATAoQ5.js,internal/api/dashboardspa/dist/assets/Health-DrAun39l.js,internal/api/dashboardspa/dist/assets/Health-KTKHQNUN.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-Ccc6d09q.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-CsBZgDeY.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-D7rcDidr.js,internal/api/dashboardspa/dist/assets/Mail-B0-2biPx.js,internal/api/dashboardspa/dist/assets/Mail-CtvOp5_h.js,internal/api/dashboardspa/dist/assets/Mail-Cv9oMxR4.js,internal/api/dashboardspa/dist/assets/PageHeader-BOHyYq8B.js,internal/api/dashboardspa/dist/assets/PageHeader-LjWwB193.js,internal/api/dashboardspa/dist/assets/PageHeader-eOK-2J28.js,internal/api/dashboardspa/dist/assets/Runs-ByXVivw_.js,internal/api/dashboardspa/dist/assets/Runs-CJWDvmjf.js,internal/api/dashboardspa/dist/assets/Runs-DVg3aWdW.js,internal/api/dashboardspa/dist/assets/SseIndicator-BayyQKjt.js,internal/api/dashboardspa/dist/assets/SseIndicator-DZTozfIF.js,internal/api/dashboardspa/dist/assets/SseIndicator-DnWnxFuS.js,internal/api/dashboardspa/dist/assets/StageLadder-B_sPkllL.js,internal/api/dashboardspa/dist/assets/StageLadder-CLe4PFNW.js,internal/api/dashboardspa/dist/assets/StageLadder-K0xpEHCS.js,internal/api/dashboardspa/dist/assets/Table-BBp1KXzQ.js,internal/api/dashboardspa/dist/assets/Table-DKCv4Bw8.js,internal/api/dashboardspa/dist/assets/Table-DkF6P8ey.js,internal/api/dashboardspa/dist/assets/agentReads-BVUtUCs5.js,internal/api/dashboardspa/dist/assets/agentReads-DinvcxiX.js,internal/api/dashboardspa/dist/assets/agentReads-QFesEfLF.js,internal/api/dashboardspa/dist/assets/constants-BI9F4CpJ.js,internal/api/dashboardspa/dist/assets/constants-C4wxxXsY.js,internal/api/dashboardspa/dist/assets/constants-DUeQ5BXS.js,internal/api/dashboardspa/dist/assets/index-BOJb5U98.js,internal/api/dashboardspa/dist/assets/index-BqToFHq9.js,internal/api/dashboardspa/dist/assets/index-ZVy6XErg.js,internal/api/dashboardspa/dist/assets/projectOf-CWMfq2j4.js,internal/api/dashboardspa/dist/assets/projectOf-CluCInrd.js,internal/api/dashboardspa/dist/assets/projectOf-DSQ64nhn.js,internal/api/dashboardspa/dist/assets/useListFilters-C2BHrL0q.js,internal/api/dashboardspa/dist/assets/useListFilters-DPvkpPSW.js,internal/api/dashboardspa/dist/assets/useListFilters-DRJnz1gK.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-CKFHB9o7.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-Cp9IgKzS.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-DHHaDcTa.js,internal/api/dashboardspa/dist/index.html)
merge_validation: conflict resolution and checks on the final merged result belong to the merge authority; this run gates the reviewed source on its own base. No rebase or source change authorized.

| # | Criterion | Current evidence |
|---|---|---|
| 1 | Review PASS | PASS: ga-blum35eu reviewed durable staging; ga-1pkh1tml reviewed the combined guard. The recorded predecessor and recut have matching exact patch-id 33697fddbc4033e70be6edd34693ad00e050334e, independently recomputed in this recovery. |
| 2 | Acceptance | PASS: A1–A7 evidence below. |
| 3 | Tests | PASS: complete five-phase runtime, 16 native-floor steps, all owned tests and pinned-base SKIP comparisons; all six required CI leaves accounted below. Two Windows runtime jobs deferred under the non-Linux policy. |
| 4 | Unresolved HIGH findings | PASS: zero HIGH findings. Review residuals remain disclosed; the accepted v2 guard is included in this combined source. |
| 5 | Clean final branch | PASS: reviewed source and canonical scratch clean before the record; this gate is the sole added file on the isolated deploy branch. Push is conditioned on a clean post-commit worktree and exact source ancestry. |
| 6 | Merge state and build/vet | PASS: build //... and make vet/nogo terminal success in c6-independent-audit.json; merge conflicts recorded above. |
| 7 | Single theme | PASS: durable self-handoff and its eligibility guard, with session/event/mail/API/generated representations. Ancestry guard was checked against the explicit same-theme sibling set; rerun before cutting a deploy branch. |

## Acceptance evidence

- A1: six controller/config precedence rows and six exact-one-probe rows passed through real handoff logic and stores. Undetermined mode never stages on the city path.
- A2: ineligible named/tokenless callers passed with zero probes, ordinary visible mail, and no mode notice. Unset context delivers immediately.
- A3: two-incarnation born-split successor test and explicit-legacy staging controls passed. Staging, hidden reads, supersession, failure retention, separate stores and successor-only exactly-once release passed by name.
- A4: retained independent review supplies compiling RED/control and nine mutant kills. These establish test sensitivity and are not credited as this release run’s runtime evidence.
- A5: fresh default nogo, shell policies, generated drift and complete runtime succeeded. All six required CI leaves are now accounted below.
- A6: existing identify probe has 500 ms dial, 500 ms write and 2 s read bounds; ping fallback uses the same bounds. Four worker-site cases pass with zero controller probes. Actual qualifying runHandoff callers measured 48,219,132 ns offline and 21,662,832 ns with production runController over its real identify socket, fake runtime and desired-state builder paused; one sample each. Both staged a brief. This is not a live v2 deployment proof.
- A7: zero-growth clause (ii). Literal complexity-check at base and merge both exit 2; all 235 rows identical/no worse, complexity-diff reports no threshold changes, baseline untouched. No baseline waiver.

## Runtime evidence

test_cmd: env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-3a3u82ug /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/merge --base 420b7bb65fff819301423a6e8631478254bbfae8 --bead-json /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/bead.json --owned /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/owned.tsv --run-dir /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/criterion3-run/evidence

Five phases: unit, acceptance, integration-packages, integration-full excluding the latency list, and integration-latency. Each uses --nocache_test_results and --noremote_local_fallback. The runner’s documented native floor also completed all 16 steps.

```text
heavy_mode: none
heavy_units: n/a
coverage_check: n/a (ordinary full-suite runner)
policy_lane: PASS (13 Bazel policy/format targets, pinned-base fresh view)
drift_lane: PASS (7 generated drift targets and 1 OpenAPI target, separate fresh views)
ci_lane_run: n/a (no CI-config changes)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
policy_attribution: none; literal complexity acceptance scored by zero-growth clause (ii)
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: PASS=154575 FAIL=0 SKIP=606
test_log_dir: /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/criterion3-run/evidence/remote/logs
farm_test_seconds: 12500.692
fallback_reason: None
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 73.28791809082031, "waited_seconds": 0.001}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
diff_tests_executed: 40 package-qualified roots PASS, 169 farm owned-root/subtest rows PASS; zero owned SKIP/FAIL; by-name-final.txt and owned-pairs-final.json
skip_inventory: criterion3-run/evidence/execution.json skip_inventory plus skip-final-audit.json, skip-site-proof-final.txt and pinned-base farm/native comparisons
test_exec_bep: 5 phase BEPs at criterion3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json; per-phase proof below
owned_topup: none required; all 40 owned roots executed in the complete suite
floor_topup: 16/16 steps, all exit 0; beadstest, Darwin compile, runner self-tests and environment-absence package top-ups included
latency_leg: integration-latency, attempt 1, clear probe; both listed tests PASS, no deferral
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 127.42
run_max_load: 136.67
run_mean_load: 126.31
run_readings: 14
wait_first_load: 99.25
wait_max_load: 134.95
wait_mean_load: 81.76
wait_readings: 61
```

The load fields above belong to the mandatory native-floor wrapper; farm admission has no host-load wait. Preserve each additional CI job’s own LOAD_GATE_SUMMARY in its accounting.

BEP proof by phase:

```json
{
  "unit": {
    "results": 415,
    "remote_executed": 415,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "acceptance": {
    "results": 25,
    "remote_executed": 25,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-packages": {
    "results": 86,
    "remote_executed": 86,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-full": {
    "results": 14,
    "remote_executed": 14,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-latency": {
    "results": 1,
    "remote_executed": 1,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  }
}
```

Counts are top-level terminal result rows per job, summed over 557 logs; repeated executions count per job and subtests are excluded. The raw SKIP audit includes subtests: 664 rows across 309 unchanged source sites, zero diff-owned SKIPs or unresolved sites. Every skip reason/site is retained in skip-rows-final.json, skip-reasons-final.json and skip-site-proof-final.txt. All 482 farm SKIP rows with shared/helper reach matched pinned-base reasons in 32 groups; all 72 native SKIP rows matched in six groups, same command/tag/flags and reasons. Other unchanged sites are platform/explicit-opt-in/helper or environment preconditions reached before the changed behavior; their exact reasons and reach arguments are retained, rather than counted as passes.

## Diff-owned results

All 40 package-qualified roots PASS, no owned root or subtest SKIP/FAIL. Package-qualified per-action source evidence: owned-pairs-final.json; by-name-final.txt and audit-owned-pairs.py.output.log.

| Package | Test | Result |
|---|---|---|
| cmd/gc | TestBornSplitSuccessorCanHandoffWithoutAStagedBriefWedge | PASS |
| cmd/gc | TestControllerLossLeavesSelfHandoffProvisional | PASS |
| cmd/gc | TestCreateHandoffMailRoutesThroughProviderSeam | PASS |
| cmd/gc | TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession | PASS |
| cmd/gc | TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder | PASS |
| cmd/gc | TestDoPrimeWithHook_JSONModeDoesNotArchiveAutoHandoff | PASS |
| cmd/gc | TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen | PASS |
| cmd/gc | TestEligibleSelfHandoffProbesControllerExactlyOnce | PASS |
| cmd/gc | TestExplicitRestartCommandsPokeControllerWithoutPeriodicTick | PASS |
| cmd/gc | TestFailedSelfHandoffRemainsProvisionalAndAuditable | PASS |
| cmd/gc | TestHandoffStagedArmLandsInTheBindingOnAMigratedCity | PASS |
| cmd/gc | TestIneligibleSelfHandoffDoesNotProbeReconciler | PASS |
| cmd/gc | TestLegacyPinnedCityKeepsSelfHandoffStaged | PASS |
| cmd/gc | TestLiveSuccessorReleasesStagedHandoffExactlyOnce | PASS |
| cmd/gc | TestNonRestartingHandoffModesRemainOrdinaryMail | PASS |
| cmd/gc | TestOriginatingIncarnationCannotReleaseStagedHandoff | PASS |
| cmd/gc | TestReconcileReleasesStagedHandoffAcrossDistinctStores | PASS |
| cmd/gc | TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore | PASS |
| cmd/gc | TestRestageAfterStagedTimeoutRestartsReleaseLifecycle | PASS |
| cmd/gc | TestRestageDefersToControllerWhenEarlierBriefIsReleasable | PASS |
| cmd/gc | TestRestageIgnoresDanglingStagedPointer | PASS |
| cmd/gc | TestRestageLeavesResolvedPriorBriefAlone | PASS |
| cmd/gc | TestRestageRoutesEachClassToItsOwnStore | PASS |
| cmd/gc | TestRestageSupersedesPendingStagedBrief | PASS |
| cmd/gc | TestRestageSupersedesWrittenOffBriefEvenAfterTokenChange | PASS |
| cmd/gc | TestRestartableSelfHandoffStagesBeforeRestartRequest | PASS |
| cmd/gc | TestRetryAfterFailedRestartSupersedesProvisionalBrief | PASS |
| cmd/gc | TestRowFirstRestartDurableWriteFailedPokeExitsZero | PASS |
| cmd/gc | TestSelfHandoffRestartNotRecordedWording | PASS |
| cmd/gc | TestSelfHandoffStagesOnlyForPositivelyLegacyReconciler | PASS |
| cmd/gc | TestSelfHandoffWithUnsetModeContextDeliversImmediately | PASS |
| cmd/gc | TestSessionReconcilePhaseTraceUsesDistinctSites | PASS |
| cmd/gc | TestSessionStartAutoHandoffUsesProvidedStoreWithoutOpeningCity | PASS |
| cmd/gc | TestStagedHandoffTimeoutNeverReleasesToOriginatingIncarnation | PASS |
| cmd/gc | TestWorkerSiteHandoffRecordsTheRestart | PASS |
| cmd/gc | TestWorkerSiteSelfHandoffStagesOnlyForLegacySiteConfig | PASS |
| internal/events | TestHandoffLifecycleEventsAreKnownAndTyped | PASS |
| internal/mail/beadmail | TestBulkArchiveLeavesAStagedHandoffOpen | PASS |
| internal/mail/beadmail | TestExistingOrdinaryAutoHandoffRemainsVisible | PASS |
| internal/mail/beadmail | TestStagedHandoffIsInvisibleAcrossEveryMailReadSurface | PASS |

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/merge 420b7bb65fff819301423a6e8631478254bbfae8 42d89489f2426b329afa4504b5982e21063c991c fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs.

ci_required_accounting:

runner-policy | LOCAL-PASS | bash /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-runner-policy.sh; exit 0; /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-required/runner-policy.log
changes | LOCAL-PASS | tool closure derivation; exit 0; /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-required/triggers.txt
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
pack-gate | LOCAL-PASS | bash /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-pack-gate.sh; exit 0; /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-required/pack-gate.log
windows-lane | LOCAL-PASS | bash /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-windows-lane.sh; exit 0; /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/ci-required/windows-lane.log
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

The local scripts execute the workflow job’s own run steps and environments; the Windows lane cross-compiles and never claims Windows runtime execution. Each local job used a separate fresh tree, admission, bounded load wait and isolation wrapper, sequentially.

runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=111.02 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=107.15 wait_max_load=111.02 wait_mean_load=80.19 wait_readings=62 read_errors=0 prewait_s=1800
pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1771 wait_timed_out=0 run_start_load=14.59 run_max_load=15.80 run_mean_load=15.44 run_readings=4 wait_first_load=114.30 wait_max_load=114.30 wait_mean_load=62.26 wait_readings=61 read_errors=0 prewait_s=1771
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=600 wait_timed_out=0 run_start_load=13.99 run_max_load=16.14 run_mean_load=15.29 run_readings=4 wait_first_load=15.82 wait_max_load=24.08 wait_mean_load=17.88 wait_readings=22 read_errors=0 prewait_s=600

windows_reach: credential-provider-windows=none; windows-lane-test=none (separate test-binary import closure computation).
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none

The PR may merge only after CI / required is green on its current head and each deferred job succeeds, or skips because its own path filter is false. No CI result on another head supplies that evidence.

## Dashboard and limits

Local dashboard build exit 0 with no generated drift; npm run preview returned HTTP 200 for HTML and entry JavaScript. Both matched the canonical commit by SHA256; the owned preview process exited. Proof: dashboard-preview-verified.json.

Accepted limits: mode flips after staging and CLI/controller version skew; no end-to-end live v2 city boot. Initial FAST DNS/capabilities refusal had no test verdict. Initial supplemental A6 harness failed nogo and is invalid; only the corrected isolated attempt2 contributes A6. All original outputs retained. No product tree was altered for supplemental checks.

## Evidence locations

/var/tmp/gc-gate-ga-3a3u82ug-1l77kge6

Final recovery audit: final-evidence-audit.json and recovery-20261115T034924Z/. It re-resolves all commits, re-derives carryover and owned inventory, verifies all 1082 CAS artifact digests/541 copied logs, inspects fresh remote BEP action windows/options, confirms C6/FAST/native-floor terminals and served asset hashes.

Release record adds no product change. Review carryover, SHA identity, complete evidence and current-base ancestry scope were independently checked. Commit/push hooks remain active. Normal publication guards and exact-head verification remain mandatory.

## Retained evidence integrity

Full logs and inventories stay at the evidence root above; pointers identify their exact retained contents. docs/PROJECT_MANIFEST.md is absent from the gated tree; the active deployer release-gate protocol supplies these seven criteria, and the gated workflow/TESTING.md supply commands.

- context.json: SHA256 9c2d34e93b2ab3e5dcf6d8f77a115be07dd2962c222087c9496d7148d3a66075
- criterion3-run/evidence/execution.json: SHA256 503d7b0d7da3521b58aafdfe9e6e092713b1ab99d4f89814d6776d89da9d054d
- c6-independent-audit.json: SHA256 5065a70abb3fd002f3679b3abc837e986999b6588a3270b82c131221e30bf4ad
- fast-attempt2/fast-independent-audit.json: SHA256 7a61132b9556b9e0494269a2b0763c8921fc17f3de0c7416802ca3eeac38a45d
- owned-pairs-final.json: SHA256 c90b5f1dffeb4fbb3138c9856aaa43d76bf712c2d64e9a1f94dffaebc761b222
- skip-final-audit.json: SHA256 86caf09ae228aae69eb98d13aec772ce2bc999656f8b3659606effe3d8aeef02
- skip-site-proof-final.txt: SHA256 b566454db9399b9fcb364eac39add4ac6fd3661cba443f868e2563c7f587f632
- shared-skip-comparison/comparison.json: SHA256 2090e68a8fbc71ce437a21d0403818038602346373bcbdc890e7d13d2710673a
- native-skip-comparison/comparison.json: SHA256 e1735198097680f1a02c798e6f58b416722834b4063b1d0d002a674d522fdf22
- ci-required/accounting.json: SHA256 12fd3d3971890b1792c6275703e676d9cc05cf30a14439059b99a02ce3474e78
- latency-probe-attempt2-verified.json: SHA256 41e6c65658c7e93fa2911bfef900c42139cb7fd4a764260c5af6341ed573498f
- dashboard-preview-verified.json: SHA256 76228da71de0fefeafabf282c061d30549bf306100e79b071d21264611373723
- icu74.txt: SHA256 dc1ab2e8ccf94bcf9a0c827a085e548134cafef5ec8378acc25ae4c948c21196
- runtime-probe.log: SHA256 855779b4fb87aa6a7283bb47561b5bdd94df7e5bbd97b023515fb20cb31759a2

Environment readiness: HEAVY_MODE mode=none shard_required=- watch=-; GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74". Runtime probe verified active rootless podman, cached pinned Dolt 2.6.0 and pinned bd v1.3.4 before the suite. No library path overlay or host mutation was used.

Final independent evidence audit: /var/tmp/gc-gate-ga-3a3u82ug-1l77kge6/final-evidence-audit.json; SHA256 59917f0a7099b62dde1fad217320b2ee05eb8f5cda463c2a40054b20be146521.
