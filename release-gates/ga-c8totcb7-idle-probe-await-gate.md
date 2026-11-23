# Idle-probe helper polling — release gate

**Verdict:** **PASS**

Deploy bead: `ga-c8totcb7`; build: `ga-om4h2ko`; review: `ga-2i29z2tx`.
Reviewed source: `008d8e9e3448bb73a61ead8c80786040732ff4e8`. Mode: remote; push remote: fork; base branch: main.
Pinned gate base: `37b7b762edba99b21e62b6d73af109e40fd563c8`; gate_base_kind: current-tip.
Tested merge: `6d942d352b63038c16b5a987dde55b7fc31d6e1a`; tree: `30469ecac4c68b460b400ecb9edf0a21910bddac`.
merge_state: clean (origin/main@37b7b762edba99b21e62b6d73af109e40fd563c8; files: none).
merge_validation: merge result built and vetted here at the pinned base. MPR owns merging; a later head requires a fresh gate.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer PASS on the exact resolved source; no carryover or reviewer-added commit. |
| 2 — Acceptance | PASS | awaitIdleProbes uses awaitCond with the same mutex-protected readiness predicate, standard hang budget and 5 ms polling; no Gosched spin remains. simGuard, its invariants and production code are unchanged. No test bodies are diff-owned; all 22 affected tests passed by name. Required PR references are prepared in the body and verified against the opened PR before close. |
| 3 — Tests and policy | PASS | Complete five-phase uncached remote suite, native floor, policy/drift lanes, unchanged sites plus complete required base skip comparisons and six-leaf required-CI accounting below. |
| 4 — Severity | PASS | Zero unresolved HIGH findings in the review. |
| 5 — Clean branch | PASS | Role checkout and test scratch pristine before writing this record. The isolated deploy head must contain the reviewed source plus this record only; publication verifies a clean worktree and exact record-only diff after commit. |
| 6 — Merge build/vet | PASS | Exact clean merge tree built and vetted/nogo; both BEPs overallSuccess=true. |
| 7 — Theme | PASS | One test-helper polling theme, one Go test file plus this record. |

The helper takes and releases its mutex within each predicate check, before awaitCond sleeps. Source scope is one _test.go file. The PR body must reference ga-om4h2ko and ga-7dtk0ks; no GitHub issue or issue-closing directive. The mayor approved the test-helper RED/GREEN exemption. Timing acceptance is satisfied by the reviewer’s 40 interleaved uncached samples: changed/base mean 11.65/11.07 seconds = 1.0524, median 9.79/10.59 = 0.9245, below 1.5. Builder ABBA totals independently report 109.2/124.9 seconds = 0.8743. These are reviewer/builder measurements, not a new deployer benchmark; deployer recomputed the ratios from the live review note. Raw benchmark logs were not available at reviewer artifact paths. See timing-review-proof.json for the full measurement note and provenance.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (Gas City has no waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-c8totcb7 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.uel09Y --base 37b7b762edba99b21e62b6d73af109e40fd563c8 --bead-json /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-bead.json --owned /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/owned.tsv --run-dir /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence`.
Unit command: make -k check (whole-tree bazel test //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration packages: bazel test --config=integration //test:integration_packages; full integration and its separated latency leg: //test/integration:integration_test. Exact per-phase flags, commands and BEPs are retained in the execution proof.
Phases: whole-tree unit/check; acceptance; every integration-tagged package; full integration excluding latency inventory; inventory-only latency under a clear farm probe. All force --nocache_test_results and --noremote_local_fallback, first attempts, remote strategy and phase-window start proven from BEP.
diff_owned_evidence: /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/merge-owned-explain.txt (owned set identical at source and tested merge).
not_owned_skip_evidence: /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/skip-source-audit/evidence.json; per-name unchanged sites plus shared-helper/import reach and complete matching base comparisons in base-skip-evidence/report.json.
test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.uel09Y/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
gate_base: 37b7b762edba99b21e62b6d73af109e40fd563c8; gate_head: 008d8e9e3448bb73a61ead8c80786040732ff4e8; delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"37b7b762edba99b21e62b6d73af109e40fd563c8","head":"008d8e9e3448bb73a61ead8c80786040732ff4e8","merge":"6d942d352b63038c16b5a987dde55b7fc31d6e1a","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"7fed3d8dacb93b737121ee3868ee8d45794694c25b66af4eec4f187a08a4a71d"}.
failure_attribution: none; policy_attribution: none; post_merge_criteria: none.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
admission: {"class":"remote","floor_gib":25.0,"free_gib":58.08424377441406,"waited_seconds":0.005,"cap":2}.
test_exec_bep: {"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}.
owned_topup: [].
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 13675.678.
test_log_dir: `/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/logs` (retained).

```text
test_counts: PASS=158783 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed: none (no diff-owned tests)
affected_tests_executed (required-name inventory, not ownership):
  TestAPIKillUnderFenceIsNotACrash PASS (integration-packages-e5330b8c981927473156.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestCutoverRoundTripV2LegacyV2 PASS (integration-packages-75f7330b9b673df65e4f.log, unit-64014bc0bb3833bc198c.log)
  TestEveryKeptBehaviorOfARegisteredArmHasAParityFixture PASS (integration-packages-12697e2ea0d32c278115.log, unit-423c6a0da1986b90e9a7.log)
  TestFreshDrainBeginsReachAdmission PASS (integration-packages-e4245ddf6a37030111bc.log, unit-fd986cf5bab8709b8d45.log)
  TestIdlePaceHoldsOnlyIdleProbes PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestIdleProbesPaceAPatrolApart PASS (integration-packages-821a75e4a503601af052.log, unit-752a0bf25b15b465517a.log)
  TestKeptGateBindsAKindOnceARegisteredArmProposesIt PASS (integration-packages-9326181a736eb30b71ca.log, unit-1ab526405ace21879f85.log)
  TestKillFenceLiftsAfterTheLeaseInBothModes PASS (integration-packages-12697e2ea0d32c278115.log, unit-f93a0ca96ccc61d0cae9.log)
  TestKilledWokenSeatRestartsAsANewIncarnationInBothModes PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-abd97d2cb9b450225d57.log)
  TestParkedSeatResumesInItsRecordedWorkDirWhenItsPinTurnsReady PASS (integration-packages-b72a67a2f9444af705e3.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestParkedSeatWhoseRecordedWorkDirIsGoneWakesFreshInTheDerivedDir PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-3d6d3036a2f5902472b5.log)
  TestParkedSeatWhoseRecordedWorkDirIsInAnotherRigWakesFreshInTheDerivedDir PASS (integration-packages-e5330b8c981927473156.log, unit-27b23e708b477fce4dad.log)
  TestRollbackGuardReadsPresenceFromTheAttachProbe PASS (integration-packages-bc482d2ae768251ad47f.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestRollbackHoldsAV2SignaledStopForAnAttachedClient PASS (integration-packages-cb110414c557c0928e82.log, unit-493754ea2d5b4d8858c8.log)
  TestSessionDifferentialEntriesHaveFixtures PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-637c74c578a2b17fc654.log)
  TestSessionDifferentialFixtures PASS (integration-packages-e5330b8c981927473156.log, unit-27b23e708b477fce4dad.log)
  TestSessionTimelineDifferential PASS (integration-packages-75af79934639abaae69c.log, unit-f93a0ca96ccc61d0cae9.log)
  TestTimelineExplainsAreStepScoped PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-d9e667428f972d067bd3.log)
  TestTimelineGoldenNeedsARuling PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-088cc38d26d094729229.log)
  TestTimelineLegacyGoldenIgnoresV2 PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-912ab643330b35bd0b6b.log)
  TestV2KillFenceGraceRecoversAFailedLeaseRelease PASS (integration-packages-9326181a736eb30b71ca.log, unit-912ab643330b35bd0b6b.log)
  TestV2WrittenRowsConvergeUnderLegacy PASS (integration-packages-568812e4d525bdd1f974.log, unit-2ef13e1b6898c5544764.log)
```

policy_lane: PASS — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; explicit nogo/lint; thirteen policy/docsync targets.
drift_lane: PASS — make bazel-sync followed by git diff --exit-code and empty porcelain; genspec/schema/client/SPA/lockfile sync targets; openapi-breaking-check against the pinned gate base.
fast_lane: PASS 2026-11-22T18:08:10.592485+00:00 merge_head=6d942d352b63038c16b5a987dde55b7fc31d6e1a.
known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree); known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close; known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green).
format_lane: PASS — //:go_format_test. All lanes ran in separate fresh views before the suite. Strict storeclass predicates used trusted base tools and passed.
complexity: zero-growth form (ii); base and merge both exit 2, no merge row worse than its base row, no new/regressed/deleted threshold changes, baseline untouched. Raw outputs retained; this is the documented scoring rule, no waived failure.
ci_lane_run: n/a (no CI-config change).

skip_justification: all 319 printed skip sites are UNCHANGED. SHARED-HUNKS in reconcile_session_differential_test.go removes an import, updates a comment and changes awaitIdleProbes; actual test-binary import closures require baseline measurements for cmd/gc. Three remote groups and the native group all exit 0 and match every selected skip reason at the pinned base. The helper readiness condition is preserved and no skip predicate changed. No owned test body exists; all 22 affected tests pass.

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a02, head rbe-a13)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
```

Native environment: rootless Podman/Dolt image and sweep preflight passed; ICU 74 recorded libraries verdict=OK.

Load observations (wait_* and run_* are separate; timed-out ordinary waits are recorded):

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 20.01
run_max_load: 23.90
run_mean_load: 18.36
run_readings: 61
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1440 wait_timed_out=0 run_start_load=14.67 run_max_load=19.43 run_mean_load=17.25 run_readings=5 wait_first_load=25.70 wait_max_load=26.60 wait_mean_load=20.04 wait_readings=49 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=20.01 run_max_load=23.90 run_mean_load=18.36 run_readings=61 wait_first_load=20.01 wait_max_load=20.01 wait_mean_load=20.01 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.82 run_max_load=18.22 run_mean_load=17.08 run_readings=8 wait_first_load=17.13 wait_max_load=19.69 wait_mean_load=17.42 wait_readings=13 read_errors=0
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.uel09Y 37b7b762edba99b21e62b6d73af109e40fd563c8 008d8e9e3448bb73a61ead8c80786040732ff4e8 fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs..

ci_required_accounting:

- runner-policy | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y runner-policy; exit 0; log /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/required-ci/runner-policy.stdout
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.uel09Y 37b7b762edba99b21e62b6d73af109e40fd563c8 008d8e9e3448bb73a61ead8c80786040732ff4e8 fork; exit 0; log /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/required-ci/closure.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y windows-lane; exit 0; log /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/required-ci/windows-lane.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none for each native Windows job, each checked by its own test import closure.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open PASS does not replace green CI / required or successful deferred jobs on this exact head.

Commands and exit statuses (native floor commands supplement the full suite):

```text
bazel build --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c6-vet.bep.json //... ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-c8totcb7 --label fast-format --record /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-format-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-lane.sh format ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-c8totcb7 --label fast-policy --record /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-policy-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-lane.sh policy ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-c8totcb7 --label fast-drift --record /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-drift-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-lane.sh drift ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-c8totcb7 --label fast-openapi --record /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-openapi-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-lane.sh openapi ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.uel09Y -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-c8totcb7 --label fast-complexity --record /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-complexity-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-lane.sh complexity ; exit=0
go build -o /var/tmp/gc-storeclass-trusted-whut7oav/storeclass-baseline-trusted ./analyzers/storeclass/cmd/storeclass-baseline ; exit=0
/var/tmp/gc-storeclass-trusted-whut7oav/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.kW60OY ; exit=0
/var/tmp/gc-storeclass-trusted-whut7oav/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.kW60OY -growth-since 37b7b762edba99b21e62b6d73af109e40fd563c8 -base-tip 37b7b762edba99b21e62b6d73af109e40fd563c8 -gate-only ; exit=0
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1795370903139 end_ms=1795371079517
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1795371080511 end_ms=1795371200310
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1795371201012 end_ms=1795371454513
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1795371455499 end_ms=1795372042124
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1795372042937 end_ms=1795372144672
go test -tags integration -count=1 -v -run '^(TestCollectInputConvoyRootsAgreesWithTheFrozenReference|TestConvoyAutocloseAgreesWithTheFrozenReference|TestMoleculeAutocloseAgreesWithTheFrozenReference|TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc ; exit=0
go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown ; exit=0
go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads ; exit=0
go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest ; exit=0
go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract ; exit=0
go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor ; exit=0
go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr ; exit=0
go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest ; exit=0
go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux ; exit=0
go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers ; exit=0
go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration ; exit=0
make test-fsys-darwin-compile ; exit=0
bash scripts/test-push-gate-lock.sh ; exit=0
bash scripts/test-local-concurrency.sh ; exit=0
python3 .github/workflows/scripts/runner_policy.py
 ; exit=0
GOOS=windows go build $(cat .github/windows-lane/build.txt) ; exit=0
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt) ; exit=0
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/... ; exit=0
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./... ; exit=0
```

Execution metadata and inventories are retained by hash, not pasted into this record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote","gate_base":"37b7b762edba99b21e62b6d73af109e40fd563c8","gate_head":"008d8e9e3448bb73a61ead8c80786040732ff4e8","admission":{"class":"remote","floor_gib":25.0,"free_gib":58.08424377441406,"waited_seconds":0.005,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"37b7b762edba99b21e62b6d73af109e40fd563c8","head":"008d8e9e3448bb73a61ead8c80786040732ff4e8","merge":"6d942d352b63038c16b5a987dde55b7fc31d6e1a","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"7fed3d8dacb93b737121ee3868ee8d45794694c25b66af4eec4f187a08a4a71d"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13675.678,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":83.542,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=8.72 load5=2.95 load15=1.05 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 18.13 6.26 2.27 32/1858 56612"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":217314}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":158783,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"}
```

Full execution retained: 6058 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c3-run/evidence/execution.json; SHA256 0fda99a416f747b56d672b9837b509f909d95802bc841b4e127cea7d3dc42373.
Full skip sites retained: 5426 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/skip-source-audit/evidence.json; SHA256 2fbe647f0559781e78189304f78956884cbdecc1c0cb36d5069c9aae9d1397b8.
Full base skip measurements retained: 241 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/base-skip-evidence/report.json; SHA256 8af0abaf11725ff8e40b1ae01a8acb10e5c2548cc3397d92f6f9db823b14a60d.
Full skip import reach retained: 920 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/skip-import-closures.json; SHA256 647d99c9ece308775d68b73c77f7f825dbb15dd8b5c29141bc496066ddc9fb5d.
Full timing reviewer measurement retained: 16 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/timing-review-proof.json; SHA256 ea2862a80868cbc7ef543cebcacc53bf9096760bf3592b8c06fef02653b5b2cc.
Full affected named results retained: 28 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/final-affected-by-name.stdout; SHA256 49b708d17eaecc5c29ca5418b973cc636f9127ad2b16b2261cf28d619edbf939.
Full CI accounting retained: 172 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/required-ci/accounting.json; SHA256 4385b85251c1120e191a6f87259e2cc2b15028c6c5b15df2d2c9d4c9c729c299.
Full known-red precheck retained: 5 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/known-red-precheck.json; SHA256 bab9e01655a9cd66afa59c6dab6e0e37be9aa1b708b95f4648cb82c51430c4af.
Full runtime preflight retained: 113 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/runtime-preflight.json; SHA256 037baca5d7c71b6e09a76f85a51f2b35eb0089f4320e91556437072a85762246.
Full diff-owned explanation retained: 17 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/merge-owned-explain.txt; SHA256 484b8db8101e40068fc17eea1bacc124edf4872bd78be94214e99c5793e16fc2.
Full fast lanes retained: 173 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/fast-result.json; SHA256 f0a5c82741767c500436a6c020e38c5f3343907ee34f495233f00daac9d99149.
Full merge build/vet retained: 59 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/c6-audit.json; SHA256 2b00b6669be1c504ea2c766409696e19c6420ec4aedfaca983d33f4c89307489.
Full retained evidence manifest retained: 1703 lines; /var/tmp/gc-gate-ga-c8totcb7-s8m7cx6y/gate-evidence-manifest.sha256; SHA256 863c4aeb776b26864fa6e9ab5fb3f1017610f43a6994ce92c6ae4fb4f4d974cc.
