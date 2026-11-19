# Rig add reload release gate

**Verdict:** **PASS**

Bead: ga-7kh81mei; build: ga-a3jvyrtv; review: ga-1iptg9mf
Reviewed commit: 3d4728d695c9c3b7aa17ccb398372065c6b18fbe
Gate base: 131d1c9ea526fb91240b586c28e38b41384e0014
Validated merge: 6bcb2f9d5f717938505685661c841e7a5fac4d35
Validated tree: d7e33f69a56194807b65c6a53254dc957a22c7ff
Sealed: 2026-11-16T21:12:24.833599+00:00

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Reviewer PASS names the resolved reviewed commit; two source commits cite the build bead. No review carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | Waiting-reload contract and no-controller fallback tests pass by name. The reported federation regression passes R/I/G in the full suite and in three local iterations with two-CPU affinity (nine case passes). Cause and deterministic RED are recorded on the build/review beads. Landing obligations are assigned below. |
| 3 | Tests | PASS | Five fresh remote phases, all sixteen native floor checks, owned-test results, complete skip accounting and required CI leaf accounting passed. |
| 4 | No open HIGH findings | PASS | Reviewer PASS records no blocking findings. |
| 5 | Clean final source | PASS | Canonical merge and role worktree clean. Publication checks that only this record is added to the isolated reviewed-source branch. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge, independent merge-tree identity, whole-tree Bazel build1496 targets and vet/nogo pass with normal hooks active. |
| 7 | Single feature theme | PASS | Two source commits, two files: wait for the rig-add configuration reload. |

merge_state: clean (origin/main@131d1c9ea526fb91240b586c28e38b41384e0014)
gate_base_kind: current-tip
gate_base: 131d1c9ea526fb91240b586c28e38b41384e0014
merge_validation: MPR validates the current merge and required PR CI before landing.
release_criteria_source: deployer release-gate-criteria.md.tmpl, TESTING.md and engdocs/contributors/release-gate-criteria-conventions.md; docs/PROJECT_MANIFEST.md is absent.
post_merge_obligations: MPR lands the PR, closes tracker ga-cdv9w80i with actual landing evidence, then requests a fresh gate for #9186 / ga-7o9slun2. None of those landing actions is claimed by this PR-open gate.

policy_lane: PASS; fmt, pinned/resolved golangci-lint2.14.0, whole-tree nogo, four make-check shell guards and policy targets.
drift_lane: PASS; make bazel-sync with empty diff/status, and all eight generated-artifact checks.
complexity_lane: PASS zero-growth(ii); base and merge absolute rc2, byte-identical250-line diagnostics; pinned complexity-diff prints no threshold changes.
fast_lane_isolation: independent fresh-tree views with the recorded gate-base pin; canonical source unchanged.
known_red_precheck: proceed nothing-known-red; check-routed-test-rows green, ga-1ptv4jey stale.

heavy_mode: none
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
farm_test_seconds: 12494.485
test_log_dir: /var/tmp/gate-ga-7kh81mei-c8l5_53f/criterion3-run/evidence/remote/logs
test_counts: PASS=155074 FAIL=0 SKIP=613
Counts above are top-level per-job result lines. The skip inventory includes subtests and has a separate denominator.
diff_tests_executed: both owned rig-add tests PASS by name in unit and integration-package logs.
owned_topup: []
floor_topup: all16 checks exit0 (13 packages, Darwin compile and two runner self-tests).
latency_leg: measured first attempt after20s probe hold, clear at load1=46.28<50; both named budget tests PASS and excluded from the main leg.
skip_inventory: 671 events (599 remote+72 native), 325 UNCHANGED source sites; no touched/unresolved events or missed environment class. Thirty ambiguous source names resolve to unchanged runtimetest fencedStart guards.
skip_justification: skipped cases use fixed lane, OS, helper, provider or store-shape prerequisites; the changed call waits for a post-provision config-reload reply. Every skip site and helper guard is unchanged. The tool reports no SHARED/HELPER test hunks, so the conditional same-base comparison rule is not triggered; no baseline run is claimed. Raw reasons are retained unchanged.

acceptance_native_scope: focused supplement; no full-suite credit
acceptance_native_command: see native-ac.sh and native-ac-run/cmd.argv
acceptance_native_cpu_pair: 0,1
acceptance_native_iterations: 3; parent PASS3; R/I/G PASS3 each; FAIL0 SKIP0; one fresh local test action, attempt1; standard1100s integration timeout unchanged.
acceptance_native_log: /var/tmp/gate-ga-7kh81mei-c8l5_53f/native-ac-artifacts/test.log; SHA256 336b4da52a97e9ece23d4cf0ac212dd305d401ee03e8f23cb4d73a0bf5973d53

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-7kh81mei-c8l5_53f/merge 131d1c9ea526fb91240b586c28e38b41384e0014 3d4728d695c9c3b7aa17ccb398372065c6b18fbe fork; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs.
ci_required_accounting:
- runner-policy | LOCAL-PASS | bash /var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-runner-policy.sh; exit=0; log=/var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-required/runner-policy.log
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-7kh81mei-c8l5_53f/merge 131d1c9ea526fb91240b586c28e38b41384e0014 3d4728d695c9c3b7aa17ccb398372065c6b18fbe fork; exit=0; log=/var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-required/triggers.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gate-ga-7kh81mei-c8l5_53f/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-required/windows-lane-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-windows-lane.sh; exit=0; log=/var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-required/windows-lane.log
- windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner;   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
windows_reach: none, computed separately for both native Windows jobs.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

This PASS permits PR publication. CI / required and each deferred job must be green on the exact PR head before MPR merges.

Native load evidence:
- criterion3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=540 wait_timed_out=0 run_start_load=14.68 run_max_load=25.87 run_mean_load=17.63 run_readings=7 wait_first_load=35.55 wait_max_load=35.63 wait_mean_load=23.70 wait_readings=19 read_errors=0
- native-ac-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=180 wait_timed_out=0 run_start_load=14.11 run_max_load=24.86 run_mean_load=20.12 run_readings=15 wait_first_load=20.48 wait_max_load=20.48 wait_mean_load=16.94 wait_readings=8 read_errors=0 prewait_s=180
- ci-required/windows-lane.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.67 run_max_load=14.55 run_mean_load=13.91 run_readings=3 wait_first_load=11.67 wait_max_load=11.67 wait_mean_load=11.67 wait_readings=2 read_errors=0 prewait_s=0
load_threshold: 15
load_waited_seconds: 540
load_wait_timed_out: 0
run_start_load: 14.68
run_max_load: 25.87
run_mean_load: 17.63
run_readings: 7

test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 58.57854461669922, "waited_seconds": 0.001}

diff_tests_executed by name:
TestDoRigAdd_AddsRigWhenControllerUnavailable PASS (integration-packages-e4245ddf6a37030111bc.log, unit-423c6a0da1986b90e9a7.log)
  TestDoRigAdd_WaitsForControllerReload PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-637c74c578a2b17fc654.log)

test_cmd: env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-7kh81mei /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gate-ga-7kh81mei-c8l5_53f/merge --base 131d1c9ea526fb91240b586c28e38b41384e0014 --bead-json /var/tmp/gate-ga-7kh81mei-c8l5_53f/bead.json --owned /var/tmp/gate-ga-7kh81mei-c8l5_53f/owned-tests.tsv --run-dir /var/tmp/gate-ga-7kh81mei-c8l5_53f/criterion3-run/evidence
Full context.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/context.json; SHA256 81046251804130022f64a5a79c91dfe6686ec74860d3252a539b835f2a5ad182
Full c6-result.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/c6-result.json; SHA256 af80be4ef33afa5988f57a7f7554c5218e86cfe0ef6e8032152c727d85bfc308
Full fast-result.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/fast-result.json; SHA256 9d5e94ec5ef30373d59b7ef39f2d01a021993df32fafb9eb60e5aa4b65a68b08
Full criterion3-run/evidence/execution.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/criterion3-run/evidence/execution.json; SHA256 6701987ae13e5e2899a09c5156fc347fd98e197e35d86dbab22c31259d311423
Full remote-phases-independent-audit.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/remote-phases-independent-audit.json; SHA256 98fee62a2ce049ccd4eebbec86f0aca82f5be1a971551b9b3f26dc2e16867ef8
Full complete-skip-audit-plan.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/complete-skip-audit-plan.json; SHA256 a388c8646704884dd50536f5b151d945ab08ba0331390b9d18d6b61684d2a049
Full gated-owned-explain.txt retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/gated-owned-explain.txt; SHA256 b770e06d830bca6a2dc9f26441efede0c486df1c7c1f113b5f104dcb634c4486
Full skip-shared-source-resolution.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/skip-shared-source-resolution.json; SHA256 6818f26267798085233d9f5187f48ae02a3e7e1573a4b75ac8749734609b0b44
Full independent-skip-reason-audit.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/independent-skip-reason-audit.json; SHA256 f0c0383f06a663eaaf9abf14d13406d270c57d5481feb285a59fae4d6072352e
Full native-ac-result.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/native-ac-result.json; SHA256 d263cdee02c4930ec2ca9f7c03e49f6522f15e848b285684ef8f2b2c9c473807
Full native-ac.bep.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/native-ac.bep.json; SHA256 5ec15cb4cf0f4ed8b8fadcce956c6fd55fed3dd05a4e575f8687e290004173d8
Full ci-required/accounting.json retained: /var/tmp/gate-ga-7kh81mei-c8l5_53f/ci-required/accounting.json; SHA256 9b338c566ff1f3e12d368f81a7dfa1784bef092d38b57786761a3771d5ad4cb9
Permanent evidence bundle: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7kh81mei/gate-pass-20261116T211224Z; manifest.json hashes all retained files.
