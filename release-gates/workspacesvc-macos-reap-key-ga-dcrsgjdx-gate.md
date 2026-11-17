# macOS reap-registry key — draft release gate

**Verdict:** **PENDING** — Linux gate PASS; actual Mac runtime criterion pending.

This intermediate draft record is authorized by mayor gcg-11064321. It grants no deploy clearance or merge-request. The final record will use the required binary PASS/FAIL verdict after actual Mac evidence is available.

- Bead: ga-dcrsgjdx; build ga-b50d6ctf; review ga-ae19oqn1.
- Reviewed source: 20ac9e490fd587ce74d8f0ca1e209df341873de1 (resolved commit object).
- Frozen base: 36447913f2aecd88512cc086d59c806c59e486e1 (origin/main); validation merge: 5f565c9de97303f37fe3e5ce59a3b30387c10588; tree: f156604a79bea9cde7d588cc14552a2450e39752.
- merge_state: clean (origin/main@36447913f2aecd88512cc086d59c806c59e486e1)
- gate_base_kind: current-tip (frozen at claim); merge_validation: merge authority validates the final merge, per docs/merge-time-guarantee.md.
- Feature: case-fold macOS service-root reap keys; three files in internal/workspacesvc. No source rebase or branch-tip substitution.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS | PASS | Review ga-ae19oqn1 has verdict pass at the exact reviewed source; no reviewer-added tests or carryover. |
| 2 | Acceptance criteria | PENDING | Darwin key calls strings.ToLower; non-Darwin selected body unchanged; BUILD sync verified. Actual Mac / make test and TestProxyProcessReapRegistryMatchesAnEquivalentSpelling must run without skip/fail on this draft's exact head. Linux skips do not satisfy this requirement. |
| 3 | Full Linux test gate and CI accounting | PASS (Linux) | Five fresh uncached remote phases, 541 first-attempt results; all16 native floor steps PASS; 155189 PASS /0 FAIL /613 SKIP. All6 CI-required leaves accounted below. Real Mac acceptance remains pending in criterion2. |
| 4 | No open HIGH review findings | PASS | Reviewer pass records no unresolved high findings. |
| 5 | Clean final branch | PASS | Canonical merge clean; publication guards require reviewed-source plus this record only and a clean tree after normal commit hooks. |
| 6 | Merge state/build/vet | PASS | Independent merge-tree equality; whole-tree Bazel build and vet/nogo rc0 with successful BEP results; normal hooks active and ICU74 OK. |
| 7 | Single feature theme | PASS | Only macOS reap-key folding plus this gate record; no independent feature or internal-doc contamination. |

## Linux execution evidence

- test_cmd: gate-criterion3.sh --repo /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/merge.6bwy75dc --base 36447913f2aecd88512cc086d59c806c59e486e1 --bead-json /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/bead.json --owned /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/owned-tests.tsv --run-dir /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/criterion3-run/evidence-2
- test_cmd_scope: full-suite
- test_counts: {"FAIL": 0, "PASS": 155189, "SKIP": 613} (top-level Go results per job, including native floor)
- diff_tests_executed: none (no test file or test body changed).
- test_log_dir: /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/criterion3-run/evidence-2/remote/logs; native logs: /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/criterion3-run/evidence-2/remote/floor-logs; read back with gate-test-evidence.py.
- test_exec_mode: remote-exec
- test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- fallback_reason: null
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 66.89607238769531, "waited_seconds": 0.001}
- farm_test_seconds: 13796.696
- heavy_mode: none (verified classifier); owned_topup: none required; floor_topup:16/16 successful steps.
- skip_inventory: /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/complete-skip-audit-plan.json — 671 skip events, 325 unchanged sites; no changed shared/helper test hunks. Source/base native floor and four base remote SKIP-control phases complete. Literal comparison gaps are retained for nudgepoller EXDEV paths and workspacesvc case-sensitive TempDir paths; unchanged guards/platform selection explain both, without reason normalization or comparator-PASS claims.
- skip_justification: Existing environment guards are unchanged; environment-absence cases got the native floor. Linux selects the byte-identical non-Darwin key function and excludes the new Darwin file. The case-sensitive-filesystem skip of the named Mac regression remains an explicit coverage gap reserved for actual Mac CI.
- latency_leg: both budget tests PASS under a clear farm probe; original files at /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/criterion3-run/evidence-2/remote/latency-leg.json.
- policy_lane: pinned fmt/lint/nogo, four shell guards and ten policy targets PASS.
- drift_lane: make bazel-sync followed by tracked/untracked drift checks PASS; all8 generated checks PASS, each in a fresh view.
- complexity: zero_growth=true; base/merge 251/251 rows; diff has no changes; baseline untouched.
- native_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1 wait_timed_out=0 run_start_load=11.84 run_max_load=48.94 run_mean_load=39.08 run_readings=9 wait_first_load=11.84 wait_max_load=11.84 wait_mean_load=11.84 wait_readings=1 read_errors=0
- load_threshold: 15; load_waited_seconds: 1; load_wait_timed_out: 0; run_start_load: 11.84; run_max_load: 48.94; run_mean_load: 39.08; run_readings: 9
- failure_attribution: none in the fresh complete run; waiver_ref:none.
- ci_lane_run: n/a (no CI-config change).

## CI-required accounting

- ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/merge.6bwy75dc 36447913f2aecd88512cc086d59c806c59e486e1 20ac9e490fd587ce74d8f0ca1e209df341873de1 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs..
- ci_required_accounting:
- runner-policy | LOCAL-PASS | /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/ci-required/runner-policy.log
- changes | LOCAL-PASS | /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/ci-required/triggers.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/ci-required/windows-lane.log
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
- windows_reach: {"credential-provider-windows": "none", "windows-lane-test": "none"}
- deferred_to_pr_ci: windows-lane-test (non-linux-runner)
- not_covered: none
- Merge readiness additionally requires CI / required green on the current head and every deferred leaf successful (or path-filter false). This draft grants no merge readiness.

## Abandoned first run, preserved separately

The first remote run stopped in unit:414 PASSED/1 FAILED; raw 91667 PASS /1 FAIL /440 SKIP. Acceptance, integration, latency and native floor did not run. All830 original artifacts were hash-verified. Execution: /var/tmp/gate-ga-dcrsgjdx-6_qaptsn/criterion3-run/evidence/execution.json.

TestFetchCityEventsWalkBudgetTruncatesInsteadOfDiscarding reported a broken pipe at unchanged cmd/gc/cmd_events_test.go:1789. Tracker ga-7tk6eklt was filed and read back. The four-clause screen used proof(c): the selected key function had0.0% coverage, Darwin implementation excluded, failing file/helper unchanged, no test-load or package overlap. The focused coverage probe ran locally because its fresh tree omitted the ignored RBE rc; its raw LCOV/driver/BEP are retained honestly as reach evidence only, with no farm or full-suite credit. Raw failed log SHA256 75703789a7375a33012bb480097e5f1bd75f666591883aeb41806d3787f0ff43. The fresh complete run was authorized by the SAME SHAPE AGAIN paragraph of mayor gcg-11079681, judged alone, with no stitched results, extra sighting or fresh-run failure_attribution. Its named events test actually PASSed (fresh-events-test-proof.json); the tracker remains open until a fix lands.

## Actual Mac runtime — pending

After this Linux-pass draft opens, dispatch mac-regression.yml suite=needs-mac with exact head_repo/head_ref/head_sha/pr_number, per mayor gcg-11064321. Record the actual run id and checkout SHA. Mac / make test must succeed and its log must contain run TestProxyProcessReapRegistryMatchesAnEquivalentSpelling with no skip/fail. No test-failure rerun, no clearance and no merge-request before this evidence. On failure keep the draft and use the ordinary remedy. Any head move requires a fresh gate.
