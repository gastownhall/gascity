# Native-open ambient environment readers — release gate

**Verdict:** **PASS**

Fresh evaluation 2026-11-17T01:50:41.234486+00:00. Bead `ga-639zcbhu`; build `ga-djl940ie`; review `ga-awjwo8d5`.

Reviewed source: `e9cdef0341d72eba11dfbbbd2b88c8dc8531c885`. Pinned base: `09518c191d1a9bea38f09d738f150bec7743a422` (`origin/main`). Canonical merge: `edaa94928c9f0988398df1e209be8e4249b665b0`; tree `6203e5bcd1acdd3682d5bcda22e9f07cc84b32f1`.

The prior hold was released after independent investigation ga-7i4fp4gi and mayor mail gcg-10903553. That investigation supplies no PASS or waiver here: every primary phase below ran fresh. This is a normal isolated replacement deploy, not a clearance backfill for predecessor PR #9164.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-awjwo8d5 records style, security and spec PASS at the exact resolved source SHA. The setup-command conflict received real review; no patch-id carryover or reviewer-added commit is assumed. |
| 2 | Acceptance criteria met | PASS | All twelve original design groups (ga-zyfet7) map to named passing tests; 40/40 package/test pairs across seven packages PASS. Static checks confirm snapshot-before-projection, idempotent release, context bounds, ambient reads, Windows name semantics, deleted lock references and preserved shell seam. Race churn test also PASSes. |
| 3 | Full-suite tests and CI accounting | PASS | Five fresh remote phases, 16/16 native floor checks, pinned-base SKIP comparisons, all changed tests and six CI-required leaves accounted below. Race validation-tool failure is separately attributed, never called a successful command. |
| 4 | No unresolved high-severity findings | PASS | Review has no blocker/high findings; exact review record is retained. |
| 5 | Final branch clean | PASS | Canonical tested checkout status is empty after all checks. Publication requires a gate-record-only commit and verifies the isolated deploy branch is clean before push. |
| 6 | Merge state recorded; build/vet | PASS | Clean merge onto the pinned base. Whole-tree Bazel build (1496 targets) and vet/nogo each return0 with successful BEP finishes; normal commit hooks active. Independent merge-tree matches the tested tree. |
| 7 | Single feature theme | PASS | One environment-projection mechanism: lock-free ambient readers and context-aware native-open admission, plus its tests/comments. Runtime production edits are comment-only; the rebase preserves the existing shell seam. Source ancestry scope guard PASS. |

merge_state: clean (origin/main@09518c191d1a9bea38f09d738f150bec7743a422)
gate_base_kind: current-tip
merge_validation: gastownhall/gascity MPR owns the final merge and its current-head checks; the deployer never rebases or merges.
accepted_ancestry_beads: ga-639zcbhu, ga-djl940ie, ga-awjwo8d5, ga-7x72j106, ga-zyfet7, ga-lhkerf

## Test execution

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/merge.6g5r8old --base 09518c191d1a9bea38f09d738f150bec7743a422 --bead-json /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/bead.json --owned /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/owned-tests.tsv --run-dir /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/criterion3-run/evidence
test_cmd_scope: full-suite
heavy_mode: none (classifier verified; no heavy package owned or reached)
test_exec_mode: remote-exec
fallback_reason: none
test_log_dir: /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/criterion3-run/evidence/remote/logs
retained_evidence_bundle: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-639zcbhu/gate-pass-20261117T015041Z
farm_test_seconds: 11493.909
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 67.2446174621582, "waited_seconds": 0.001}
owned_topup: none (all changed tests present in the primary phases)
floor_topup: 16/16 checks exit0; every environment-absence SKIP selected by package, plus beadstest, Darwin compile and runner self-tests. Separate pinned-base floor: 16/16 exit0.

| Phase | Fresh remote results | Cache / retries / other strategy | BEP window |
|---|---:|---|---|
| unit | 415 | 0 / 0 / 0 | valid; artifact hashes independently verified |
| acceptance | 25 | 0 / 0 / 0 | valid; artifact hashes independently verified |
| integration-packages | 86 | 0 / 0 / 0 | valid; artifact hashes independently verified |
| integration-full | 14 | 0 / 0 / 0 | valid; artifact hashes independently verified |
| integration-latency | 1 | 0 / 0 / 0 | valid; artifact hashes independently verified |

Each primary phase uses remote execution, no test cache, no local fallback and verbose test output. Unit uses the documented make check chain; acceptance and integration use the authoritative Bazel tiers. Full integration runs all fourteen shards minus the named latency split; the separate leg runs both latency tests.

test_counts: remote {"PASS": 155118, "FAIL": 0, "SKIP": 599}; native {"PASS": 24, "FAIL": 0, "SKIP": 14}; combined {"PASS": 155142, "FAIL": 0, "SKIP": 613}
Counts are column-0 Go result lines per retained job, not distinct tests. Remote-only view contains exactly541 action logs; native aliases enter the combined count once. The SKIP inventory includes subtests and has its separate671-record denominator.

latency_leg: TestGcBdLatencyBudget PASS, TestGcSeatHookLatencyBudget PASS; first attempt. Farm probe waited20s from load1=55.78 to42.39 (<50); no deferred or retired test result.
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 7.32
run_max_load: 14.88
run_mean_load: 12.20
run_readings: 7
wait_first_load: 7.32; wait_max_load: 7.32; wait_mean_load: 7.32; wait_readings: 1; read_errors: 0. These are the source native-floor wait measurements; remote admission has no host-load wait. Base-floor wait is retained in native-base-floor.log.

## Changed tests and acceptance

| Package | Changed test | Result |
|---|---|---|
| internal/beads | TestAmbientNativeDoltOpenEnvAnswersDuringNativeDoltOpenEnv | PASS |
| internal/beads | TestAmbientReadersAnswerPromptlyDuringEveryOpenWindow | PASS |
| internal/beads | TestBdStoreCommandRunsDuringAnOpenWindow | PASS |
| internal/beads | TestBdStorePurgeRunsDuringNativeDoltOpenEnv | PASS |
| internal/beads | TestNativeJournalRecheckResolvesTheAmbientEnvInAnOpenWindow | PASS |
| internal/beads | TestOpenWindowIsReleasedAfterEveryWayOut | PASS |
| internal/beads | TestOrdinaryNativeOpenWaitsForWholeBeadsNamespaceRestore | PASS |
| internal/beads | TestProcessEnvSnapshotSeesAmbientDuringProxiedWindow | PASS |
| internal/beads | TestProcessEnvSnapshotShowsTheAmbientValueDuringNativeDoltOpenEnv | PASS |
| internal/beads | TestProcessEnvSnapshotShowsTheWholeBeadsNamespaceDuringAWindow | PASS |
| internal/beads | TestProxiedEventsJournalResolutionAnswersFromTheAmbientEnvInAnOpenWindow | PASS |
| internal/beads | TestProxiedNativeOpenWindowDoesNotBlockChildEnvSnapshots | PASS |
| internal/beads | TestProxiedStoreEventsJournalRefusalOrder | PASS |
| internal/beads | TestReadReconnectReturnsTheBudgetErrorWhileAnotherOpenHoldsTheWindow | PASS |
| internal/beads | TestScopedNativeDoltOpenEnvIgnoresKeysOutsideItsList | PASS |
| internal/beads | TestSecondOpenWindowIsBoundByItsOwnContext | PASS |
| internal/execenv | TestAWaitingWindowTakesItsSnapshotOnlyOnceItHoldsTheToken | PASS |
| internal/execenv | TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiredAsItWasTaken | PASS |
| internal/execenv | TestEnterProjectionWindowGivesTheTokenBackWhenTheContextExpiresAfterTheTokenIsTaken | PASS |
| internal/execenv | TestEnterProjectionWindowReturnsTheContextErrorWhileAnotherWindowIsHeld | PASS |
| internal/execenv | TestEnterProjectionWindowWithAnExpiredContextTouchesNothing | PASS |
| internal/execenv | TestEnvironReturnsAFreshSliceInAndOutOfAWindow | PASS |
| internal/execenv | TestLookupEnvInAWindowKeepsWhatTheWindowWithheldAndAdded | PASS |
| internal/execenv | TestLookupSnapshot | PASS |
| internal/execenv | TestLookupSnapshotAgreesWithOSLookupEnv | PASS |
| internal/execenv | TestLookupSnapshotNamesFollowTheHostsCase | PASS |
| internal/execenv | TestProjectionExitIsIdempotent | PASS |
| internal/execenv | TestProjectionTokenIsReleasedWhenTheWindowBodyPanics | PASS |
| internal/execenv | TestProjectionWindowsAreNotReentrant | PASS |
| internal/execenv | TestReadersReturnPromptlyWithTheAmbientValueDuringAProjection | PASS |
| internal/execenv | TestSecondWindowRunsOnceTheFirstExits | PASS |
| internal/execenv | TestWindowChurnNeverLeaksAProjectionToReaders | PASS |
| internal/runtime/acp | TestStartCanceledDuringTheParentEnvReadSpawnsNothing | PASS |
| internal/runtime/acp | TestStartDuringAProcessEnvProjectionLeavesTheProviderServing | PASS |
| internal/runtime/exec | TestCityOpRunsAtOnceDuringAProcessEnvProjection | PASS |
| internal/runtime/exec | TestDeadlineEnvIsUnaffectedByAProcessEnvProjection | PASS |
| internal/runtime/herdr | TestRunSetupCommandRunsAtOnceDuringAProcessEnvProjection | PASS |
| internal/runtime/subprocess | TestStartDuringAProcessEnvProjectionDoesNotInheritIt | PASS |
| internal/runtime/subprocess | TestStartDuringAProcessEnvProjectionLeavesTheProviderServing | PASS |
| internal/runtime/tmux | TestRunSetupCommandRunsAtOnceDuringAProcessEnvProjection | PASS |

There are40 package/name pairs but38 distinct names: two runtime test names occur in different packages. The independent package-qualified audit verifies all40, including reported subtests. The generic name-only reader is complementary evidence. All twelve design groups are mapped in acceptance-code-test-map.json.

## SKIP evidence

skip_inventory: 671 records (599 remote,72 native), all judged:669 literal pinned-base matches plus2 generated-path-only differences. No new, diff-owned, reasonless or unjudged SKIP. All325 candidate sites are UNCHANGED. Thirty basename ambiguities resolve through compiled import closures to unchanged runtimetest/conformance.go, fencedStart guard line491.
All fourteen SHARED-HUNKS entries are named with compiled-input and caller evidence across51 package/configuration closures. Test-helper imports/comments and changed helper call chains do not reach the skipping roots. Go AST receiver types distinguish the new test-only openSeam methods from unrelated same-spelled callback variables.
The two remaining raw reasons are retained verbatim alongside normalized copies in generated-path-skip-proof.json. Mayor ruling gcg-10825729 on this bead permits only Go temp-directory random digits and Bazel worker work-dir UUIDs to be normalized. Both printed file:line values are equal; both sites are UNCHANGED; normalized reasons are identical. This is evidence equivalence, not a waiver.
TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled: unchanged os.Link EXDEV guard; no changed shared/helper package in its binary closure.
TestProxyProcessReapRegistryMatchesAnEquivalentSpelling: unchanged os.Stat/os.SameFile filesystem guard, before native/proxy construction; setup uses NewMemStore and runtime.NewFake. The imported shared entries are internal/beads/{native_dolt_events_journal_read_test.go,native_dolt_open_env_window_test.go,native_dolt_projection_window_test.go,native_dolt_proxied_open_test.go,native_dolt_store_test.go} and internal/execenv/process_env_test.go. All six dependency test files are absent from this binary’s compiled inputs. Changed production window readers/writers do not execute before this guard.
Raw reasons/classes independently audited:671 records, zero canonical-reason differences and zero newly discovered remote environment-absence classifications.

## Race supplement and attribution

The actual TestWindowChurnNeverLeaksAProjectionToReaders ran once under the race detector and PASSed: one uncached first-attempt remote action, no data-race warning; original test.log/test.xml hashes verified. The command exits1 because nogo/gocritic cannot load fmt from its embedded ruleguard rules, so the race command is not reported as green. No validation was disabled and the candidate test was not rerun.
failure_attribution: race-config gocritic embedded-rule loader -> ga-38b2nor (opened pre-existing tracker; sighting appended). Clause1: changed churn test actually PASSes; the failure belongs to a separate validation tool. Clause3(d): fresh build at the pinned base, without this diff, reproduces the same execenv/testenv importer diagnostic. Clause4: that stronger base proof is recorded, no census bump/new target/new test file in either validation-action package; seven toolchain/importer inputs have identical blobs. Normal full-suite nogo passed. Detailed original nonzero command and control are retained in race-supplement-attribution.json and race-nogo-base-result.json.
waiver_ref: none

## Fast checks and CI-required leaves

Fast lanes ran first in separate fresh-tree views: pinned lint2.14.0, format/nogo/policy and four guards; BUILD sync; all eight generated/drift checks; complexity. Complexity base/merge both exit2 with identical existing diagnostics, and the pinned diff reports no threshold changes. No absolute-baseline rewrite.
ci_lane_run: n/a (no CI job/matrix/timeout/required-list changes in this diff)
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-639zcbhu-fresh-m4zshe76/merge.6g5r8old 09518c191d1a9bea38f09d738f150bec7743a422 e9cdef0341d72eba11dfbbbd2b88c8dc8531c885 fork; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs.

| Leaf job | Accounting status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact merged-workflow command exit0; retained log and admission/load evidence |
| changes | LOCAL-PASS | Exact merged-workflow command exit0; retained log and admission/load evidence |
| credential-provider-windows | DEFERRED-TO-PR-CI | non-linux-runner; reach=none |
| pack-gate | LOCAL-PASS | Exact merged-workflow command exit0; retained log and admission/load evidence |
| windows-lane | LOCAL-PASS | Exact merged-workflow command exit0; retained log and admission/load evidence |
| windows-lane-test | DEFERRED-TO-PR-CI | non-linux-runner; reach=none |

deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
Both native Windows jobs retain independent reach calculations (none). Pack preconditions passed; both live pack steps passed. The exact Windows build/test compile, analyzer cross-compile and all-package Darwin build/test compile passed. This gate authorizes opening a PR; CI / required must be green on its current head before merge.

## Evidence integrity and publication

Preflight: own predecessor PR #9164 was OPEN at its older head; the reviewed replacement source was not an ancestor of the pinned main. Only recognized automation/self engagement was present. Main was frozen before C6; no self-rebase or source-branch push occurred.
An initial invocation was rejected for an argv error before any test or BEP existed; its terminal logs remain under criterion3-argv-rejected. Corrected invocation used a fresh directory and supplies all primary evidence above. Auxiliary proof refinement retained the original conservative method-name collision result and the final receiver-aware proof; no test was repeated for it.
Evidence sources: TESTING.md, authoritative release criteria and release-gate conventions; docs/PROJECT_MANIFEST.md is absent. Current protocol snapshots and criterion3-start city commit proof are retained.
The bundle contains all raw logs, original BEPs/test artifacts, per-name readbacks, base comparisons, static/review checks, CI receipts and the gate record, with a SHA256 manifest. Publication will preserve the reviewed source, tested merge and record commit under refs/gate-evidence and verify the PR’s exact current head before clearance.
