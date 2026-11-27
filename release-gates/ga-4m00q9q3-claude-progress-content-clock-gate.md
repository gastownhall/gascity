# Release gate: Claude content-idle progress clock (ga-4m00q9q3)

**Verdict:** **PASS**

Evaluated UTC: 2026-11-26T09:53:08.864079+00:00
DEPLOY_MODE=remote; PUSH_REMOTE=fork; BASE_BRANCH=main; BASE_REF=origin/main.
Reviewed source: a7cadd5e85e99b2ab50ce5640b5f7205600a3d05
Pinned gate base: d38ec7842f8d66b380660d5a21d4050ee2b93b04
Tested merged validation head: 95a72acf07c78403eb9de8a508b0665ab25b9f17
Tested merged tree: 187e6e839a5c68517fa6b83a9f8b9a49eda1763c
merge_state: clean (origin/main@d38ec7842f8d66b380660d5a21d4050ee2b93b04)
gate_base_kind: current-tip; gate_base_sha: d38ec7842f8d66b380660d5a21d4050ee2b93b04
merge_validation: merge result built and vetted here. Main may advance; MPR owns final merge-time validation. No deployer rebase.
Review: ga-2u9ihcm1; build: ga-g9n7ov; investigation: HQ gm-zm6zt9. Source branch builder/ga-g9n7ov is provenance only. The isolated deploy branch is cut from the resolved reviewed source; only this release record is added afterwards.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
|1|Review|PASS|Security/style/spec PASS in review ga-2u9ihcm1 binds to the resolved reviewed source. No carryover or reviewer-added test head substitution.|
|2|Acceptance|PASS|Pre-merge clock/anchor/recycle contract audited and all7 changed tests PASS by name. The real deployed30-40min criterion is explicitly deferred to an owned post-merge observation below, not met or waived.|
|3|Tests and required CI|PASS|Full five-phase uncached remote chain; native16/16 and isolated latency PASS; raw159423 PASS/0 FAIL/618 SKIP. All676 structured skip observations justified. Six required CI leaf jobs accounted below; one native Windows job deferred.|
|4|Review findings|PASS|No unresolved HIGH/major finding; reviewer security has none and style/spec have no blocker.|
|5|Clean source|PASS|Validation scratch clean after all lanes. Publication guards require the isolated deploy branch to add this record only to the reviewed source, with clean worktree after its commit and before push.|
|6|Merge/build/vet|PASS|Clean materialized merge on the recorded base; Bazel build+nogo of both CLIs exit0. Scratch git hooks disabled.|
|7|Scope|PASS|One legacy progress-stall clock feature:5 cmd/gc files, +332/-11,2 builder-tagged commits. Denylist/citation ancestry guard PASS; no stack.|

## Acceptance and review audit

One feature theme: Claude non-ACP progress-stall content clock, only5 cmd/gc files and2 bead-tagged commits. Review ga-2u9ihcm1 security/style/spec PASS at resolved a7cadd5e85e99b2ab50ce5640b5f7205600a3d05; no reviewer-added test carryover. Production audit: independent progressIdleAnchors map initialized and cleared with idleSince; shared observeIdleRun preserves busy/indeterminate reset and transient-error anchor retention; contentAwareLastActivity chooses min(raw,content anchor) only for Claude/non-ACP SnapshotIdle runtimes and preserves raw for unsupported,busy,error,nil/zero cases; the one reconciler call sits inside the existing positive progress/claim-holder threshold gate and feeds both unchanged predicates. All pending-interaction, attachment, pinned-session, pool-floor, assigned-work and provider-health exemptions remain. Legacy reconciler only; v2 ignores the setting. Existing tests deliberately preserve additive raw-clock fallback on snapshot failure. Three new reconciler fake-clock tests cover updater-flash recycle,busy-reset,error-with-fresh-raw guard; four tracker/helper tests cover anchor lifecycle,independence and provider/transport/fallback cases. Real deployed30-40min criterion deferred to owned post-merge observation as above, never claimed by synthetic time. Runtime evidence pending full C3.
The shared snapshot observer resets on busy or indeterminate readings; transient errors preserve the anchor and return no content-idle evidence. Unsupported/provider/transport/nil/zero/busy/error cases retain the existing raw-clock behavior. In particular an unavailable snapshot does not introduce an earlier clock; the existing additive raw fallback is intentional and is tested. Progress and idle-timeout anchors are separate; clearIdleAnchor clears both. The existing health, attachment, pending-interaction, configured-named-session, pool-floor and work-claim exemptions remain in the predicate path.

## Criterion6 and fast lanes

C6 command, detached via load gate and isolation wrapper, in the scratch: `bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501`; exit0,5188 actions including4 remote. Run: /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/criterion6-v2.
An initial setup attempt rejected a wrong hand-saved JSON shape/class before compilation; it is no build/test result. The corrected run uses the full bd JSON array and canonical remote class, same merge tree, without changing source.
policy_lane: `bazel test --config=remote-exec --noremote_local_fallback --config=ci //:go_format_test`; four shell guards (`make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary`); `make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'`; all exit0, full lint1505 targets.
drift_lane: `make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'` then `git diff --exit-code`; exit0. Both lanes use separate fresh committed views, gate-base.sh, isolation wrapper, detached. Neither writes the suite scratch. All fast lanes completed before runtime setup or C3 launch.
`docs/PROJECT_MANIFEST.md` absent; TESTING.md and Makefile document the Bazel equivalents. CI generated-file drift was found in bazel.yml: make bazel-sync plus git diff --exit-code. No API/dashboard/docs source changed; dashboard target tests still ran in the full suite.
known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)
Interlock: upstream lineage, exit0. Round-cap gate round1 permitted. `make check-hooks` confirms .githooks active; the gate-record commit must run its normal pre-commit hook.

## Criterion3 execution

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.30oDKG --base d38ec7842f8d66b380660d5a21d4050ee2b93b04 --bead-json /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/bead.json --owned /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/diff-owned.txt --run-dir /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
heavy_mode: none (canonical classifier exit0)
waiver_ref: none
failure_attribution: none; no failed test retried or excused.
test_counts: PASS=159423 FAIL=0 SKIP=618
criterion3_valid: true; exit_code:0
farm_test_seconds: 14876.694
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/logs (571 retained logs; digest manifest below)
The full chain is run by a systemd service through gate-detached-run.sh. Runtime preparation: isolation-wrapper rootless Podman _ping OK, paired Ryuk-disabled/unreaped flags; this rig has no Go testcontainers image pin to match. ICU74 reports OK with recorded soname/digests. Disk precheck25GiB passed at42.3GiB. BD pinned to v1.3.4 with ref check match. No fake backend or test-scope override supplied. Actual selection and worker-env evidence:
```json
{"admission":{"class":"remote","floor_gib":25.0,"free_gib":42.35431671142578,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"d38ec7842f8d66b380660d5a21d4050ee2b93b04","head":"a7cadd5e85e99b2ab50ce5640b5f7205600a3d05","merge":"95a72acf07c78403eb9de8a508b0665ab25b9f17","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"4622338dccfc2df1091ef4b65a0f943801bf28fdec81b66bb32dafd25518e015"}}
```

| Phase | Actual remote results | Cached local/remote | Retried/other strategy | Window and verdict |
|---|---:|---|---|---|
|unit|427|0/0|0/0|valid window, PASS|
|acceptance|25|0/0|0/0|valid window, PASS|
|integration-packages|87|0/0|0/0|valid window, PASS|
|integration-full|15|0/0|0/0|valid window, PASS|
|integration-latency|1|0/0|0/0|valid window, PASS|

Phase commands/flags as recorded by the canonical runner:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1795684006942 end_ms=1795684381625
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1795684382541 end_ms=1795684560432
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1795684560963 end_ms=1795684843267
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1795684844041 end_ms=1795685321765
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1795685322431 end_ms=1795685432049

By-name evidence:
```text
diff_tests_executed:
  TestContentAwareLastActivity PASS (integration-packages-509ef0a6cd81866833cc.log, unit-752a0bf25b15b465517a.log)
  TestIdleTracker_ProgressIdleSinceIndeterminateResetsAndErrorKeepsAnchor PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-d9e667428f972d067bd3.log)
  TestIdleTracker_ProgressIdleSinceIsIndependentOfIdleTimeoutAnchor PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-fd986cf5bab8709b8d45.log)
  TestIdleTracker_ProgressIdleSinceTracksContinuousIdleRun PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-1ab526405ace21879f85.log)
  TestReconcileSessionBeads_ProgressStallBusyObservationResetsContentAnchor PASS (integration-packages-12697e2ea0d32c278115.log, unit-637c74c578a2b17fc654.log)
  TestReconcileSessionBeads_ProgressStallRecyclesContentIdleClaudeDespiteUpdaterFlash PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-27b23e708b477fce4dad.log)
  TestReconcileSessionBeads_ProgressStallSnapshotErrorDoesNotRecycle PASS (integration-packages-9326181a736eb30b71ca.log, unit-423c6a0da1986b90e9a7.log)
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name
```

Native floor: all16 canonical commands exited0, including every environment-absence package, beadstest cache guard, Darwin compile and runner self-tests. Full command list: /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/remote/floor-topup.json. Each runs with the canonical scrubbed test environment and workload umask; local count is not stitched to a fallback suite.
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.10 run_max_load=10.59 run_mean_load=9.44 run_readings=7 wait_first_load=8.10 wait_max_load=8.10 wait_mean_load=8.10 wait_readings=1 read_errors=0

Latency lane only: both names from gate-latency-tests.txt ran once and PASS; neither runs in the full-integration leg. Probe initially busy, then clear after30s; measured window below threshold50, no budget or statistic changed.
```json
{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":107.116,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=73.93 load5=87.74 load15=47.56 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=44.80 load5=79.35 load15=46.04 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=9 max_load1=44.80 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.21 73.61 45.02 17/1054 56153"],"window":{"readings":9,"max_load1":"44.80","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0}
```

## Skip integrity

Canonical count618 is604 farm skip records plus14 native column-zero root records. The structured site audit additionally retains58 native subtest skips:676 total observations, no result changed. All318 sites UNCHANGED, func_touched=no,0 unresolved; each printed reason/site/reach basis is retained per observation.
SHARED-HUNKS: cmd/gc/session_reconciler_progress_test.go adds scriptedIdleSnapshotProvider/runUpdaterFlashTicks, called by the new fake-clock tests; cmd/gc/session_reconciler_test.go adds fakeIdleTracker.progressIdleSince returning no content evidence. These test files do not link into CLI children. Whole-module Go list graph, with integration+acceptance tags, has718 package variants and no importer of cmd/gc outside its main package. For every cmd/gc skip the required pinned-base comparison independently reproduces the same skip/reason: unit gc_sim_exec_test4, unit gc_test189, integration-packages gc_test68, exact native cmd/gc floor command36;297 observations across3 Bazel groups plus1 native group, ALL PRE-EXISTING. Other unchanged guards have no reachable changed test/helper hunk. No owned test skips.
```json
{"raw_skip_events":676,"canonical_skip_count":618,"remote_skip_records":604,"native_skip_records":72,"native_root_skip_records":14,"native_subtest_skip_records":58,"counter_note":"Canonical SKIP618 is604 remote records plus14 native column-zero root records. The structured site audit also retains58 native subtest records:676 total, no result changed.","unique_sites":318,"unresolved_sites":0,"base_measured_events":297,"bazel_control_groups":3,"native_control_groups":1,"all_comparisons":"ALL PRE-EXISTING"}
```

## Required CI accounting

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.30oDKG d38ec7842f8d66b380660d5a21d4050ee2b93b04 a7cadd5e85e99b2ab50ce5640b5f7205600a3d05 fork`; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612;6 LEAF jobs, check is AGGREGATOR. CI suite_mode filtered is a path-filter result only; this deploy ran the full suite.

| Leaf job | Status | Evidence |
|---|---|---|
|changes|LOCAL-PASS|/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/closure.txt|
|runner-policy|LOCAL-PASS|/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/runner-policy-output.txt|
|windows-lane|LOCAL-PASS|/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/windows-lane-output.txt|
|credential-provider-windows|NOT-TRIGGERED|credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'|
|pack-gate|NOT-TRIGGERED|pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'|
|windows-lane-test|DEFERRED-TO-PR-CI reason=non-linux-runner|/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/windows-native-closure.txt|

LOCAL jobs ran sequentially from fresh views of this exact merged commit, behind load gates, isolation wrapper and detached service; full commands are retained in /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/runner-policy-lane.sh and ci-windows-lane.sh. All4 Windows/macOS cross-compile steps have actual CI_STEP_PASS markers. The untriggered jobs are the tool's rows, not manual filter guesses.
windows_reach: windows-lane-test none, GOOS=windows GOARCH=amd64 complete test-binary closure of its own allowlist (credentialprovider,genshell,processgroup), rc0; no changed non-test input. Actual runner from runner-policy output: blacksmith-4vcpu-windows-2025. Native Windows tests are not claimed by compilation.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
ci_lane_run: n/a (no CI-config diff)
The gate PASS authorizes PR-open only. Require CI / required and native windows-lane-test success on the PR head before merge.
runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.30 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=9.30 wait_max_load=9.30 wait_mean_load=9.30 wait_readings=1 read_errors=0
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.14 run_max_load=12.07 run_mean_load=10.83 run_readings=3 wait_first_load=9.14 wait_max_load=9.14 wait_mean_load=9.14 wait_readings=1 read_errors=0

## Post-merge observation, not a gate item

The legacy free-text criterion requires a real deployed always-on Claude/non-ACP named session with progress_stall_timeout=30m, its last completed turn and updater flash, and an actual recycle30-40min later plus the corresponding progress-stalled diagnostic. It is **deferred to post-merge observation, not met or waived** (gm-i4tfv). Before closing this deploy, mint its owned observation with post-merge-observation.sh and the explicit flags in /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/post-merge-mint-args.json, naming the actual PR. The24h window starts at GitHub mergedAt, never this gate or bead close time. The observation requires deployed-build proof and exact-window real-session evidence; unavailable/missing/rotated trace or unknown deployment is UNKNOWN, not zero/PASS. Carry its verified id in the PR note and merge-request. The deferred check is evidence extraction plus manual adjudication; exit0 alone is not a behavioral pass.

## Retained evidence

Private durable tested-merge ref: refs/release-gates/ga-4m00q9q3/tested-merge-95a72acf07c7 -> 95a72acf07c78403eb9de8a508b0665ab25b9f17. Raw execution.json and all per-job logs are retained unchanged. The completed scratch Bazel output base was released after C3; copies of the digest-verified phase logs remain. Original setup refusal, all launch receipts, ownership explanation, baseline command/output/comparisons and CI records remain under /var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1, /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence, and /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/execution.json: SHA256 11e7ddc29498897526b05609fb55606c1a643253a5fecf1799a63b804903db8a.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/analysis/not-owned-skip-evidence.json: SHA256 b8ba3af6cbfa3b08a06d16943cbcecfca6b729de4151b6cd1e01e8deb395fa8d.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/analysis/skip-audit-summary.json: SHA256 269e1008cf6c886136f2cf9f7f353097ff066827c50a5ec4b22fe8bf8c125077.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence/analysis/retained-log-sha256.json: SHA256 e169375701700ed07367eca4e4a54c6fa35a614045b23f3cfa4574d2705347f3.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/accounting.json: SHA256 03e7badd7f26b8f7909148d621119874cf1188a8bf2f649733ef7e656068d834.
/var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.ci-required/windows-native-reach.json: SHA256 5575af57a34b806e0e851cacee6d521c50e7de404eea25e903d41c4bffda1e5e.
/var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/post-merge-mint-args.json: SHA256 9677e732ac91a322eac1949a3b6b72f157ccab7c3cd85b24f5f94da861b1905c.
/var/tmp/gc-gate-ga-4m00q9q3.l0h3p2g1/shared-hunk-reach-proof.json: SHA256 56fd1c9b752e5a58d4ca1f3d7c8c438fa6026532526c403fafdf590664f6b0fa.

prepush_suite: skipped
prepush_suite_c3_evidence: /var/tmp/gc-heavy-gate/runs/ga-4m00q9q3.c3/evidence
