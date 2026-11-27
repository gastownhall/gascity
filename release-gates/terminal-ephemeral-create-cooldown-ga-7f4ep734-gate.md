# Terminal ephemeral create cooldown release gate

**Verdict:** **PASS**

bead: ga-7f4ep734; reviewed build lineage: ga-jj03cq / ga-n6kztu; review: ga-10qn2mwk
deploy_mode: remote; push_remote: fork; base_branch: main
reviewed_commit: 89dd683ebc002b786074a8833c8cea2bc191528e
gate_base: 6dce31bbb68e31a95482815050a0ace951be47ce
gate_head: 09a3f70ec1d152c6a815d8d14e27f20744d0d5b5
gate_tree: 464711bf74a62709c6371e59672df3154e78f620
gate_base_kind: current-tip
merge_state: clean (origin/main@6dce31bbb68e31a95482815050a0ace951be47ce)
merge_validation: the materialized merge result was built, vetted and tested here; MPR owns final merge freshness and exact-head CI checks.
evidence_root: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7f4ep734/gate-20261127T065842Z-q7o03uz1

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Review ga-10qn2mwk records PASS for the exact resolved reviewed commit. No carryover or reviewer-added test exception. Full review retained in evidence/review-bead.json. |
| 2 | Acceptance criteria | PASS. Five-minute fixed window keyed by template and resolved work directory; open and closed terminal history; per-decision clock; future-stamp bound; visible fail-open errors; no directory creation; plan-only reads nothing; manual/named sessions unaffected. All 14 owned regression tests PASS by name. The reviewed fixed-window decision removes the public config knob. |
| 3 | Tests and required CI jobs | PASS for PR-open. All five suite phases audited fresh remote; native floor complete; latency measured under a clear probe; every owned test PASS. All six required CI leaf jobs accounted below. Skips cleared by unchanged-site/reach evidence and pinned-base comparisons. |
| 4 | Open high-severity findings | PASS. Review has no unresolved HIGH/blocking finding. Its helper-name nit and bounded history-scan cost remain non-blocking observations. |
| 5 | Clean final branch | PASS. Reviewed source was clean at the isolated branch cut. This gate record is the sole deploy addition; the final post-commit clean check is required before push and is recorded on the bead. |
| 6 | Merge state and build/vet | PASS. Pinned-base materialized merge built with bazel build --config=remote-exec --noremote_local_fallback //...; nogo/vet clean, rc=0, 1505 targets. Hooks disabled only for the scratch merge commit. |
| 7 | One feature theme | PASS. All three source commits and seven changed files implement or test this cooldown. Ancestry scoped with the accepted lineage IDs; no unrelated theme, internal agent config, or stack. |

## Runtime evidence

test_cmd: gate-criterion3.sh --repo <pinned merge scratch> --base 6dce31bbb68e31a95482815050a0ace951be47ce --bead-json bead.json --owned owned-tests.tsv --run-dir c3-run/evidence
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 15, "results": 15, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 87, "results": 87, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 427, "results": 427, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 42.10712432861328, "waited_seconds": 0.001}
farm_test_seconds: 14398.988
test_counts: {"FAIL": 0, "PASS": 159819, "SKIP": 618} (gate-test-evidence.py result-line counts, not distinct test names)
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7f4ep734/gate-20261127T065842Z-q7o03uz1/evidence/c3-run/evidence/remote/logs
owned_topup: none needed; all 14 owned tests PASS in unit and integration-packages.
floor_topup: complete, 16 checks; pinned bd v1.3.4; separate local floor after returning the remote slot.
latency_leg: measured in one attempt; TestGcBdLatencyBudget PASS; TestGcSeatHookLatencyBudget PASS. Probe waited 50s, clear at load1=47.45, threshold=50, seven window readings, zero readings over threshold.
heavy_mode: none
delta_gate: shadow, full gate (not-gate-pr); nothing narrowed.
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 6.49
run_max_load: 10.44
run_mean_load: 8.82
run_readings: 7

The load fields describe the native floor; remote test execution has no host-load gate. Full phase commands, BEP windows, raw CAS artifacts and normalized logs are retained. Raw and archived CAS files byte-verified: 1324. The tested merge is retained in tested-merge.bundle.

diff_tests_executed:

```text
diff_tests_executed:
  TestPoolTerminalCreateCooldownBoundsRepeatedFailedCreates PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-423c6a0da1986b90e9a7.log)
  TestPoolTerminalCreateCooldownDoesNotInterceptManualOrNamedSessions PASS (integration-packages-e4245ddf6a37030111bc.log, unit-abd97d2cb9b450225d57.log)
  TestPoolTerminalCreateCooldownDoesNotSuppressDistinctOrIneligibleHistory PASS (integration-packages-9326181a736eb30b71ca.log, unit-752a0bf25b15b465517a.log)
  TestPoolTerminalCreateCooldownExpiresOnDecisionTimeNotBeacon PASS (integration-packages-e5330b8c981927473156.log, unit-1ab526405ace21879f85.log)
  TestPoolTerminalCreateCooldownHistoryQueryFailureFailsOpenAndIsVisible PASS (integration-packages-821a75e4a503601af052.log, unit-f93a0ca96ccc61d0cae9.log)
  TestPoolTerminalCreateCooldownLogsSuppression PASS (integration-packages-12697e2ea0d32c278115.log, unit-fd986cf5bab8709b8d45.log)
  TestPoolTerminalCreateCooldownPlanOnlyReadsNothing PASS (integration-packages-75af79934639abaae69c.log, unit-cd6930bb0d0da8646500.log)
  TestPoolTerminalCreateCooldownReadsOpenAndClosedHistory PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-d9e667428f972d067bd3.log)
  TestPoolTerminalCreateCooldownToleratesSmallClockSkew PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestPoolTerminalCreateCooldownWorkDirResolutionDoesNotCreateDirectories PASS (integration-packages-074fc6c34a1fb2577127.log, unit-088cc38d26d094729229.log)
  TestPoolTerminalCreateCooldownWorkDirResolveFailureFailsOpenAndIsVisible PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-912ab643330b35bd0b6b.log)
  TestLatestEphemeralTerminalFailure PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLatestEphemeralTerminalFailureNilStore PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLatestEphemeralTerminalFailureSurfacesStoreError PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 14 diff-owned tests verified by name
```

## Fast lane and skipped tests

fast_lane: PASS. Format //:go_format_test; make lint (nogo/vet); make check shell guards; CI-discovered make bazel-sync followed by git diff --exit-code. Each ran in a separate fresh-tree-run view pinned by gate-base.sh. Exact FRESH_TREE_RECORD and GATE_BASE_RECORD lines are in evidence/fast-output.log.
known_red_precheck: proceed, nothing-known-red. The check-routed-test-rows guard passed; ga-1ptv4jey was reported stale.
skip_inventory: 604 remote skip events plus 72 native skip events. All 319 remote and 45 native skip sites UNCHANGED, line_in_hunk=no; no unmapped site. Inventories and source/reason recoveries are retained.
skip_justification: subprocess-only helper rows, platform-specific checks, split-store fixtures and optional live-environment checks. No owned test skipped. Every shared hunk/changed input in reach is named by skip-measure-plan.json; none changes the recorded skip decisions. Remote420 plus native40 required events matched the pinned base; remaining remote184/native32 events have no changed input in their computed import closure.
remote-acceptance: COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
remote-integration-packages: COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a03, head rbe-a09)
remote-integration-packages: COMPARE-SUMMARY head_skips=88 judged=88 pre-existing=88 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
remote-unit: COMPARE-SUMMARY head_skips=318 judged=318 pre-existing=318 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-0: COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
native-1: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

## Required CI accounting

ci_required_closure: ci_required_triggers.py <merge scratch> 6dce31bbb68e31a95482815050a0ace951be47ce 89dd683ebc002b786074a8833c8cea2bc191528e fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci-closure.txt; ci_required_triggers.py rc=0 |
| runner-policy | LOCAL-PASS | ci-runner-policy-output.log; exact workflow command rc=0 |
| credential-provider-windows | NOT-TRIGGERED | ci-closure.txt: needs.changes.outputs.credential_provider == true; no match |
| pack-gate | NOT-TRIGGERED | ci-closure.txt: needs.changes.outputs.packs == true; no match |
| windows-lane | LOCAL-PASS | ci-windows-output.log; both exact workflow commands rc=0 |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | ci-windows-output.log; actual Windows native test allowlist import closure computed by ci-windows-reach.py, reached_files=[] |

windows_reach: none for the actual native Windows allowlist and credential-provider job; successful go list -test -deps closures, command and counts in evidence/ci-windows-output.log.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Merge requires CI / required green on the exact PR head and the deferred native Windows job success (or its own false path filter). This PASS authorizes PR-open only.

docs/PROJECT_MANIFEST.md is absent; canonical suite/policy commands came from TESTING.md, Makefile and the merged CI workflows. No source, config, schema or dashboard file was changed by the deployer.
