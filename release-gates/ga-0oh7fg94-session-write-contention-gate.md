# Release gate: consistent session write contention (ga-0oh7fg94)

**Verdict:** **PASS**

Evaluated UTC: 2026-11-26T08:22:17.677067+00:00
DEPLOY_MODE=remote; PUSH_REMOTE=fork; BASE_BRANCH=main; BASE_REF=origin/main.
Reviewed source: 95da871e403db500e002b5624bcc7020b6f70f44
Pinned gate base: aa17d25430f1dda4a8a7834e004875961b3f7806
Tested merged validation head: f901765d5c0fd5afc3f0f80cabc7b41badd1dd17
Tested merged tree: ee7b7bbf0c97419c021dd6d56c7fe0dd92278932
merge_state: clean (origin/main@aa17d25430f1dda4a8a7834e004875961b3f7806)
gate_base_kind: current-tip; gate_base_sha: aa17d25430f1dda4a8a7834e004875961b3f7806
The gate tested the clean merge on the recorded base. The deploy source remains the reviewed commit; its only later addition is this release record. Main may advance; MPR owns final merge-time validation.
Review: ga-29m0s0bd. Build: ga-6rv2z35u, with round2 ga-4twkorpu. Original contract/fixtures: ga-yyev65, ga-uw4mfi, ga-kq4duy, ga-48ezwr4q. Landing record: ga-bygzs8i0; follow-up: ga-4iy5mmmh.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review | PASS | Reviewer-added-tests helper independently verified 1f06b2fcac259b6b4097086804b767c305d55c10 to the resolved reviewed source: six appended tests,327 added lines, no rewritten tests, in internal/beads/write_conflict_telemetry_contract_test.go. Review ga-29m0s0bd has security/style/spec PASS. |
| 2 | Acceptance | PASS | Approved server-side T1-T5/G1-G8 audited; all13 owned tests PASS by name, including classifier,503 mapping,budgets and telemetry. Detailed audit below. |
| 3 | Tests and required CI | PASS | Complete local recovery after remote transport loss; three non-owned failures individually attributed below. Raw counts retained. Native16/16 and isolated latency PASS; all skips justified; six required CI leaf jobs accounted. |
| 4 | Review findings | PASS | No unresolved HIGH/major finding. Accepted LOW S1, recovered-final class serialization, belongs to follow-up ga-4iy5mmmh. |
| 5 | Clean source | PASS | Validated scratch clean after all lanes. Publication guards require the isolated deploy branch to contain the reviewed source plus this record only, and a clean worktree before push. |
| 6 | Merge/build/vet | PASS | Clean merge on the pinned base; both CLIs built under Bazel+nogo, detached via isolation wrapper, exit0. Scratch commit hooks disabled. |
| 7 | Scope | PASS | One session-row contention contract across store,API and telemetry. Ancestry denylist/citation guard passed for this lineage. No CI config,dependency or internal agent-config change. |

Scope is the approved server-side session-row contention contract (ga-kq4duy G1-G8, ga-uw4mfi BUILD1-4, ga-yyev65 T1-T5). The independent CLI retry vehicle remains outside this diff.
Code audit: IsRetryableConflict delegates to the existing native serialization predicate plus typed CAS exhaustion; the separate API string matcher and its bare40001 false positive are removed. Existing 1213 mapping is preserved, CAS exhaustion now receives the declared503/store-unavailable/store_conflict: shape at humaStoreError and the post-close delete failure. NotFound404, unrelated500 and write_unconfirmed handling retain their previous mapping. Runtime close/delete side effects and op-journal outcomes remain distinct from response mapping. Clients may observe a stopped runtime with an open row after an exhausted write and must re-read or retry; the source handoff report enumerates nine helper callers plus the post-close delete path.
Budgets stay3 attempts with25/50ms native/Bd write backoff, unchanged conditional/CAS budgets. Delete's five-attempt helper stops before its final125ms sleep. No handler retry or new outer retry is added. The decision explicitly leaves the removed trailing-sleep mutant unpinned; no new timing claim is made from it.
Telemetry records exhausted and recovered-final outcomes; only exhausted produces a structured warning. Metric labels are fixed operation/class/outcome tokens; bead id and attempts appear only on the warning. All15 native wrapper call sites and BdStore ordinary/conditional/CAS paths are wired. Nonretryable failures on the last lap are excluded. Known LOW S1 (recovered-final labels serialization even after another class) is accepted in review and belongs to P4 ga-4iy5mmmh with a separate architect contract, not this gate.
Guardrails: no retry budget, usage-writer, non-session error-mapping, legacy session-handler, live-contract-test, session mutation-lock, OpenAPI/genclient or dashboard-type change. No CI-config change. Exported functions/constants have doc comments. Source additions and tests support one feature across beads, API and telemetry.
Evidence: criterion6 merged build+nogo PASS; criterion1 reviewer-added-tests helper independently verifies X->Y test-only append. Fresh evidence: all13 owned tests PASS; full unit lane includes the OpenAPI/generated drift targets. API-area quality gates include make dashboard-check and the prescribed local npm preview in a separate fresh view.
Landing prerequisite ga-bygzs8i0 has mayor owner acceptance (live notes read this run). Close it only after this eventual PR lands, with squash/tree/work-record evidence and main inspection of recordWriteBudget, all retry-loop wiring and contract tests. Then notify gascity/pm --notify naming ga-bygzs8i0 and ga-4iy5mmmh.

criterion6_cmd: isolated-test-run.sh --rbe --bead ga-0oh7fg94 -- bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501; rc0.
policy_lane: format plus check-release-dist-ignore/check-routed-test-rows/check-split-topology-rows/check-residency-boundary and full nogo; rc0. Drift lane: make bazel-sync, git diff --exit-code; rc0. Both ran in separate fresh views before the suite.
API quality: make dashboard-check, zero drift, make dashboard-build-npm, zero drift and localhost npm preview; HTML200 and served JS200, preview process group stopped; rc0.
known_red_precheck: proceed=nothing-known-red; check-routed-test-rows green, stale tracker ga-1ptv4jey. ci_lane_run: n/a (no CI-config diff). docs/PROJECT_MANIFEST.md absent; TESTING.md, Makefile and actual CI supplied commands.

test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.9nxeG5/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
Criterion3 adjudication: canonical exit80 retains criterion3_valid=false and criterion3_valid_if_attributed=true. A complete restart failed only by the three named tests below, with no holes; all four attribution clauses are satisfied for every row. This is the documented gm-jkode decision path; execution.json is unchanged.
test_exec_mode: local
fallback_reason: transport-failure
test_cmd_scope: full-suite
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
The remote unit attempt had425 uncached results and two NO STATUS actions (missing executable CAS blobs, exit34). The canonical command then ran the complete local chain. Partial remote evidence is retained and supplies no local suite counts.
test_counts: {"FAIL": 3, "PASS": 159531, "SKIP": 614}
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/logs (retained; never deleted)
gate_base: aa17d25430f1dda4a8a7834e004875961b3f7806
gate_head: 95da871e403db500e002b5624bcc7020b6f70f44
heavy_mode: none
coverage_check: n/a (not a heavy composite)
waiver_ref: none
delta_gate: {"abandoned": false, "applied": false, "base": "aa17d25430f1dda4a8a7834e004875961b3f7806", "decision": "full", "detail": "no PR record (pr.json)", "head": "95da871e403db500e002b5624bcc7020b6f70f44", "mode": "shadow", "reason": "not-gate-pr"}
Launcher: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-validate.9nxeG5 --base aa17d25430f1dda4a8a7834e004875961b3f7806 --bead-json /var/tmp/gc-gate-ga-0oh7fg94.vprk988y/bead.json --owned /var/tmp/gc-gate-ga-0oh7fg94.vprk988y/diff-owned.txt --run-dir /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence
Launcher env: GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3. Rootless Podman ping OK through the isolation wrapper. TESTCONTAINERS_RYUK_DISABLED=true paired with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Local admission40GiB/cap3; ICU74=OK recorded.
test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-local-bazel-run.py --leg-dir /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local --owned /var/tmp/gc-gate-ga-0oh7fg94.vprk988y/diff-owned.txt
Local phase commands (all uncached; includes both unit legs):
```text
unit: make -k check 'BAZEL_FLAGS=--config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json'; rc=2; results=426; failed=2; strategies={"linux-sandbox": 426}
unit-leg2: make -k check 'BAZEL_FLAGS=--config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json'; rc=2; results=1; failed=0; strategies={"processwrapper-sandbox": 1}
acceptance: /home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/acceptance.bep.json --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; rc=0; results=25; failed=0; strategies={"linux-sandbox": 25}
integration-packages: /home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-packages.bep.json --config=integration //test:integration_packages; rc=0; results=87; failed=0; strategies={"linux-sandbox": 87}
integration-full: /home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-full.bep.json --config=integration --test_arg=-test.v '--test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$' //test/integration:integration_test; rc=0; results=15; failed=0; strategies={"linux-sandbox": 15}
```
Native floor:16/16 commands exit0. Latency: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS
```text
GATE_FARM_PROBE verdict=clear waited=0s load1=16.32 load5=17.76 load15=33.58 cpus=20 threshold=50 max_wait=600s
GATE_FARM_PROBE window readings=10 max_load1=23.42 over_threshold=0 threshold=50
```
diff_tests_executed:
  TestHumaStoreErrorContentionContract PASS (integration-packages-34d98506a9035fc5c189.log, unit-41b0147122a5a347bae6.log)
  TestSessionCloseDeleteContentionResponse PASS (integration-packages-6b1b44be6472158c6503.log, unit-5aa4195c8c3f823721e5.log)
  TestBdStoreCASEmulationTelemetry PASS (integration-packages-ca921cfed4d2f2c83e9a.log, unit-a8370ad49836590ee98a.log)
  TestBdStoreConditionalWriteTelemetry PASS (integration-packages-4272944fec738099e4c6.log, unit-a1b3b4e736188a37ea5e.log)
  TestBdStoreWriteConflictTelemetry PASS (integration-packages-87d512276f4a18fb3ce6.log, unit-66f4b99cada8b0d076f0.log)
  TestBdWriteTarget PASS (integration-packages-271121d3690ce0b495f0.log, unit-9ad70be3bebc0e3d2b67.log)
  TestIsRetryableConflictContract PASS (integration-packages-33357424fd64a982d40f.log, unit-83844def8572ed2de159.log)
  TestNativeDoltConditionalWriteTelemetryOperations PASS (integration-packages-d7801e72f71a1b743531.log, unit-6052d757bfbd44a5dfc9.log)
  TestNativeDoltStoreMetadataConflictThenVersionMismatchStops PASS (integration-packages-4f861baefb334d73c807.log, unit-66f4b99cada8b0d076f0.log)
  TestNativeDoltWriteConflictTelemetry PASS (integration-packages-4f861baefb334d73c807.log, unit-a62a933afd2c8a03b505.log)
  TestRecordWriteBudgetDecisions PASS (integration-packages-e08400aacccc1207f710.log, unit-fa8a4a1fb9aed4b2f054.log)
  TestRetryNativeDoltWriteBoundAndRecovery PASS (integration-packages-2bf3b7b7a11201d32f9e.log, unit-a62a933afd2c8a03b505.log)
  TestWriteConflictTelemetryIgnoresPermanentFailureOnLastAttempt PASS (integration-packages-6f3aa0dda0dc94a1d973.log, unit-e9397dd3bec5216581a6.log)
GATE_TEST_EVIDENCE_RESULT ok: 13 diff-owned tests verified by name

failure_attribution:
TestInterruptMidTurnContinuesTheLoop -> ga-5rhk2q3w
  clause1: not diff-owned; actual failure site UNCHANGED, no shared hunks in failure file
  clause2: new first-sighting tracker, gm-sf3238 escape: proof a landed, clauses1/4 clear; not claimed to predate this run
  clause3: a MECHANISM: computed test-binary import closure plus unchanged Bash/Python stub child inputs reaches no changed input
  clause4: no package path overlap, passes unconditionally; first hit for this deploy.
  log: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/logs/unit-7bd450be5325baa9d503.log
TestIdleSeparatedPromptsStaySeparate -> ga-5rhk2q3w
  clause1: not diff-owned; actual failure site UNCHANGED, no shared hunks in failure file
  clause2: new first-sighting tracker, gm-sf3238 escape: proof a landed, clauses1/4 clear; not claimed to predate this run
  clause3: a MECHANISM: computed test-binary import closure plus unchanged Bash/Python stub child inputs reaches no changed input
  clause4: no package path overlap, passes unconditionally; first hit for this deploy.
  log: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/logs/unit-7bd450be5325baa9d503.log
TestGoTestObservableConcurrentRunsKeepDistinctLogs -> ga-37v8laxt
  clause1: not diff-owned; actual failure site UNCHANGED, no shared hunks in failure file
  clause2: existing exact-condition tracker predates this run; opened; sighting appended and read back
  clause3: c COVERAGE: both reachable changed functions initInstruments and RecordWriteConflict measure0%; all other changed packages absent from import closure
  clause4: no package path overlap, passes unconditionally; first hit for this deploy.
  log: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/logs/unit-a0a8796e4c383ef8d5af.log

The focused scripts coverage probe measured initInstruments (99 instrumented lines) and RecordWriteConflict (17) at0%; other changed packages are absent from the test import closure. Its PASS is triage only and does not replace the original FAIL. ZCode uses unchanged Bash/Python stub inputs; its module closure has no changed input. No failed test was retried into PASS.
skip_justification: 672 raw phase/floor SKIP events; 315 sites UNCHANGED;0 unresolved. 35 same-lane pinned-base Bazel groups and 4 exact native-floor controls all measured pre-existing reasons. All672 per-event reasons,site proofs,shared-hunk reach and comparison rows are retained below.
diff_owned_evidence: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/diff-owned-explain.txt
not_owned_skip_evidence: /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/not-owned-skip-evidence.json
Sole shared hunk: internal/beads/write_conflict_telemetry_contract_test.go:1; no HELPER-HUNKS. Measurements cover its importing test binaries. Probe observer v3 reused completed comparisons. Unit-leg2 preserves processwrapper strategy; the final native commands share one load gate like the main native floor. Main C3 was not relaunched.

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.9nxeG5 aa17d25430f1dda4a8a7834e004875961b3f7806 95da871e403db500e002b5624bcc7020b6f70f44 fork; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
```text
diff aa17d25430f1dda4a8a7834e004875961b3f7806...95da871e403db500e002b5624bcc7020b6f70f44: 18 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               MATCH internal/beads/BUILD.bazel, internal/beads/batch_copy.go, internal/beads/bdstore.go
suite_mode: full  (shared filter matched)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```
ci_required_accounting:
changes | LOCAL-PASS | {"log": "/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/closure.txt", "status": "LOCAL-PASS"}
runner-policy | LOCAL-PASS | {"log": "/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/runner-policy.log", "status": "LOCAL-PASS"}
windows-lane | LOCAL-PASS | {"log": "/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/windows-lane-output.txt", "status": "LOCAL-PASS"}
pack-gate | LOCAL-PASS | {"log": "/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/pack-gate-output.txt", "status": "LOCAL-PASS"}
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "credential-provider-windows-reach.txt", "reach": "none", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI"}
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "windows-native-reach.json", "reach": "none", "reason": "non-linux-runner", "runner": "blacksmith-4vcpu-windows-2025", "status": "DEFERRED-TO-PR-CI"}
windows_reach: none, official credential-provider and complete native Windows-test allowlist closures, rc0.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none; host_precondition_gaps: none. MPR must require CI / required plus both native Windows jobs success on the exact PR head before merging.
Windows lane commands: GOOS=windows go build $(cat .github/windows-lane/build.txt); GOOS=windows go test -count=1 -vet=off -run ^$ -exec /bin/true $(cat .github/windows-lane/test-build.txt); in tools/nogo GOOS=windows go build ./analyzers/... and matching test compile; GOOS=darwin go build ./... and matching test compile. All four exact workflow steps exit0.
Pack gate: exact bundled-pin three-attempt CI loop and GC_TEST_GASCITY_PACKS_REGISTRY=main live smoke loop; no failed attempts. scripts/update-bundled-gastown-pack --check; make test-pack-registry-live (both exact Go invocations); rc0. Probe git ls-remote --exit-code https://github.com/gastownhall/gascity-packs.git HEAD; rc0.
runner-policy: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; rc0. Actual Windows runner blacksmith-4vcpu-windows-2025.

Load and runtime receipts:
```text
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.9nxeG5/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.92 run_max_load=16.31 run_mean_load=13.92 run_readings=11 wait_first_load=12.92 wait_max_load=12.92 wait_mean_load=12.92 wait_readings=1 read_errors=0
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.9nxeG5/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.58 run_max_load=139.68 run_mean_load=60.45 run_readings=125 wait_first_load=11.58 wait_max_load=11.58 wait_mean_load=11.58 wait_readings=2 read_errors=0 prewait_s=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1110 wait_timed_out=0 run_start_load=14.85 run_max_load=18.86 run_mean_load=17.80 run_readings=8 wait_first_load=113.85 wait_max_load=113.85 wait_mean_load=40.88 wait_readings=38 read_errors=0
GASCITY_GATE_LEGS leg=1 of=2 start argv="bazel test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json --keep_going -- //... -//internal/connect/tokenrender:tokenrender_test"
GASCITY_GATE_LEGS leg=1 of=2 rc=3 executed="Executed 309 out of 309 tests: 307 tests pass and 2 fail locally." argv="bazel test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json --keep_going -- //... -//internal/connect/tokenrender:tokenrender_test"
GASCITY_GATE_LEGS leg=2 of=2 start argv="bazel test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.leg2.json --keep_going --strategy=TestRunner=processwrapper-sandbox --test_output=all --nocache_test_results -- //internal/connect/tokenrender:tokenrender_test"
GASCITY_GATE_LEGS leg=2 of=2 rc=0 executed="Executed 1 out of 1 test: 1 test passes." pass=122 fail=0 skip=1 argv="bazel test --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.leg2.json --keep_going --strategy=TestRunner=processwrapper-sandbox --test_output=all --nocache_test_results -- //internal/connect/tokenrender:tokenrender_test"
GASCITY_GATE_LEGS verdict=FAIL rc=3 leg1_rc=3 leg2_rc=0 legs_run=2
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-gate-base-check.mm6yxh1e/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=17.17 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=27.08 wait_max_load=64.77 wait_mean_load=32.87 wait_readings=61 read_errors=0
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-fresh-tree.qcuY1i/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.09 run_max_load=15.39 run_mean_load=14.26 run_readings=4 wait_first_load=12.09 wait_max_load=12.09 wait_mean_load=12.09 wait_readings=1 read_errors=0
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-fresh-tree.j2Hca0/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.43 run_max_load=18.93 run_mean_load=18.29 run_readings=5 wait_first_load=15.26 wait_max_load=15.26 wait_mean_load=14.84 wait_readings=2 read_errors=0
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

Exact Windows job commands (all four steps exit0):
```bash
#!/bin/bash
set -euo pipefail
GOOS=windows go build $(cat .github/windows-lane/build.txt)
printf 'CI_STEP_PASS windows-allowlist-build\n'
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
printf 'CI_STEP_PASS windows-allowlist-tests-compile\n'
(
  cd tools/nogo
  GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
printf 'CI_STEP_PASS nogo-windows-build-and-tests-compile\n'
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
printf 'CI_STEP_PASS darwin-all-build-and-tests-compile\n'
```

Retained evidence:
Full execution record retained: 6141 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/execution.json; SHA256 a29d0213b46a0e782a80503c5de95404b71926158499c08142498b02cbaf5071.
Full failure attribution retained: 35 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/failure-attribution.json; SHA256 070c3c7f88f7b237a8756c0410681b18f1ff2e12e57d53dfbd1b68b2ca4b452b.
Full skip inventory with proofs retained: 18528 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/not-owned-skip-evidence.json; SHA256 b41be99eaeaf9c0f10eb875c024741131d06ff673ec6ae34602a5b7ad028e53d.
Full skip site proofs retained: 315 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/local-skip-sites.txt; SHA256 d279e02153d3c1ceddd91b0ece8e90c1fc87e00f81412350d8b3e5bc001ea274.
Full skip audit summary retained: 50 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/skip-audit-summary.json; SHA256 7f26a0afd7d9ad33317dae765e2c70d2729d6e8145ece2022ded0da7626627a5.
Full import closures retained: 9919 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/skip-import-closures.json; SHA256 6a4704f935711d77ad47007173e0ff25e55af4998d60367fa83e02dc2c6ed028.
Full owned-test explanation retained: 45 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/diff-owned-explain.txt; SHA256 dbc5796554994afe93402358a06e05c12dd8346d6b27f10501b9c0ef9dbb14a2.
Full native floor commands retained: 1014 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/floor-topup.json; SHA256 20302f0ae8177b0c8d5c5ed1b91dcab31b92ba1d328950a9464bc991bc5da927.
Full log hashes retained: 2857 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/retained-log-sha256.json; SHA256 7bd4093154aca36005537622a0427010baf68a94c9c2d8d5e1102b13c67c44ba.
Full scripts coverage retained: 783 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/scripts-test-coverage.lcov; SHA256 7d70075cd08d84c9472f14bff98cb813d6e166d69255ff3a2cd0fb8e7bade51b.
Full CI accounting retained: 32 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/accounting.json; SHA256 72cca1c270760365405b8f267d10c2edcc1574c662fc2bdedf9d2d71684fce9d.
Full native baseline load receipt retained: 12 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/native-controls-batch-output.txt; SHA256 26ab2e67129eceb83a28d7a8a069728ca8769667b57e8df1d4876941805ed6fe.
Full Windows lane output retained: 358 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/windows-lane-output.txt; SHA256 24d5de1085d40041e4e3c040697d0898dd6357a11c3c7ccba236750a9b068d3c.
Full pack gate output retained: 18 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/pack-gate-output.txt; SHA256 48e4077d8ee0ec33ff4f1c6fc23fb68d4b005aeacc8a1b9032d663ce8fde1090.
Full exact pack job script retained: 26 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.ci-required/pack-gate.sh; SHA256 6885681240a4467607f56aed1dda7e2a021abdc02d9640c9c0ccb52e81b39f08.
Full acceptance audit retained: 7 lines; /var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/analysis/acceptance-audit.txt; SHA256 104ca1908f5c4173277601192d0172eb1ed5931382b78e19638b95ff1a428d32.

Compact execution record (raw counts and pending-attribution fields preserved):
```json
{"test_exec_mode":"local","fallback_reason":"transport-failure","exit_code":80,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local","gate_base":"aa17d25430f1dda4a8a7834e004875961b3f7806","gate_head":"95da871e403db500e002b5624bcc7020b6f70f44","admission":{"class":"local-heavy","floor_gib":40.0,"free_gib":53.35713577270508,"waited_seconds":0.001,"cap":3,"prewait_s":0.013},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"abandoned":false,"head":"95da871e403db500e002b5624bcc7020b6f70f44","base":"aa17d25430f1dda4a8a7834e004875961b3f7806"},"test_cmd":["/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-local-bazel-run.py","--leg-dir","/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local","--owned","/var/tmp/gc-gate-ga-0oh7fg94.vprk988y/diff-owned.txt"],"criterion3_valid":false,"runner":"bazel","icu74":{"verdict":"OK","provenance":"recorded","lines":["GATE_ICU74 lib=libicui18n.so.74 file=\"/usr/local/lib64/icu74-compat/libicui18n.so.74.2\" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes","GATE_ICU74 lib=libicuuc.so.74 file=\"/usr/local/lib64/icu74-compat/libicuuc.so.74.2\" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes","GATE_ICU74 lib=libicudata.so.74 file=\"/usr/local/lib64/icu74-compat/libicudata.so.74.2\" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes","GATE_ICU74 verdict=OK provenance=recorded sonames=\"libicui18n.so.74 libicuuc.so.74 libicudata.so.74\""]},"latency_split":{"tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"skip_regex":"^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","package":"./test/integration","tags":"integration","threshold":50,"hold_max":600,"poll":10,"attempts":2,"probe":"/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-farm-probe.sh","probe_text_sha256":"7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27","go_timeout":"30m"},"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":0.0,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=16.32 load5=17.76 load15=33.58 cpus=20 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=10 max_load1=23.42 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 21.74 18.87 33.11 22/5652 2033199"],"window":{"readings":10,"max_load1":"23.42","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"bazel_chain":{"schema":1,"runner":"gate-local-bazel-run.py","clean":true,"errors":[],"phases":{"unit":{"argv":["make","-k","check","BAZEL_FLAGS=--config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json"],"rc":2,"start_ms":1795673357574,"end_ms":1795674085126,"log":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.make.log","bep":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json","errors":[],"results":426,"passed":424,"failed":2,"strategies":{"linux-sandbox":426}},"unit-leg2":{"argv":["make","-k","check","BAZEL_FLAGS=--config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.json"],"rc":2,"start_ms":1795673357574,"end_ms":1795674085126,"log":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.make.log","bep":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/unit.bep.leg2.json","errors":[],"results":1,"passed":1,"failed":0,"strategies":{"processwrapper-sandbox":1}},"acceptance":{"argv":["/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh","test","--config=fork-cache","--config=ci","--rewind_lost_inputs","--remote_download_outputs=toplevel","--keep_going","--nocache_test_results","--modify_execution_info=GoTestRun=+no-cache","--test_env=GO_TEST_WRAP_TESTV=1","--build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/acceptance.bep.json","--config=acceptance","//test/acceptance:acceptance_test","//test/acceptance:acceptance_solo_tests"],"rc":0,"start_ms":1795674085576,"end_ms":1795674391212,"log":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/acceptance.stdout","bep":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/acceptance.bep.json","errors":[],"results":25,"passed":25,"failed":0,"strategies":{"linux-sandbox":25}},"integration-packages":{"argv":["/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh","test","--config=fork-cache","--config=ci","--rewind_lost_inputs","--remote_download_outputs=toplevel","--keep_going","--nocache_test_results","--modify_execution_info=GoTestRun=+no-cache","--test_env=GO_TEST_WRAP_TESTV=1","--build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-packages.bep.json","--config=integration","//test:integration_packages"],"rc":0,"start_ms":1795674391219,"end_ms":1795674933699,"log":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-packages.stdout","bep":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-packages.bep.json","errors":[],"results":87,"passed":87,"failed":0,"strategies":{"linux-sandbox":87}},"integration-full":{"argv":["/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh","test","--config=fork-cache","--config=ci","--rewind_lost_inputs","--remote_download_outputs=toplevel","--keep_going","--nocache_test_results","--modify_execution_info=GoTestRun=+no-cache","--test_env=GO_TEST_WRAP_TESTV=1","--build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-full.bep.json","--config=integration","--test_arg=-test.v","--test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","//test/integration:integration_test"],"rc":0,"start_ms":1795674933794,"end_ms":1795675671793,"log":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-full.stdout","bep":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/integration-full.bep.json","errors":[],"results":15,"passed":15,"failed":0,"strategies":{"linux-sandbox":15}}},"make":{"rc":2,"kind":"clean","legs":{"verdict":"FAIL","legs_run":2,"notes":[]}},"floor_rc":0,"owned_topups":[],"left_to_latency_leg":[],"jobs":554,"failed_jobs":["unit-7bd450be5325baa9d503","unit-a0a8796e4c383ef8d5af"],"skipped_phases":[],"config":"fork-cache","skip_regex":"^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","output_base":"/home/jaword/.cache/bazel/_bazel_jaword/33ceff5f6dee8374f01a5ca6c53ddf89","legs":"/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh"},"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":36141}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/floor-logs"},"owned_topup":[],"skip_inventory":{"counts_by_reason_class":{"other":513,"environment-absence":85},"top_level_counts_by_reason_class":{"other":255,"environment-absence":40},"tests":{"elided_array":{"items":598,"bytes":213408}}},"restart":{"complete":true,"declared":554,"started":554,"ok":552,"failed":2,"named":3,"holes":0,"reasons":[],"exit_code":1,"for_this_exit_code":true,"restart_json":"/var/tmp/gc-heavy-gate/runs/ga-0oh7fg94.c3/evidence/local/restart.json"},"farm_test_seconds":4610.837,"pending_test_attribution":[{"test":"TestInterruptMidTurnContinuesTheLoop","job":"unit-7bd450be5325baa9d503","log":"unit-7bd450be5325baa9d503.log"},{"test":"TestIdleSeparatedPromptsStaySeparate","job":"unit-7bd450be5325baa9d503","log":"unit-7bd450be5325baa9d503.log"},{"test":"TestGoTestObservableConcurrentRunsKeepDistinctLogs","job":"unit-a0a8796e4c383ef8d5af","log":"unit-a0a8796e4c383ef8d5af.log"}],"criterion3_valid_if_attributed":true,"restart_exit_code":1,"test_counts":{"PASS":159531,"FAIL":3,"SKIP":614},"diff_tests_executed":"diff_tests_executed:\n  TestHumaStoreErrorContentionContract PASS (integration-packages-34d98506a9035fc5c189.log, unit-41b0147122a5a347bae6.log)\n  TestSessionCloseDeleteContentionResponse PASS (integration-packages-6b1b44be6472158c6503.log, unit-5aa4195c8c3f823721e5.log)\n  TestBdStoreCASEmulationTelemetry PASS (integration-packages-ca921cfed4d2f2c83e9a.log, unit-a8370ad49836590ee98a.log)\n  TestBdStoreConditionalWriteTelemetry PASS (integration-packages-4272944fec738099e4c6.log, unit-a1b3b4e736188a37ea5e.log)\n  TestBdStoreWriteConflictTelemetry PASS (integration-packages-87d512276f4a18fb3ce6.log, unit-66f4b99cada8b0d076f0.log)\n  TestBdWriteTarget PASS (integration-packages-271121d3690ce0b495f0.log, unit-9ad70be3bebc0e3d2b67.log)\n  TestIsRetryableConflictContract PASS (integration-packages-33357424fd64a982d40f.log, unit-83844def8572ed2de159.log)\n  TestNativeDoltConditionalWriteTelemetryOperations PASS (integration-packages-d7801e72f71a1b743531.log, unit-6052d757bfbd44a5dfc9.log)\n  TestNativeDoltStoreMetadataConflictThenVersionMismatchStops PASS (integration-packages-4f861baefb334d73c807.log, unit-66f4b99cada8b0d076f0.log)\n  TestNativeDoltWriteConflictTelemetry PASS (integration-packages-4f861baefb334d73c807.log, unit-a62a933afd2c8a03b505.log)\n  TestRecordWriteBudgetDecisions PASS (integration-packages-e08400aacccc1207f710.log, unit-fa8a4a1fb9aed4b2f054.log)\n  TestRetryNativeDoltWriteBoundAndRecovery PASS (integration-packages-2bf3b7b7a11201d32f9e.log, unit-a62a933afd2c8a03b505.log)\n  TestWriteConflictTelemetryIgnoresPermanentFailureOnLastAttempt PASS (integration-packages-6f3aa0dda0dc94a1d973.log, unit-e9397dd3bec5216581a6.log)\nGATE_TEST_EVIDENCE_RESULT ok: 13 diff-owned tests verified by name\n"}
```

Landing handoff: mayor owns ga-bygzs8i0. After this PR lands, bind the squash/tree/work record to main, inspect recordWriteBudget, all retry-loop wiring and contract tests, close that landing record and notify gascity/pm --notify with ga-bygzs8i0 and ga-4iy5mmmh.

## Publication hook attribution

The normal pre-push at5ba91616bc38b51b437bc30503852ba666779f03 (reviewed source plus this record) ran `bazel test //... --config=remote-exec --keep_going`:307/310 targets passed and3 failed locally; no push landed. This is separate evidence from criterion3 above. Six top-level failing tests, grouped by three conditions:

| Failed tests | Condition tracker | Clause3 proof |
|---|---|---|
| TestRepositoryLedgerMatchesCensusAndDocumentation; TestRepositoryCountsFileIsCanonicalAndDerivesEveryCountedRow | ga-bxcuiz6j | (a) complete binary import closure has no changed non-test package; source-data audit below |
| TestSchemaFreshness (city-schema.json, config.md) | ga-6a24iyhf | (c) all24 reachable changed functions instrumented, zero coverage |
| TestInitScrubsLeakVectors; TestInitPassthroughPreservesNamed; TestInitRefusesProdDoltPort | ga-2fv9v6z6 | (c) all24 reachable changed functions instrumented, zero coverage |

Clause1 for all six: actual failure sites census_test.go:2444, counts_test.go:1202, docsync_test.go:532, testenv_test.go:50/146/532 read UNCHANGED, func_touched=no, shared_hunks_in_file=0. The sole SHARED-HUNKS entry is an import/declaration hunk in a Beads test file; dependency test files do not compile into any failing binary, and it cannot reach these tests.
Clause2: six exact-name open searches empty;151 open condition trackers read with no matching condition;122 closed trackers read. Census ledger test has unrelated UNVERIFIED historical ga-yihql5 (subprocess705/704 growth), linked on the new census tracker; the other five names return NONE. Open ga-5550q2dx covers expired historical waiver dates, a different condition. Each new tracker is OPEN and read-back verified, labelled gate-tracker only, unrouted/unassigned. New first-sighting gm-sf3238 escape: proof3 landed and clauses1/4 clear; no claim that a new id predates the observation.
Clause4 for all six: no failing-test package/directory overlaps the changed API,Beads or telemetry packages. No same-package or inconclusive exception is used. No signature blocked this deploy twice.

Census source-data mechanism: its unchanged AST scanner reports31 missing runnable owners, all in unchanged packages/files; none of the changed test declarations can remove those owners. Their source declarations and currently materialized runfiles were inspected (not asserted to describe the original action's entire filesystem). No policy,ledger/counts or scanner input in that failing owner set changed. Census/ledger/counts and schema generator/config/reference inputs are byte-identical between reviewed source and the passed merged validation tree. Root causes of the diagnostics remain NOT_ENOUGH_INFO; no generic host-load attribution is claimed.

Coverage probe: fresh committed view of the exact pre-push head, through the isolation wrapper, remote-class load gate and detached service. Command `bazel coverage --config=remote-exec --noremote_local_fallback --combined_report=lcov --instrumentation_filter='^//internal/(beads|telemetry)[/:]' --test_filter='^(TestSchemaFreshness|TestInitScrubsLeakVectors|TestInitPassthroughPreservesNamed|TestInitRefusesProdDoltPort)$' --test_sharding_strategy=disabled --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all //test/docsync:docsync_test //internal/testenv:testenv_test`; exit0, both target logs confirm selected names actually ran, both targets remote. All24 changed functions in Beads/telemetry have measured instrumented lines and zero covered lines; API is absent from both full import closures. Supplementary passing probes establish reachability only; original pre-push failures and raw criterion3 counts remain unchanged.
`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=15.97 run_max_load=17.00 run_mean_load=17.00 run_readings=1 wait_first_load=15.97 wait_max_load=15.97 wait_mean_load=15.97 wait_readings=1 read_errors=0`

Pre-push attribution uses the standing authorization in non-diff-owned-gate-failure: record the six attributions, then push this reviewed code plus release-record-only commits with --no-verify. No product code, baseline, shared Git template or test result was changed. Before that push, guards re-check branch identity, reviewed ancestry, record size, clean worktree and that all additions since the reviewed source are this gate record only.

Retained publication proof directory: /var/tmp/gc-gate-ga-0oh7fg94.vprk988y/prepush-attribution.
coverage-proof.json: SHA256 54ccd9676dcb26f4ec660ea9e40619563056f49b40d7e884bac828a76b59432d.
coverage.lcov: SHA256 23719bcd72e0d14d19648a007c24eaaa26d1a2154c7fa5dab2421850d19c63d4.
ownership.txt: SHA256 e2d57d4fd9bc0a4aa601b0a00bc2f5fc4d53d9b40bb99d03d4cf0779b5da4c15.
closed-trackers.txt: SHA256 1020a6ec186d97fd35a6429c99dfdb190423cb00522de052434cf6639455cff1.
tracker-ids.json: SHA256 efb3f4b028cb4402250ec497581881b5c86901538f03826831ca4fa115858772.
missing-owners-source-inspection.json: SHA256 a77c595cd6e8163cc46ce81fc995ebb5ab502679dd24a0744fc0e61f37ec3492.
