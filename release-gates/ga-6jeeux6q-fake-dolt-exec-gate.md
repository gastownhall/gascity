# Fake dolt final-exec synchronization release gate

**Verdict:** **PASS**

Deploy: ga-6jeeux6q; build: ga-4t8q3sc6; reviewer PASS: ga-bluc0fji.
Reviewed and gated source: `3d521700d1c87f9e3dc60e64e2550f360cfd1a56`; source branch is provenance only.
gate_base: `5c755132ad4dc9416a6edd6a2943de3100c85747`. gate_base_kind: current-tip.
Tested merge: `9c003669fa44bcacd8bf31cf5e32a47483cb64dc`; retained ref `refs/release-gates/ga-6jeeux6q/tested-merge`.
merge_state: clean (origin/main@5c755132ad4d).
merge_validation: full build/vet and prescribed test chain on the clean merged tree; merge authority validates any newer base at merge time.
Evidence root: `/var/tmp/gc-ga-6jeeux6q-gate.rqB4PN`. No reviewer or prior-head execution result is credited.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Reviewer ga-bluc0fji PASS at exact source; both required findings groups present, no HIGH/blocking finding, no reviewer-added commit. description-context.json. |
| 2 | Acceptance criteria | PASS | Fake process final cmdline and nonempty environ are awaited before kill. Same PID across exec; existing scoped-survives and unscoped-dies assertions and kill matching remain intact. Parent and both subtests PASS independently. |
| 3 | Full tests and required CI | PASS | Complete single full-suite run: one named unrelated deadline failure attributed under3a below; every other phase/top-up and every owned row PASS. Canonical exit80 preserved. Complete skip audit and six CI leaf accounts. |
| 4 | Open HIGH findings | PASS | No unresolved HIGH finding. Reviewer literal-deadline style and hypothetical absent-/proc findings are nonblocking and do not weaken assertions. |
| 5 | Clean final branch | PASS | Source checkout clean before gate-record commit; isolated deploy branch contains reviewed source plus gate-only audit commit, separately verified before push. |
| 6 | Merge and build/vet | PASS | Clean merged tree; full bazel build --config=remote-exec --noremote_local_fallback //... exit0, including nogo;22683 actions. c6 logs retained. |
| 7 | Single feature theme | PASS | One commit, one test file: synchronize fake dolt before its final-exec environment scan. |

fast_lane: PASS; full format, make lint/nogo, git diff --check, four policy guards; make bazel-sync plus git diff --exit-code in separate fresh views, pinned gate-base run. Logs under fast/.
heavy_mode: none (computed). waiver_ref: none. ci_lane_run: n/a (no CI-config diff).
reviewer_added_tests: none. uncovered_criteria: none. descoped_criteria: none. deploy_stage_criteria: none. post_merge_criteria: none.
test_cmd: gate-criterion3.sh full prescribed unit/check, acceptance, integration-packages, full integration excluding registered latency tests, monitored latency phase and16-step native floor. Exact commands/BEPs retained in execution.json.
test_cmd_scope: full-suite. test_exec_mode: remote-exec. fallback_reason: None.
canonical_runner_exit:80. criterion3_valid:false in the original runner record means pending named attribution, discharged here under3a; original record is unmodified. gate-remote-failures.py verify exit0 confirms complete named-only failure with no missing/crashed/timed-out binary or transport hole. validate-run.py revalidates the full run, exact source/base, phase results and owned rows.
test_counts: PASS=159220 FAIL=1 SKIP=618 (top-level result lines; the FAIL is attributed below).
test_log_dir: `/var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/criterion3/evidence/remote/logs`.
farm_test_seconds: 14765.145. Credited fresh remote results:554.

| Phase | Remote uncached results | Cached remote/local | Retries | Window |
|---|---:|---|---:|---|
| unit | 426 | 0/0 | 0 | valid |
| acceptance | 25 | 0/0 | 0 | valid |
| integration-packages | 87 | 0/0 | 0 | valid |
| integration-full | 15 | 0/0 | 0 | valid |
| integration-latency | 1 | 0/0 | 0 | valid |

latency_leg: measured PASS, complete=true, rc0; both registered budgets PASS in final clear-probe window. The main integration leg carries no latency verdicts.
owned_topup: []. floor_topup:16/16 steps exit0.
local_native_load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.66 run_max_load=14.36 run_mean_load=13.21 run_readings=8 wait_first_load=10.66 wait_max_load=10.66 wait_mean_load=10.66 wait_readings=1 read_errors=0`.
load_evidence (native floor): {"load_threshold": 15.0, "load_wait_timed_out": false, "load_waited_seconds": 0, "run_max_load": 14.36, "run_mean_load": 13.21, "run_readings": 8, "run_start_load": 10.66}.
Remote driver telemetry is a separate scope, retained under criterion3/telemetry.

diff_tests_executed:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/criterion3/evidence/remote/logs logs=570 logs_with_results=502
test_counts: PASS=159220 FAIL=1 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0eded8e058b22af3c47d.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log (+56 more)
failed: TestGcStepReminderOnBornSplitCity (integration-full-0c1c194fca4897b865f3.log)
diff_tests_executed:
  TestAgentScopeKillSparesAgentStartedManagedDolt PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestAgentScopeKillSparesAgentStartedManagedDolt/without_infrastructure_scoping_the_store_dies_with_the_agent PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestAgentScopeKillSparesAgentStartedManagedDolt/scoped_from_the_session_the_store_survives_the_agent PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-1cc8c8ab55e59a9e03a9.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
```

failure_attribution: TestGcStepReminderOnBornSplitCity -> ga-3sh5znk0 | clause3:a MECHANISM — not reachable from this test-only diff.
clause1: failure source site test/integration/gc_seat_hook_latency_test.go:394 UNCHANGED, func_touched=no. Only changed file cmd/gc/infra_scope_test.go. Every shared hunk answered in failure-attribution.json: test-only constant/helper excluded from integration binary and launched product gcBinary; comments have no runtime effect; generated fake script still execs sleep60.
clause2: tracker ga-3sh5znk0 opened, created07:32Z before this run; new sighting comment7d4df35e-a76f-575f-9a50-81ca9b2d80e1 read back. Same born-split cold active-step GET deadline condition. This sighting is route=fallback reason=api-error, GET context deadline exceeded,54.36s; earlier tracker sightings spell reason=timeout. No503 observed; attribution does not rely on those spellings being identical.
clause3: own integration-tagged test-binary import closure computed at the tested merge, exit0; no changed non-test input. Child gcBinary excludes cmd/gc _test.go helpers. Structural non-reach independently rules out the source diff; no failed test/base/phase rerun was performed.
clause4: no package overlap, cmd/gc vs test/integration. No census/policy bump, new target or new test file.
repeat-hit exemption: build ga-4t8q3sc6 already stamped gc.fixes_tracker=ga-20tkvfg2; blocking condition has open/unbuilt ga-c4fv0wsb stamped gc.fixes_tracker=ga-3sh5znk0, no shipped/landed artifact. Both live records opened. This is attribution under3a, no waiver.
Full ownership, closure, helper/subprocess and live-stamp evidence: failure-owned-proof.txt, failure-import-closure.stdout, failure-attribution.json, blocking-fix-live.json and tracker.json.

skip_inventory / skip_justification: 676 rows including subtests; 320 unchanged source sites; zero unresolved row/root. Printed reasons, source proofs and every helper/import reach decision retained. Platform/tool/fixture/fast-tier skips outside changed shared reach are unchanged; shared-reachable skipped tests have fresh pinned-base comparisons: 3 farm groups plus 1 exact native command groups, all comparisons pre-existing. No owned test SKIP. Full inventories/baseline logs in skip-audit-summary.json and referenced evidence; no omitted skip claim.
baseline_setup: the first comparison test invocation passed24 local results but had no generated farm config; it failed the remote/retention audit and is uncredited. Its48 log/xml artifacts are SHA256-retained under uncredited-local-baseline-evidence. Corrected setup generated/verified the farm config before the fresh three-group remote comparisons above. No failed test retried and no suite result replaced.
COMPARE-NOTE evidence: ["COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a06, head rbe-a15)"].
delta_gate: shadow/full, no narrowing or old execution credit.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.tT34lh 5c755132ad4dc9416a6edd6a2943de3100c85747 3d521700d1c87f9e3dc60e64e2550f360cfd1a56 fork; actual merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612;6 LEAF jobs need a status; ci-closure.txt retained.
ci_required_accounting:

| Leaf job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact pull_request/quad341/FORCE_BLACKSMITH= runner_policy.py recipe, exit0; ci-runner-policy.stdout.txt, stderr.txt and output under /var/tmp/gc-ga-6jeeux6q-gate.rqB4PN |
| changes | LOCAL-PASS | ci_required_triggers.py over merged tree, pinned base/source and fork; exit0; /var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/ci-closure.txt |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | All four exact merged ci.yml run steps in ci-windows-lane.sh; detached/fresh-tree/load-gate/isolation; exit0; /var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/ci-windows.stdout.txt and ci-windows.stderr.txt |
| windows-lane-test | DEFERRED-TO-PR-CI | runner_policy selects Windows; own GOOS=windows test-binary closure exit0/reach none, ci-windows-native-reach.json; child subprocess review retained. Actual Windows execution belongs to PR CI. |

windows_reach: none in this native job's own fresh GOOS=windows test-binary closure; subprocess source review retained in ci-windows-subprocess-review.txt. Full local cross-build recipe and native floor retain their commands/logs.
deferred_to_pr_ci: windows-lane-test (non-linux-runner). not_covered: none. host_gaps: none.
Gate PASS authorizes opening the PR. Merge only after CI / required and deferred windows-lane-test succeed on its exact head.

execution_evidence (large arrays compacted; full original record retained by pointer):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":80,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/criterion3/evidence/remote","gate_base":"5c755132ad4dc9416a6edd6a2943de3100c85747","gate_head":"3d521700d1c87f9e3dc60e64e2550f360cfd1a56","admission":{"class":"remote","floor_gib":25.0,"free_gib":53.964229583740234,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"5c755132ad4dc9416a6edd6a2943de3100c85747","head":"3d521700d1c87f9e3dc60e64e2550f360cfd1a56","merge":"9c003669fa44bcacd8bf31cf5e32a47483cb64dc","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"f661dc3fb54c2a5d6531111513b2d79a1b7ca7d1eee91621384a8f06f295c97b"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14765.145,"criterion3_valid":false,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":{"elided_array":{"items":1,"bytes":984}},"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38935}},"log_dir":"/var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218523}}},"remote_exit_code":80,"pending_test_attribution":[{"test":"TestGcStepReminderOnBornSplitCity","job":"//test/integration:integration_test","phase":"integration-full","log":"integration-full-0c1c194fca4897b865f3.log"}],"criterion3_valid_if_attributed":true,"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159220,"FAIL":1,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestAgentScopeKillSparesAgentStartedManagedDolt PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-1cc8c8ab55e59a9e03a9.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

Full execution.json retained: 6076 lines; /var/tmp/gc-ga-6jeeux6q-gate.rqB4PN/criterion3/evidence/execution.json; SHA256 f92df7269c36580480ffc5b69006736cd327d5233e3ad65503b6c9ae56970c27.
