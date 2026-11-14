# Doctor: reclaim nested worktrees despite repository-global stashes

**Verdict:** **PASS**

bead: ga-76grt0lk; build: ga-005o3m; review: ga-6atoklwc.
DEPLOY_MODE: remote; push_remote: fork; BASE_REF: origin/main.
reviewed_commit: 2df7943a0e2c985cf11c5f4479c29904be7dd421
pinned_base: dc5486b999b3187b85a1e4c12c799580ad299d60
canonical_merge_head: ab9e32af587d1d232f91118a4c49119b980549f3
canonical_merge_tree: a133d5a652f42deaa365837c85d6631d0e2f528d
merge_state: clean (origin/main@dc5486b999b3187b85a1e4c12c799580ad299d60)
gate_base_kind: current-tip
merge_validation: exact source plus pinned base gated; later base movement is settled and checked by the merge authority.
reviewer_tests_verified: bab283a5cf3ef2db9a765ec083e6e9b528f66899 -> 2df7943a0e2c985cf11c5f4479c29904be7dd421 files=internal/doctor/checks_semantic_test.go (reviewer-added-tests.sh check).

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Review ga-6atoklwc binds the exact source. Reviewer-added tests are one additive, test-only commit; helper check independently confirms the recorded files and parent. |
| 2 | Acceptance | PASS. Doctor no longer refuses a clean fully pushed nested worktree because refs/stash exists elsewhere in the common repository. Dirty/unpushed, probe-error and repository checks remain; Fix revalidates before removal. Fresh real-git regression verifies a stash made inside the removed worktree survives and a dirty sibling remains. The config comment and generated reference/schema describe this same change and pass fresh generation checks. Daemon parent repair b244d69f5979e316d86b4d82cbe9ca91f2312e90 is an ancestor of source and pinned main. See acceptance-final.json. |
| 3 | Tests and required CI | PASS. Full standard five-phase Bazel suite plus16 mandatory native jobs; all3 owned tests PASS by name; all6 required CI leaf accounts present below. No waiver or attributed failure. |
| 4 | Open HIGH findings | PASS. Zero unresolved HIGH review/security findings. |
| 5 | Clean final tree | PASS. Canonical head/tree re-resolved and scratch has no tracked or untracked changes before rendering. Gate record is the only deployer addition to reviewed source. |
| 6 | Merge and build/vet | PASS. Exact clean canonical merge matches independent git merge-tree at pinned main. Whole-tree build and nogo/vet both finish SUCCESS; normal hooks active. |
| 7 | Single theme | PASS. Doctor nested-worktree reclaim safety and its generated config wording; no independent feature. |

## Test evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/merge-retry-sme_htxg --base dc5486b999b3187b85a1e4c12c799580ad299d60 --bead-json /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/bead.json --owned /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/owned.tsv --run-dir /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence
test_cmd_scope: full-suite
heavy_mode: none
test_exec_mode: remote-exec
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI job/matrix/timeout/required-check configuration changed)
test_counts: 154408 PASS,0 FAIL,606 SKIP (top-level result lines across557 logs, repeated tests count per job)
test_log_dir: /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence/remote/logs

GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence/remote/logs logs=557 logs_with_results=489
test_counts: PASS=154408 FAIL=0 SKIP=606
  (top-level result lines, column 0, summed over the 557 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+56 more)
diff_tests_executed:
  TestNestedWorktreePruneCheck_ClassifiesSafeAndUnsafe PASS (unit-7bce503b7c011c7475f3.log)
  TestNestedWorktreePruneCheck_ProbeErrorsAreUnsafe PASS (unit-7bce503b7c011c7475f3.log)
  TestNestedWorktreePruneCheck_RepoGlobalStashDoesNotBlockReclaim PASS (unit-7bce503b7c011c7475f3.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name


541 runtime result actions: unit415, acceptance25, integration-packages86, full integration14, separate registered latency1. All remote, uncached locally/remotely, attempt1, terminal verdicts and phase windows valid. Both latency tests PASS after clear worker probe. owned_topup=[] (no owned MISSING test); required native floor complete with16 exit0 jobs including beadstest, Darwin compile and runner self-tests. Native doctor top-up actually PASSes all3 CustomTypesCheck tests skipped on farm for missing bd.

policy_lane: PASS; fresh pinned gate-base format/policy lane13 result rows and whole-tree C6 nogo/vet.
drift_lane: PASS; separate fresh-tree make bazel-sync plus empty tracked/untracked drift,7 generated results.
OpenAPI compatibility: PASS in a separate fresh tree,1 result.
Build/policy caches are permitted and give no runtime credit. ICU74 provenance: icu74.txt. The isolation wrapper reports PASS-THROUGH on this rig; no clean-environment claim comes from its banner. Remote environments are declared by Bazel; native floor explicitly scrubs GC_/BD_/BEADS_/DOLT_ selectors and uses the already installed pinned bd.

Compact execution record (all scalar verdict/count/resource fields preserved; large arrays are pointers):

{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":69.09196853637695,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13444.36,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":67.144,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=16.51 load5=21.47 load15=15.94 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 16.54 20.74 15.92 6/1088 53935"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40524}},"log_dir":"/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":502,"environment-absence":90},"top_level_counts_by_reason_class":{"other":244,"environment-absence":43},"tests":{"elided_array":{"items":592,"bytes":222752}}},"test_cmd":{"elided_array":{"items":5,"bytes":2149}},"test_counts":{"PASS":154408,"FAIL":0,"SKIP":606},"diff_tests_executed":"diff_tests_executed:\n  TestNestedWorktreePruneCheck_ClassifiesSafeAndUnsafe PASS (unit-7bce503b7c011c7475f3.log)\n  TestNestedWorktreePruneCheck_ProbeErrorsAreUnsafe PASS (unit-7bce503b7c011c7475f3.log)\n  TestNestedWorktreePruneCheck_RepoGlobalStashDoesNotBlockReclaim PASS (unit-7bce503b7c011c7475f3.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}

Full full execution record retained: 5943 lines; /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/criterion3-run/evidence/execution.json; SHA256 ef36de929622b96c5c9409e8d6900fdf4e981b9ad39238aaea05a380f019d7d5.

## Skip evidence and shared hunks

skip_inventory: farm502 other/90 environment-absence rows. Final raw farm/native inventory664 rows including subtests,72 native,309 unique sites: every site line_in_hunk=no and UNCHANGED; zero unresolved/diff-owned skips. Full test/site/source-line/reason/log evidence stays in skip-rows-final.json and skip-site-proof-final.txt.

Remaining skips are unchanged platform/capability guards, explicit live-provider or fixture inputs, intentional negative provider conformance and unsupported storage-shape cases. None is credited as PASS. Every environment-absence root gets the mandatory native attempt. Three doctor missing-bd skips are independently identical at pinned base and PASS in the native floor. Broader importer SKIPs are also compared under the same target/tags at pinned base: integration-full14/14, cmd/gc integration66/66, sim4/4 and unit187/187. Native cmd/gc36/36 matches the same native command on base. Official compare-skips returns all pre-existing, with no new/reason-changed/diff-owned/unjudged skip.

SHARED-HUNKS internal/doctor/checks_semantic_test.go(4) are each answered: a fakeGitWorktree comment, removal of its stash bookkeeping fields, removal of its stash fake method and the BrokenRepoGate comment. No init/TestMain/build guard changed. The three CustomTypesCheck skips do not use that fake and stop at an unchanged bd-availability guard. All affected-package/importer/native base comparisons and actual commands remain retained; importer closures are mechanically computed with go list -test -deps. These focused supplements never replace the full suite.

Full final skip/site inventory retained: 6642 lines; /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/skip-rows-final.json; SHA256 02885dc16046a715e7e5e947c4d76ac6403eaa466dd6758d99e87786e70df5be.
Full final skip site proof retained: 312 lines; /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/skip-site-proof-final.txt; SHA256 b566454db9399b9fcb364eac39add4ac6fd3661cba443f868e2563c7f587f632.

## Resource observations

The ordinary native commands keep threshold15/maxwait1800 bounded soft waits, including timeouts; no synthetic load or widened threshold. Wait observations and actual run observations are separate:

native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=82.12 run_max_load=85.12 run_mean_load=80.19 run_readings=9 wait_first_load=96.09 wait_max_load=96.09 wait_mean_load=68.83 wait_readings=61 read_errors=0
native-skip-base: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=25.07 run_max_load=25.15 run_mean_load=24.91 run_readings=2 wait_first_load=110.61 wait_max_load=113.47 wait_mean_load=52.12 wait_readings=62 read_errors=0 prewait_s=1801
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1740 wait_timed_out=0 run_start_load=14.89 run_max_load=21.16 run_mean_load=18.39 run_readings=4 wait_first_load=111.46 wait_max_load=116.62 wait_mean_load=73.42 wait_readings=60 read_errors=0 prewait_s=1740
pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=960 wait_timed_out=0 run_start_load=14.30 run_max_load=23.76 run_mean_load=18.27 run_readings=4 wait_first_load=25.50 wait_max_load=30.10 wait_mean_load=23.51 wait_readings=34 read_errors=0 prewait_s=960
native-floor normalized: {"load_threshold": "15", "load_wait_timed_out": "1", "load_waited_seconds": "1801", "run_max_load": "85.12", "run_mean_load": "80.19", "run_readings": "9", "run_start_load": "82.12"}
native-skip-base normalized: {"load_threshold": "15", "load_wait_timed_out": "1", "load_waited_seconds": "1801", "run_max_load": "25.15", "run_mean_load": "24.91", "run_readings": "2", "run_start_load": "25.07"}
windows-lane normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "1740", "run_max_load": "21.16", "run_mean_load": "18.39", "run_readings": "4", "run_start_load": "14.89"}
pack-gate normalized: {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "960", "run_max_load": "23.76", "run_mean_load": "18.27", "run_readings": "4", "run_start_load": "14.30"}

## Required CI leaf accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/merge-retry-sme_htxg dc5486b999b3187b85a1e4c12c799580ad299d60 2df7943a0e2c985cf11c5f4479c29904be7dd421 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
leaf_count: 6 LEAF jobs need a status (the check aggregator rolls them up).

ci_required_accounting:
runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit=0; log=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/runner-policy.log
changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/merge-retry-sme_htxg dc5486b999b3187b85a1e4c12c799580ad299d60 2df7943a0e2c985cf11c5f4479c29904be7dd421 fork; exit=0; log=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/closure.txt
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"reach": "none", "reach_stderr": "/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/credential-windows-reach.stderr", "reach_stdout": "/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/credential-windows-reach.stdout", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI", "tool_row": "  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"}
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"reach": "none", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI"}
windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/merge-retry-sme_htxg -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/windows-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-windows-lane.sh; exit=0; log=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/windows-lane-output.log
pack-gate | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/merge-retry-sme_htxg -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/pack-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-pack-gate.sh; exit=0; log=/var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k/ci-required-v2/pack-gate-output.log

windows_reach: {"credential-provider-windows": "none", "packages": ["./internal/credentialprovider", "./internal/genshell/..."], "windows-lane-test": "none"}
deferred_to_pr_ci: windows-lane-test (non-linux-runner), credential-provider-windows (non-linux-runner)
not_covered: none

The exact merged workflow's Windows/Darwin cross-build and live pack-gate commands completed serially in separate admitted/load-gated fresh trees. Native Windows jobs require their own runner and stay deferred; both independent Windows closures have empty changed-file reach. CI / required and every deferred job must be green on the current head before merge; PASS here authorizes PR-open only. MPR owns merge.

The prior latency failure is retained under the prior_run pointer in context.json and is not reused as a pass. Actual fixed-main repair dc5486b999b3187b85a1e4c12c799580ad299d60 landed before this fresh run. Tracker ga-y39d8tx stays open until both named gates pass; this record does not close it.

Evidence root: /var/tmp/gc-gate-ga-76grt0lk-post9162-gjl4qa8k
UTC: 2026-11-14T08:55:05.293873+00:00
