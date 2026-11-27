**Verdict:** **PASS**

# Claude-layout transcript discovery for the kill guard

Deploy bead: ga-21uo910q. Build: ga-qnqmzy.1. Review: ga-3ggcts4u.
Reviewed source: `f586815e4f86399c0f7636098cece816ac12d827`.
Tested merge: `4bcdd4ba703c62b7feb5a4fae4f7ac9a768eb986`.
Gate base: `cf5b4ad953f2b9a9b14156d88a461fed0e201510`; deploy_mode: remote.
The publication adds this record as one child of the reviewed source.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-3ggcts4u records PASS for this exact resolved source; no carryover or reviewer-added-test transformation. |
| 2 | Acceptance criteria met | PASS | The kill guard selects a Claude-layout transcript view; six owned tests executed by name cover the five acceptance cases and the generic lookup contrast below. |
| 3 | Full tests and required lanes | PASS | Five fresh remote phases plus 16 native floor checks; 159612 PASS / 0 FAIL / 618 SKIP result lines. All owned tests PASS; skipped tests justified below. CI-required closure fully accounted. |
| 4 | No unresolved HIGH review finding | PASS | Reviewer style/security/spec verdict PASS; zero unresolved HIGH findings. |
| 5 | Final source tree clean | PASS | Reviewed checkout clean before adding this record; the record is the only publication change. Publication notes verify the committed branch is clean. |
| 6 | Merge state and build/vet | PASS | Clean merged tree built with full Bazel build and nogo; rc=0. |
| 7 | Single feature theme | PASS | Four source commits, seven files: one kill-guard lookup change spanning session and worker. No independent theme or denylisted path. |

merge_state: clean (origin/main@cf5b4ad953f2b9a9b14156d88a461fed0e201510)
merge_validation: merge result built and vetted here
gate_base_kind: current-tip
gate_base: cf5b4ad953f2b9a9b14156d88a461fed0e201510
reviewed_sha_verified: f586815e4f86399c0f7636098cece816ac12d827
merge_head: 4bcdd4ba703c62b7feb5a4fae4f7ac9a768eb986
scope_guard: PASS; accepted citations ga-21uo910q and ga-qnqmzy.1; no stack
preflight: no PR associated with the reviewed source, checked again before publication

## Acceptance evidence

- Empty provider with 200 FIFO Codex decoys: TestInFlightBackgroundSubagents_DoesNotOpenCodexRollouts/provider= PASS.
- Auto provider with the same decoys: TestInFlightBackgroundSubagents_DoesNotOpenCodexRollouts/provider=auto PASS.
- Claude protection by key and cwd slug, including bare start_command/empty provider: TestInFlightBackgroundSubagents_FindsClaudeTranscriptForEveryProviderShape PASS with all six subtests.
- Missing transcript remains fail-open: TestInFlightBackgroundSubagents_NoTranscriptIsFailOpen PASS.
- A matching Codex rollout supplies no Claude guard evidence: TestInFlightBackgroundSubagents_IgnoresMatchingCodexRollout PASS.
- Generic transcript discovery remains available to other callers: TestSessionHandleTranscriptPath_GenericLookupStillFindsCodexRollout and TestTranscriptPathClaudeLayoutNeverReadsCodexTree PASS.

diff_tests_executed:
  TestTranscriptPathClaudeLayoutNeverReadsCodexTree PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestInFlightBackgroundSubagents_DoesNotOpenCodexRollouts PASS (unit-481d1d821b2235b34d5b.log)
  TestInFlightBackgroundSubagents_FindsClaudeTranscriptForEveryProviderShape PASS (unit-481d1d821b2235b34d5b.log)
  TestInFlightBackgroundSubagents_IgnoresMatchingCodexRollout PASS (unit-481d1d821b2235b34d5b.log)
  TestInFlightBackgroundSubagents_NoTranscriptIsFailOpen PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionHandleTranscriptPath_GenericLookupStillFindsCodexRollout PASS (unit-481d1d821b2235b34d5b.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name

## Full-scope execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: 159612 PASS, 0 FAIL, 618 SKIP
test_log_dir: /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/logs
Counts sum top-level result lines per job; they are not a count of distinct test functions.
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/merge --base cf5b4ad953f2b9a9b14156d88a461fed0e201510 --bead-json /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/bead.json --owned /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/owned.tsv --run-dir /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence
The runner executed make -k check, acceptance, integration-packages, full integration minus the two latency tests, and the separate latency lane. The native floor and its environment-absence top-ups completed 16/16 commands.

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

heavy_mode: none
ci_lane_run: n/a (no CI configuration change)
waiver_ref: none (Gas City has no waiver path)
failure_attribution: none (zero failed test verdicts)
known_red_precheck: proceed; check-routed-test-rows passed; ga-1ptv4jey reported stale, not closed by this gate

## Fast lane

Full build/vet: bazel build --config=remote-exec --noremote_local_fallback --keep_going //...; rc=0; 1505 targets, 22699 actions, 230.226 seconds.
policy_lane: make lint with remote-exec/nogo_fix plus every shell guard from make check; PASS.
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec, then git diff --exit-code; PASS.
fast_lane: PASS before any suite phase.

```text
format: PASS; log=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/format.output
GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/merge head=4bcdd4ba703c tree=58488f2e7135 dir=/var/tmp/gc-fresh-tree.Jj2Blo modified=0 untracked=0 ignored=2 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/
GATE_BASE_RECORD base=cf5b4ad953f2b9a9b14156d88a461fed0e201510 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.Jj2Blo
policy: PASS; log=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/policy.output
FRESH_TREE_RECORD from=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/merge head=4bcdd4ba703c tree=58488f2e7135 dir=/var/tmp/gc-fresh-tree.PKni4S modified=0 untracked=0 ignored=2 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/
GATE_BASE_RECORD base=cf5b4ad953f2b9a9b14156d88a461fed0e201510 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.PKni4S
drift: PASS; log=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/drift.output
FRESH_TREE_RECORD from=/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/merge head=4bcdd4ba703c tree=58488f2e7135 dir=/var/tmp/gc-fresh-tree.2d3WHj modified=0 untracked=0 ignored=2 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/
GATE_BASE_RECORD base=cf5b4ad953f2b9a9b14156d88a461fed0e201510 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.2d3WHj
```

## Skip justification and load

All six owned tests PASS, with no owned FAIL or SKIP.
The skip audit covers 676 events including subtests/native runs, at 319 unique sites; every site is unchanged and none is unresolved.
The new worker test helpers are the only SHARED-HUNKS. Test-binary import closures select 427 events: all 387 remote events matched pinned-base reason/status across seven actual target/config groups, and all 40 native events matched across two packages. Every comparison completed with rc=0.
The other 249 events have no changed shared helper in their test-binary closure. Their retained reasons are unchanged platform/privilege, unavailable tool/fixture, explicit manual/live-run gate, or unsupported provider-fixture cases. The three session skip sites are OS/orphan-fixture guards, independent of transcript lookup. Environment-absence skips received the prescribed native top-up; remaining non-owned skips stay inventoried.
Full reason/site, closure, and comparison records are retained below.

load_threshold: 15
load_waited_seconds: 601
load_wait_timed_out: 1
run_start_load: 62.03
run_max_load: 56.8
run_mean_load: 47.02
run_readings: 7
These load fields describe the native floor. Its bounded wait timed out; all native checks completed successfully. The farm latency lane was measured separately under probe clear; both named budget tests PASS.

## CI-required job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/merge cf5b4ad953f2b9a9b14156d88a461fed0e201510 f586815e4f86399c0f7636098cece816ac12d827 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
closure: 6 LEAF jobs; aggregators take no status

| Job | Status | Command/result evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/ci-runner-policy.output; rc=0 |
| changes | LOCAL-PASS | /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/ci-closure.txt; rc=0 |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/ci-windows-lane.output; rc=0 |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/windows-native-reach.json |

windows_reach: windows-lane-test = none; its own GOOS=windows test-binary closure over .github/windows-lane/test.txt contains no changed input (/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/windows-native-reach.json).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
The Windows lane account is compilation evidence. Native Windows test results and CI / required must be green on the PR head before merge. Gate PASS authorizes PR opening; merge authority is MPR.

## Compact execution record

The full record retains test_exec_bep, rbe_state, admission, farm_test_seconds, skip_inventory, owned_topup, floor_topup, latency_leg and the shadow delta plan. Large arrays below are elided by the prescribed compactor; nothing in the retained record was rewritten.

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote","gate_base":"cf5b4ad953f2b9a9b14156d88a461fed0e201510","gate_head":"f586815e4f86399c0f7636098cece816ac12d827","admission":{"class":"remote","floor_gib":25.0,"free_gib":51.592525482177734,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"cf5b4ad953f2b9a9b14156d88a461fed0e201510","head":"f586815e4f86399c0f7636098cece816ac12d827","merge":"4bcdd4ba703c62b7feb5a4fae4f7ac9a768eb986","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"65d5d9c32de9004c8577b1445010a35145f64f636badef0b29a53d39cf167bb6"},"test_exec_bep":{"unit":{"results":427,"remote_executed":427,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13322.096,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":104.108,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=77.83 load5=86.24 load15=47.04 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=47.17 load5=78.00 load15=45.54 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=9 max_load1=47.17 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.66 71.06 44.27 18/954 56098"],"window":{"readings":9,"max_load1":"47.17","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38935}},"log_dir":"/var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/c3/evidence/remote/floor-logs","load":{"threshold":15,"waited_seconds":601,"wait_timed_out":1,"run_start_load":62.03,"run_max_load":56.8,"run_mean_load":47.02,"run_readings":7,"wait_first_load":59.65,"wait_max_load":85.51,"wait_mean_load":68.64,"wait_readings":21,"read_errors":0},"remote_slot":{"released":true,"outcome":"released"}},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218622}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159612,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestTranscriptPathClaudeLayoutNeverReadsCodexTree PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestInFlightBackgroundSubagents_DoesNotOpenCodexRollouts PASS (unit-481d1d821b2235b34d5b.log)\n  TestInFlightBackgroundSubagents_FindsClaudeTranscriptForEveryProviderShape PASS (unit-481d1d821b2235b34d5b.log)\n  TestInFlightBackgroundSubagents_IgnoresMatchingCodexRollout PASS (unit-481d1d821b2235b34d5b.log)\n  TestInFlightBackgroundSubagents_NoTranscriptIsFailOpen PASS (unit-481d1d821b2235b34d5b.log)\n  TestSessionHandleTranscriptPath_GenericLookupStillFindsCodexRollout PASS (unit-481d1d821b2235b34d5b.log)\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```

Full execution.json retained: 6084 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/runs/ga-21uo910q.c3/evidence/execution.json; SHA256 3bda101d9295b9c3a5b8e90e54f979f043bbd8b840d7f2e39828437c5348700e.
Full all skip events retained: 5410 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/all-skip-events.json; SHA256 daaf7efd8b50f3c52e80fd9fc281627c9bf1dea5f3d95df9cc63da2e39981223.
Full skip-site proof retained: 342 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/skip-proof-batch-1.txt; SHA256 d0449ea5c33b694e6a0fcd8b0ff79855f8ac35105450ecf1e8f3368fcdc752e7.
Full shared-helper import closures retained: 189 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/shared-helper-import-closures.json; SHA256 0c2296a461381b4429e656fb08c576692ee02758eb4287c02f71406a37f4be16.
Full helper-reached skip events retained: 5126 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/shared-helper-skips-needing-base.json; SHA256 5ee87986989edfb0a217833458916153cd88d8ecee10f45136ce292c628a7d4c.
Full remaining skip reasons and sites retained: 1994 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/skip-no-helper-reach.json; SHA256 9dd08ef21f30c85bb59b21d05b63a30c858e29ce641a7c8fea15335bf0fca914.
Full remote base comparisons retained: 975 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/base-skip-comparisons-rbe/results.json; SHA256 cff3933e4dcdc23966eeef3ebd41c8db137ce752e1385cb6b87809d0698c9f6c.
Full native base comparisons retained: 60 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/base-native-skip-comparisons/results.json; SHA256 7fde6e3ee95875ffc0b431faa8d5383d60c84e4f82380323fd0ba1766c31976f.
Full CI accounting retained: 56 lines; /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/ci-accounting.json; SHA256 7e3e38bae9db7c93e9a5415773d77d722235b4690270b891c0339298bc20127c.

Per-job logs are retained permanently under the run directory protected by .gc-no-reap.

prepush_suite: skipped
prepush_suite_c3_evidence: /var/tmp/gc-gate-ga-21uo910q-fresh.sygVwrYH/runs/ga-21uo910q.c3/evidence
