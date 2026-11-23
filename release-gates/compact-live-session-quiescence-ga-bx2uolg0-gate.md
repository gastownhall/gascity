# compact-live-session-quiescence-ga-bx2uolg0 — release gate

**Verdict:** **PASS**

Deploy bead: `ga-bx2uolg0`; mode: remote; base branch: main.
Reviewed source: `281d38da6ce299390e38b236ecc94c51d329a7e5`.
Pinned base: `ee6f9ccc4eb38a09ed0ea3e52fd8dfa59d0497dd`; gate_base_kind: current-tip.
Tested merge head: `b96753840ae26b4a9a8609fc1dd19a2dad1bb18e`; tree: `4734f8df77dd7cb2c6ebaf0b80f629b9e0e79997`.
merge_state: clean (origin/main@ee6f9ccc4eb38a09ed0ea3e52fd8dfa59d0497dd; files: none).
merge_validation: MPR merge authority; Gas City main merge queue checks the merged result. A later head change requires a new gate; see deployer/docs/merge-time-guarantee.md, Gap 1.

| # | Result | Evidence |
|---|---|---|
| 1 — Review PASS | PASS | Live closed review verdict pass; metadata.commit resolves to deploy source. |
| 2 — Acceptance criteria | PASS | Code and by-name test evidence below; live observation explicitly deferred where named. |
| 3 — Full suite, policy, skips and CI | PASS | Five fresh uncached remote phases, native floor, owned tests by name, full SKIP baseline proofs, and all six CI leaf accounts verified. |
| 4 — Review severity | PASS | Zero unresolved HIGH findings. |
| 5 — Final branch | PASS | Isolated deploy/ga-bx2uolg0-gate is cut at the exact reviewed source; only this gate record is added in the commit. Clean working tree is checked after that commit and before push. |
| 6 — Merge build and vet | PASS | Canonical clean merge build and vet BEPs both overallSuccess=true; normal hooks active. |
| 7 — Single theme | PASS | Protect explicit Dolt compact GC from connected sessions;7 files are implementation, tests, help, docs, and BUILD wiring for that one behavior. |

Acceptance checks:

- Both run_full_gc and run_bare_gc call gc_live_session_gate before live_gate_arm and DOLT_GC; full-after-flatten and pending-GC retry use run_full_gc.
- live_session_count bounds the processlist query at2s, returns nonzero on errors/timeouts/non-numeric output; failed confirmation skips GC.
- --force defaults0, is set only by explicit CLI flag, bypasses session-count/probe failures with a logged reason.
- Nearby code explicitly states point-in-time/TOCTOU limitation and no lock; wording about retry cadence is the existing nonblocking followup ga-6sf0uk4f.
- Eight owned quiescence tests actually PASS by name in fresh unit and integration-package output.

Help and troubleshooting additions match skip/fail-closed/force behavior; docsync PASS in both FAST and fresh unit lane. No changed docs link/anchor is missing.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (no Gas City waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-bx2uolg0 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate-ga-bx2uolg0-ecx8k1fo/merge --base ee6f9ccc4eb38a09ed0ea3e52fd8dfa59d0497dd --bead-json /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/bead-criterion3.json --owned /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/owned.tsv --run-dir /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence -- python3 /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/c3-local-bazel.py`.
Five phases: make -k check; acceptance and solo tests; all integration packages; full integration minus gate-latency-tests.txt; only that latency inventory under a clear farm probe.
All phases force --nocache_test_results and --noremote_local_fallback, first attempts only. No cached test result is credited. test_exec_mode is derived from BEP: remote-exec.
test_log_dir: `/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence/remote/logs`; full per-job logs retained.

```text
test_counts: PASS=154775 FAIL=0 SKIP=607
  (top-level result lines, column 0, summed over the 557 logs; a test that several jobs run counts once per job; sub-tests are not counted)
```

diff_tests_executed:

- TestCompactScriptBareGCProbesBeforeGCWhenQuiescent PASS
- TestCompactScriptBareGCSkipsDatabaseWithLiveSessions PASS
- TestCompactScriptDryRunDoesNotProbeLiveSessions PASS
- TestCompactScriptFlattenDefersFullGCAsPendingWhenLiveSessions PASS
- TestCompactScriptForceOverridesLiveSessionGate PASS
- TestCompactScriptGCGateFailsClosedWhenProbeFails PASS
- TestCompactScriptGCOnlySkipsDatabaseWithLiveSessions PASS
- TestCompactScriptPendingGCRetrySkipsAndKeepsMarker PASS

FAST lane: policy/lint, formatting, BUILD drift and generated-file drift all passed before the suite, each in a fresh view. Complexity zero-growth form (ii): base_rc=2, merge_rc=2, byte-identical diagnostics, diff_rc=0; no threshold or policy relaxation.
ci_lane_run: n/a (no CI-config change in this diff).

skip_justification: Every skip is non-owned by test; every printed site is outside a diff hunk. Test-binary import closures were computed for unit/integration/acceptance_a, naming every shared/helper hunk. Every reached original key matches a pinned-base same-name/same-reason comparison in its actual farm tier; native herdr additionally matches the canonical native floor. Unreached keys have no changed helper/package in those closures. Exact original reasons remain in all_sites rows. No owned SKIP is accepted.
All 383 shared-reached original keys have farm-tier baseline proof; all310 printed sites unchanged. Native herdr same-command proof is complete; native SimCoverage same-command proof is complete. Supplemental comparisons are focused attribution evidence, never a substitute for the full suite.

Load observations:

- full-chain: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.95 run_max_load=96.16 run_mean_load=50.82 run_readings=111 wait_first_load=9.95 wait_max_load=9.95 wait_mean_load=9.95 wait_readings=1 read_errors=0`.
- native floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=36.32 run_max_load=46.83 run_mean_load=37.67 run_readings=9 wait_first_load=32.65 wait_max_load=95.96 wait_mean_load=66.29 wait_readings=61 read_errors=0`.

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate-ga-bx2uolg0-ecx8k1fo/merge ee6f9ccc4eb38a09ed0ea3e52fd8dfa59d0497dd 281d38da6ce299390e38b236ecc94c51d329a7e5 fork`; .github/workflows/ci.yml blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs.

ci_required_accounting:

- changes | LOCAL-PASS | {"command":["python3","/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py","/var/tmp/gc-merge-validate-ga-bx2uolg0-ecx8k1fo/merge","ee6f9ccc4eb38a09ed0ea3e52fd8dfa59d0497dd","281d38da6ce299390e38b236ecc94c51d329a7e5","fork"],"rc":0,"log":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/closure.txt"}
- runner-policy | LOCAL-PASS | {"command":["python3",".github/workflows/scripts/runner_policy.py"],"env":{"EVENT_NAME":"pull_request","PR_AUTHOR":"quad341","FORCE_BLACKSMITH":"","GITHUB_OUTPUT":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/runner-policy-final.out"},"rc":0,"log":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/runner-policy-final.log"}
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | {"command":"bash /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/windows-lane.sh","rc":0,"log":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/windows-lane/units/gate.w1.stdout.log","wrapper_log":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/windows-lane/units/gate.w1.stderr.log","load":"LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=48.24 run_max_load=47.33 run_mean_load=43.88 run_readings=5 wait_first_load=68.52 wait_max_load=87.11 wait_mean_load=54.37 wait_readings=62 read_errors=0 prewait_s=1801"}
- windows-lane-test | DEFERRED-TO-PR-CI | {"reason":"non-linux-runner","windows_reach":"none","own_reach_record":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/windows-lane-test-reach.json"}

windows_reach: none for every deferred non-Linux job; each uses its own import-closure proof.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open clearance never replaces green CI / required or the deferred jobs on the PR head.

Execution metadata (large inventories elided; retained pointers below):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":71.22991180419922,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13936.432,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":104.996,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=83.90 load5=93.65 load15=50.13 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=40s load1=43.03 load5=81.91 load15=48.01 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.28 75.77 46.88 22/1143 54651"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39570}},"log_dir":"/var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":503,"environment-absence":90},"top_level_counts_by_reason_class":{"other":245,"environment-absence":43},"tests":{"elided_array":{"items":593,"bytes":217968}}},"test_cmd":{"elided_array":{"items":5,"bytes":2104}},"test_counts":{"PASS":154775,"FAIL":0,"SKIP":607},"diff_tests_executed":"diff_tests_executed:\n  TestCompactScriptBareGCProbesBeforeGCWhenQuiescent PASS (integration-packages-130aa78402132a9cb052.log, unit-9d67ff2f074bc8f49779.log)\n  TestCompactScriptBareGCSkipsDatabaseWithLiveSessions PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-98fce3c243b1e6971368.log)\n  TestCompactScriptDryRunDoesNotProbeLiveSessions PASS (integration-packages-130aa78402132a9cb052.log, unit-9d67ff2f074bc8f49779.log)\n  TestCompactScriptFlattenDefersFullGCAsPendingWhenLiveSessions PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-da81bca7c80446c5af28.log)\n  TestCompactScriptForceOverridesLiveSessionGate PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-98fce3c243b1e6971368.log)\n  TestCompactScriptGCGateFailsClosedWhenProbeFails PASS (integration-packages-a9e12e81511731ef69dd.log, unit-6b47d0615f42f4ae2b8d.log)\n  TestCompactScriptGCOnlySkipsDatabaseWithLiveSessions PASS (integration-packages-2c974992a3e988636c94.log, unit-89d44f035a0dc6a9b6c9.log)\n  TestCompactScriptPendingGCRetrySkipsAndKeepsMarker PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-ff6413fa61a58d5e02ec.log)\nGATE_TEST_EVIDENCE_RESULT ok: 8 diff-owned tests verified by name\n"}
```

Full execution retained: 5952 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence/execution.json; SHA256 2df4e295d56e201320f6f9e4dd2095f3a24f68309e7e1045e1fc24ed021a31ef.
Full skip inventory retained: 4746 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-run/evidence/remote/skip-inventory.json; SHA256 81db35f3a6895c6761ccd5d177e7ad7345e53e0918a51454dd83cccef6257081.
Full native skip inventory retained: 578 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/criterion3-native-floor-skip-inventory.json; SHA256 072d40b83ac69a416a3a0db03b4e7621276a45fad812d7938e4a269fe5191c02.
Full skip proof closure retained: 133 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/skip-closure-audit.json; SHA256 905bedb72f2ab357d89f27ec6802e80d8446e1a880d8625cc8305095403b719b.
Full printed skip sites retained: 5304 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/all-skip-site-audit.json; SHA256 f39ee8c64e63b5f0636213b66f89aee7aca021daac7de59afdad58455f90944c.
Full import reach retained: 5966 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/all-skip-comparison-coverage.json; SHA256 d325d25ca9e3d73fffb6a2cfe6bad802862f23a8a5f947d996a1c3a871f893df.
Full CI job accounting retained: 82 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/ci-required/accounting.json; SHA256 d6dd3edf8b3abecf4534e4836ef32e1af48c7f7a93a05bca4dbd998c3e95a7e9.
Full FAST lanes retained: 167 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/fast-result.json; SHA256 c5c83c3229c3c23937c42e541fc87907ab1c4918c3fde07314ca5b39a0fd9ce2.
Full merge build and vet retained: 25 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/c6-result.json; SHA256 ad6c16d4f585b0c5bc6f79e6b4a8acbddb63defad828774558d1f0a210f9ec28.
Full evidence hash manifest retained: 1698 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/publication-evidence-manifest.sha256; SHA256 7da3cf5317f30072c43f6916131c5a473b958897477c2e40b6fef0cae397e752.

Publication verification 2026-11-16T02:57:52.209344+00:00:

policy_lane: bash /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/fast-policy.sh — PASS; drift_lane: bash /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/fast-drift.sh and bash /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/fast-generated.sh — PASS. Actual fresh-tree, gate-base, admission, load and isolation argv are retained in fast-result.json.
failure_attribution: none for runtime tests. No owned SKIP/FAIL, no retry/cache credit, no waiver.
reviewer_tests_verified: n/a (reviewer added no tests).
Native SimCoverage baseline comparison: same pinned base, unmodified canonical floor command, same name/reason; compare_rc=0, ALL PRE-EXISTING. Full native baseline proof retained: 45 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/native-skip-sim-base-result.json; SHA256 078fbed2c158f362c140861b16dfbfd877cfe62ccee7daf9a64e0e86bee0e74c.
Full publication audit retained: 99 lines; /var/tmp/gc-gate-ga-bx2uolg0-rs3csk58/publication-evidence-audit.json; SHA256 0156155a2680af036cb52e9548ee1d85b6851ebd20e430a6b8f8ea600bd51666.
The earlier manifest was captured while the native baseline was pending; its only changed entry was skip-closure-audit.json when that proof completed. All 1657 earlier immutable artifact hashes matched; the final manifest includes the completed native baseline logs and current summary. Earlier manifest remains retained.
