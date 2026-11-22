# Suspended-scope background-reader release gate

**Verdict:** **PASS**

bead: ga-7r9u8hlj
evaluated_at: 2026-11-16T12:57:53.371357+00:00
deploy_mode: remote
reviewed_commit: 160cde38ae67e2aebf1ffb27861bcc0c47ac79a1
source_tree: 09c2883c02c2be1aa81a6d5e1da62f2cd9d2e8c2
tested_merge_commit: 73008f623cc0a2ef3a0d3e688151531b3184f112
tested_merge_tree: 0df5b9142c49a41e4ae7398c571de059278806bf
merge_state: clean (origin/main@5e60a76af64cfbc702011218854d070f55c65149)
gate_base_kind: current-tip
gate_base: 5e60a76af64cfbc702011218854d070f55c65149
merge_validation: clean pinned merge built and tested; MPR validates the actual merge result at merge time. Deploy source remains the exact reviewed commit plus this record.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer ga-dqjd0tq7 records PASS at the exact reviewed source; review binding independently verified. No carryover or reviewer-added commit. |
| 2 | PASS | Count cache PrimeActive/Prime/reconcile reads and wait for the bead-event backlog before retirement. Seven owned roots pass in unit and integration; suspended-city and suspended-rig acceptance pass with backstop speedup 6. Named RED precedes GREEN. Complexity has zero threshold growth against the pinned gate base. |
| 3 | PASS | Five fresh remote phases (541 test results), 16 native-floor steps, all owned roots, SKIP obligations and six-leaf required-CI accounting complete. Native Windows jobs remain for PR CI. |
| 4 | PASS | Zero unresolved HIGH review findings. Minor observations and non-blocking option-level coverage follow-up ga-azn28djo are retained in the review. |
| 5 | PASS | Reviewed source and canonical merge pristine before this evidence record; isolated-branch publication guards recheck clean status and that only this record follows the reviewed source. |
| 6 | PASS | Clean merge evaluated first; normal hooks, full Bazel build and nogo/vet pass. Both raw BEPs finish SUCCESS. |
| 7 | PASS | One suspension-quiescence theme across cache accounting, event-watcher progress, regression tests and the design note. No unrelated ancestry or internal configuration. |

Acceptance evidence: investigator FINDINGS.md digest matches the PM-verified record; both uncounted reader mechanisms reproduced. The exact original-gate interleaving remains unproven, as criterion 1 permits. The canonical carrier ga-02lhinfz retains gc.fixes_tracker=ga-9f6n31zj; the tracker remains open/unassigned/unrouted until landing. Held gate ga-v98m6lg was not rerun or released.

test_cmd_scope: full-suite
heavy_mode: none
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config diff)
test_counts: 155059 PASS / 0 FAIL / 607 SKIP (top-level Go result lines across original source logs, not unique tests).
including_subtests: 155423 PASS / 0 FAIL / 665 SKIP.
log_deduplication: 541 original action logs plus 16 native-floor logs; copied floor aliases excluded.
test_log_dir: /var/tmp/deploy-ga-7r9u8hlj.skEcLD/criterion3-run/evidence/remote
policy_lane: pinned golangci-lint 2.14.0 format, Bazel nogo, four shell guards and 10 static policy targets PASS. Static cache credit is permitted and disclosed in raw logs.
drift_lane: make bazel-sync plus git diff --exit-code and empty tracked/untracked status PASS; eight generated/docs targets PASS. Each lane used its own fresh-tree view and gate-base.sh pin.
complexity: form(ii) PASS: base and merge global checks both rc2 with identical existing rows; pinned complexity-diff rc0 reports no threshold changes. Historical ga-q7p6is is closed; no failure is attributed to it and no baseline was refreshed.
known_red_precheck: proceed fix-carrying (ga-02lhinfz stamped for ga-9f6n31zj).
native_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.71 run_max_load=14.04 run_mean_load=11.99 run_readings=6 wait_first_load=9.71 wait_max_load=9.71 wait_mean_load=9.71 wait_readings=1 read_errors=0

Remote phases (all strategy remote, attempt 1, inside phase window, no cached result):

| Phase | Results | Outcome |
|---|---:|---|
| unit | 415 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |

farm_test_seconds: 12943.485
latency_leg: measured under clear farm probe; both named budgets PASS.
owned_topup: none (all seven roots already verified).
floor_topup: 16/16 PASS; beadstest, Darwin compile, runner self-tests and all environment-absence selections included.

```text
diff_tests_executed:
  TestBeadsQuiescenceDoesNotWaitAfterNonBeadEventsAreConsumed PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-088cc38d26d094729229.log)
  TestBeadsQuiescenceDoesNotWaitWithoutAnEventWatcher PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-912ab643330b35bd0b6b.log)
  TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCachePrimeFinishes PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCacheReconcileFinishes PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-752a0bf25b15b465517a.log)
  TestBeadsQuiescenceRetiresOnlyAfterTheBeadEventWatcherFinishesEarlierEvents PASS (integration-packages-75af79934639abaae69c.log, unit-abd97d2cb9b450225d57.log)
  TestBeadsQuiescenceWaitsWhenTheEventLogHeadCannotBeRead PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-f93a0ca96ccc61d0cae9.log)
  TestMergeOracleFieldCoverage PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-9a6a09bab414814cab53.log)
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name
```

SKIP evidence: 593 remote events plus 72 native events. All sites are unchanged; 30 ambiguous conformance helper reports resolve to the unchanged runtimetest helper. 664 source events reproduce at the pinned base with the same reason. The remaining nudgepoller hard-link guard is unchanged and unreachable from the diff; its optional exact-string comparison remains reason-changed because action paths differ. No comparator PASS or reason normalization is claimed for that event.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.T2EwhV 5e60a76af64cfbc702011218854d070f55c65149 160cde38ae67e2aebf1ffb27861bcc0c47ac79a1 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; the rest are rolled up through their needs.

| Required job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | exit=0; /var/tmp/deploy-ga-7r9u8hlj.skEcLD/ci-required/runner-policy.log |
| changes | LOCAL-PASS | exit=0; /var/tmp/deploy-ga-7r9u8hlj.skEcLD/ci-required/triggers.txt |
| credential-provider-windows | DEFERRED-TO-PR-CI | reason=non-linux-runner; reach=none |
| pack-gate | LOCAL-PASS | exit=0; /var/tmp/deploy-ga-7r9u8hlj.skEcLD/ci-required/pack-gate.log |
| windows-lane | LOCAL-PASS | exit=0; /var/tmp/deploy-ga-7r9u8hlj.skEcLD/ci-required/windows-lane.log |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; reach=none |

windows_reach: none for both deferred native Windows jobs.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Before merge: CI / required must be green on the exact PR head and both deferred native Windows jobs must succeed (or skip only because their own path filter is false). MPR owns merge/CI review; this gate authorizes PR opening.

Canonical execution (large arrays compacted):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-7r9u8hlj.skEcLD/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":68.67064666748047,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12943.485,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":121.385,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=70.34 load5=86.38 load15=46.16 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=43.53 load5=78.34 load15=44.77 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.09 72.60 43.76 6/980 54574"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39252}},"log_dir":"/var/tmp/deploy-ga-7r9u8hlj.skEcLD/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":503,"environment-absence":90},"top_level_counts_by_reason_class":{"other":245,"environment-absence":43},"tests":{"elided_array":{"items":593,"bytes":216291}}},"test_cmd":{"elided_array":{"items":5,"bytes":2089}},"test_counts":{"PASS":155059,"FAIL":0,"SKIP":607},"diff_tests_executed":"diff_tests_executed:\n  TestBeadsQuiescenceDoesNotWaitAfterNonBeadEventsAreConsumed PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-088cc38d26d094729229.log)\n  TestBeadsQuiescenceDoesNotWaitWithoutAnEventWatcher PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-912ab643330b35bd0b6b.log)\n  TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCachePrimeFinishes PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCacheReconcileFinishes PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-752a0bf25b15b465517a.log)\n  TestBeadsQuiescenceRetiresOnlyAfterTheBeadEventWatcherFinishesEarlierEvents PASS (integration-packages-75af79934639abaae69c.log, unit-abd97d2cb9b450225d57.log)\n  TestBeadsQuiescenceWaitsWhenTheEventLogHeadCannotBeRead PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-f93a0ca96ccc61d0cae9.log)\n  TestMergeOracleFieldCoverage PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-9a6a09bab414814cab53.log)\nGATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name\n"}
```

Full retained evidence:
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/final-gate-audit.json; SHA256 516d0b4036d48b52c7d7eaedb1d4979e4e48e016b7b95e0b68dc565c960fb7b0
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/remote-phases-independent-audit.json; SHA256 49406a80b0317525f68bad76d16e77f0644d79528b5a66cb64c5a779d5378a94
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/runtime-independent-audit.json; SHA256 7b0dea03d6b576e8aaec26a68ac4de83b5de25c167c3dded14bd6819a8a9b57a
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/skip-comparison-audit.json; SHA256 e70c4274808124e141df4193bc5d0a8dbc63309fa29173ccda2736d3f9e83547
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/nudgepoller-skip-mechanism-proof.json; SHA256 a17e795a5e78094764793494c4a2f0f4b82eec14bb03fe2b84a5af2fb36eb029
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/ci-required/accounting.json; SHA256 0f977873728a017e43f5192a6d519c3d9a7585a60271d354e8189016f97910f4
- Full evidence manifest: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/SHA256SUMS; SHA256 6ed176ee7acbcf1b1435ca3e7c2881eb33babb17b982b5d4eb2105c67ef538d8

Full command and inventory pointers:
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/criterion3-run/evidence/execution.json; SHA256 a6b0c3ecf349203f44f666abd22dbe4c2ccac22cd054adc26c7721d036565283
- /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7r9u8hlj/gate-pass-20261116T125912Z/evidence/complete-skip-audit-plan.json; SHA256 11fa5a032edfb827a45a01abbc259869fbdde6a655a141ea71cdbbc9d81839cf
