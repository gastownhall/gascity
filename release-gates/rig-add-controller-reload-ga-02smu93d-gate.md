**Verdict:** **PASS**

## Rig-add configuration visibility

Bead: ga-02smu93d; build: ga-04c9i0cb; review: ga-2eybcmp7.
This record supersedes release-gates/rig-add-reload-ga-7kh81mei-gate.md (an earlier head and two owned tests).
Reviewed deploy source: 645d020d6a58629884f03c218eca5d1e16f9e409
Validation merge: 857d7f5cc0994565b13d00dbba5b079cd5bf2109; tree: 8acc6aaee7d070e913697787c3f0817fd0137b61.
gate_base: 36447913f2aecd88512cc086d59c806c59e486e1
gate_base_kind: current-tip
merge_state: clean (origin/main@36447913f2aecd88512cc086d59c806c59e486e1)
merge_validation: canonical merge result built and vetted here; merge authority retains merge-time validation.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS re-read at the exact resolved source; all six source-range commits reviewed; no substituted branch tip. |
| 2 | PASS | Three rig-add roots PASS by name; native federation AC3 runs under two-CPU affinity with R/I/G each3/3, no fail/skip. The builder-recorded starved base failed every iteration; this gate independently repeats that discriminating condition and count; deterministic unit coverage pins the waiting request, unavailable controller and incomplete-reload warning. Complexity has zero growth. |
| 3 | PASS | Fresh complete evidence-2 alone: all five phases, native floor, skip controls and exact CI-leaf accounting below. |
| 4 | PASS | No unresolved HIGH review findings. The recorded non-blocking gaps remain unchanged. |
| 5 | PASS | Source and canonical trees clean; final publication must be a gate-only commit with a clean working tree. |
| 6 | PASS | Whole-tree Bazel build and vet/nogo rc0 on the independently verified canonical merge; normal hooks active, ICU74 OK. |
| 7 | PASS | One theme: waiting for rig-add reload visibility, its warning, regression tests and complexity extraction. |

test_cmd: gate-criterion3.sh --repo /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/merge.0y_37qb5 --base 36447913f2aecd88512cc086d59c806c59e486e1 --bead-json /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/bead.json --owned /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/owned-tests.tsv --run-dir /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/criterion3-run/evidence-2; launched durably with GC_GATE_REMOTE_ENABLED=1, remote floor25GiB/cap2. Phase commands force nocache_test_results, noremote_local_fallback, keep_going, ci flags and verbose by-name evidence; full argv/BEPs retained.
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: 541/541 remote results;0 cached locally/remotely,0 other strategy,0 retried; all five windows valid (full data in independent audit).
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 67.34693908691406, "waited_seconds": 0.001}
farm_test_seconds: 13653.529
skip_inventory: retained remote/skip-inventory.json and complete-skip-audit-plan.json, with site/reason accounting below.
owned_topup: 0; all owned roots passed in the main phases.
floor_topup: complete,16 steps exit0; remote/floor-topup.json retained.
latency_leg: measured after clear farm probe, exit0; remote/latency-leg.json retained.
test_counts: 155195 PASS / 0 FAIL / 613 SKIP (top-level per-job Go results, including native floor; repeated roots counted per job).
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/criterion3-run/evidence-2/remote/logs
diff_tests_executed: all3 owned roots below PASS, including reported subtests.
waiver_ref: none
failure_attribution: none in the final fresh run
heavy_mode: none, verified classifier
policy_lane: pinned format/lint/nogo and policy guards PASS
drift_lane: BUILD synchronization, empty tracked/untracked diff, specs/schema/client/dashboard/lockfile/docs drift checks PASS
ci_lane_run: n/a (no CI-config change)
complexity: base/merge checks both rc2 with 251 diagnostic rows, none worse; diff improves doRigAddWithResult 26 to22 and has no new/regressed/deleted row; baseline JSON unchanged.

| Phase | Test results | Evidence |
|---|---:|---|
| unit | 415 | PASS, fresh remote attempt1 |
| acceptance | 25 | PASS, fresh remote attempt1 |
| integration-packages | 86 | PASS, fresh remote attempt1 |
| integration-full | 14 | PASS, fresh remote attempt1 |
| integration-latency | 1 | PASS, fresh remote attempt1 |

All 541 remote results have terminal PASS, attempt1, strategy remote, neither cached flag and valid phase windows. Latency ran after a clear farm probe. All16 source native-floor steps and all16 fresh pinned-base native controls exit0; four focused remote base-comparison phases exit0.

- TestDoRigAdd_AddsRigWhenControllerUnavailable: PASS
- TestDoRigAdd_WaitsForControllerReload: PASS
- TestDoRigAdd_WarnsWhenControllerReloadIncomplete: PASS

Focused AC3 supplement: CPU pair 0,1, three iterations, parent3/3 and R/I/G each3/3,0 FAIL/0 SKIP, one uncached local Bazel result. It carries no full-suite credit. Both suspended-city and suspended-rig quiescence tests also PASS by name with landed #9217 fix in the frozen base.

skip_justification: 671 test/subtest SKIP events at 325 unchanged sites; no owned skip. All raw reasons/site mappings are resolved, including30 conformance helper locations; no environment class is missed. No shared/helper test hunk changes. All cmd/gc literal base comparisons pass. The 2 extra filesystem comparator gaps keep their original reason-changed result: random path names differ while both trees observe EXDEV or distinct-case paths not naming one directory. Both guard files/sites are unchanged and both test-binary import closures exclude every changed production input. No raw normalization, comparator PASS or waiver is claimed.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/merge.0y_37qb5 36447913f2aecd88512cc086d59c806c59e486e1 645d020d6a58629884f03c218eca5d1e16f9e409 fork; 6 LEAF jobs need a status; the rest are rolled up through their needs.; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; derived from the canonical tree for fork publication.

| CI leaf | Status | Exit |
|---|---|---:|
| runner-policy | LOCAL-PASS | 0 |
| changes | LOCAL-PASS | 0 |
| credential-provider-windows | NOT-TRIGGERED | n/a |
| pack-gate | NOT-TRIGGERED | n/a |
| windows-lane | LOCAL-PASS | 0 |
| windows-lane-test | DEFERRED-TO-PR-CI (non-linux-runner) | n/a |

windows_reach: {"credential-provider-windows": "none", "windows-lane-test": "none"}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Deferred runtime jobs, CI / required and the repository's required checks must be green on the published head before merge. Gate PASS authorizes publication only.

load_threshold / wait / observed run metrics:
source native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.34 run_max_load=13.01 run_mean_load=12.24 run_readings=6 wait_first_load=8.34 wait_max_load=8.34 wait_mean_load=8.34 wait_readings=1 read_errors=0
base native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.39 run_max_load=17.82 run_mean_load=16.98 run_readings=7 wait_first_load=14.39 wait_max_load=14.39 wait_mean_load=14.39 wait_readings=2 read_errors=0 prewait_s=0
controlled AC3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.26 run_max_load=17.82 run_mean_load=15.81 run_readings=16 wait_first_load=12.26 wait_max_load=12.26 wait_mean_load=12.26 wait_readings=2 read_errors=0 prewait_s=0
CI Windows compilation: LOAD_GATE_SUMMARY threshold=15 waited_seconds=660 wait_timed_out=0 run_start_load=14.75 run_max_load=16.91 run_mean_load=16.33 run_readings=3 wait_first_load=16.86 wait_max_load=24.26 wait_mean_load=19.34 wait_readings=24 read_errors=0 prewait_s=660
Remote phases use remote-class admission without a host-load wait. Each local run retains its measured soft-wait telemetry above and in its own logs.

### Abandoned first criterion-3 attempt

Per mayor process ruling gcg-11079681, the first run is retained as ABANDONED and contributes no evidence or failure_attribution to this verdict. It ended unit415 PASS, acceptance25 PASS, integration-packages85 PASS/1 FAIL (TestACPOrphanReapedAfterProviderRestart); full integration, latency and floor did not run. Raw partial Go counts154968 PASS/1 FAIL/585 SKIP and execution.json exit1/criterion3_valid=false remain at /var/tmp/gate-ga-02smu93d-fresh-10uan_wc/criterion3-run/evidence. All four3a screening clauses cleared: unchanged file/site, tracker ga-6l9ls252, parent and fakeacp-child import closures excluding all changed production inputs, no package overlap. The screen added no sighting; no joined continuation was used. The fresh complete run above is judged alone. The condition tracker stays open until its fix lands.

Evidence bundle: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z
- final audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/final-evidence-audit.json; SHA256 de0544a30a4b135b59f3e50434a7cc132ab5ebb052aed6a413d43e279aa3ba67
- independent BEP/artifact audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/remote-phases-independent-audit.json; SHA256 c27b3577286d59dcb303b3647334c8362fc413f0dbc6772f38a5b900850ecd7f
- skip sites/reasons: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/complete-skip-audit-plan.json; SHA256 ef421d808c4a5ce21ac35cf3d39735decac719ee6901474f36d43db82469f18a
- shared/helper reach: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/shared-helper-audit.json; SHA256 3ae187dd20af38f39d429a32a9093a681ba26fa5655e79f8b6204ab5fa98f6aa
- filesystem gap proofs: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/filesystem-skip-gap-proofs.json; SHA256 462fd39205fca690e40210ea45c39dfb5c0e7d69d8b8fe54488a6ff8d2663d2d
- controlled AC3: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/native-ac-result.json; SHA256 46e9aceb8fa271556c066b0d641f11065226c9147eac8efdbf44fd68521bd699
- exact CI commands/results: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/ci-required/accounting.json; SHA256 529fb1b114d6c7f93b1e86be55bafcc61a44b538c12ca768a3de08e2adf04367
- abandoned-run screening: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-02smu93d/gate-pass-20261117T052952Z/failure/abandoned-screen.json; SHA256 4404b847e35259dff5e97e7dcbe8f2ea3b4c11d676a4461c4d8b884299a55b66

Landing exit: after this replacement for PR#9233 merges, mayor/MPR closes tracker ga-cdv9w80i with landing evidence and wakes #9186 deploy ga-7o9slun2. No tracker is closed before landing.
