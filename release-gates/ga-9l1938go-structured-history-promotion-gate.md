# Structured history promotion test release gate

**Verdict:** **PASS**

- Deploy bead: `ga-9l1938go`; build: `ga-ieeks88`; review: `ga-den7ewux`.
- Reviewed commit: `e814e8705a8bd6799933de23a263bfb0f891c77f`.
- Gated merged commit: `50083e7e6d1df20c8f7f80fb7d4c84d16774175c`; tree: `932b35c6316e838326d56f7a7219211cf630e552`.
- Evidence archive: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9l1938go/pass-20261118T052528Z-c7cip7ze` (1759 files, SHA-256 manifest verified).

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Reviewer PASS names the exact resolved reviewed commit; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS | Test-only implementation matches all five build criteria; original RED evidence retained, both promotion tests and both surfaces independently repeated 200 times: 1,200 PASS, 0 FAIL, 0 SKIP. |
| 3 | Tests and CI accounting | PASS | Complete five-phase suite plus 16 native checks; one non-owned failure attributed below. Six CI leaf jobs accounted; Windows native execution deferred to PR CI. |
| 4 | No unresolved HIGH findings | PASS | Zero HIGH/major findings; one nonblocking comment wording finding. |
| 5 | Clean branch | PASS | Reviewed source and gated merge scratch clean before adding this record; publication requires a clean index/worktree after this commit. |
| 6 | Merge state and build/vet | PASS | Clean merge at the pinned base; full remote build and nogo vet completed successfully, with raw BEP terminals verified. |
| 7 | Single feature theme | PASS | Two API test files only: reliable promotion from pane fallback to structured transcript history. No production, BUILD, or CI changes. |

## Merge and static evidence

merge_state: clean (origin/main@9039b3a61f0d70ef3ca9f03838bfd7b14e0d0ebe)
gate_base_kind: current-tip
gate_base_sha: 9039b3a61f0d70ef3ca9f03838bfd7b14e0d0ebe
merge_head: 50083e7e6d1df20c8f7f80fb7d4c84d16774175c
merge_tree: 932b35c6316e838326d56f7a7219211cf630e552
merge_validation: merge authority rechecks the current PR head, merge state and required CI at merge time; this gate records its pinned tree.
deploy_mode: remote; push_remote: origin; heavy_mode: none; stack: none
build_vet: bazel build --config=remote-exec --keep_going --noremote_local_fallback //...; same with --output_groups=nogo_fix; both exit 0 (c6-remote-{build,vet}.bep.json).
policy_lane: PASS — shell release/routed-row/topology/residency/hook guards; 14 policy/format Bazel targets; full-tree nogo covered by criterion 6.
drift_lane: PASS — 7 generated-file targets, OpenAPI sync, make bazel-sync plus empty git diff, make dashboard-check and npm preview smoke. Each lane used its own fresh tree and gate-base.sh run.
ci_lane_run: n/a (no CI configuration change).

## Full-suite evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-9l1938go.l5Tjq81f/merge --base 9039b3a61f0d70ef3ca9f03838bfd7b14e0d0ebe --bead-json /var/tmp/deploy-ga-9l1938go.l5Tjq81f/bead.json --owned /var/tmp/deploy-ga-9l1938go.l5Tjq81f/owned.tsv --run-dir /var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2
test_cmd_scope: full-suite
test_exec_mode: remote-exec (required native floor recorded separately)
test_counts: 155342 PASS, 1 FAIL, 613 SKIP (top-level result lines; subtests excluded).
test_log_dir: evidence archive c3/remote/logs; 557 retained per-job logs; gate-test-evidence.py independently re-read them.

| Phase | Remote results | Raw verdicts |
|---|---:|---|
| unit | 415 | 414 PASS, 1 FAIL (attributed) |
| acceptance | 25 | 25 PASS |
| integration-packages | 86 | 86 PASS |
| integration-full | 14 | 14 PASS |
| integration-latency | 1 | 1 PASS |

test_exec_bep: c3/remote/<phase>.bep.json — every result remote, no cached flags, attempt 1, phase window verified. Successful terminal for every later phase; unit terminal TESTS_FAILED is retained.
rbe_state: eligible own-code provenance, worker-env pin matches base, certificate valid; fallback_reason: none.
admission: remote; floor_gib=25; free_gib=46.603363037109375; cap=2; waited_seconds=0.006.
farm_test_seconds: 12982.143

Commands and flags actually used (unit = make -k check; acceptance includes acceptance_test and acceptance_solo_tests; integration packages/full/latency use their canonical targets):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9l1938go.l5Tjq81f/c3-run/evidence-2/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
- TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect: PASS (unit and integration packages).
- TestSessionStreamStructuredPromotesHistoryWrittenBeforeFirstPoll: PASS (unit and integration packages).
- Both city-huma/legacy subtests PASS; supplemental configured remote run reports each of all six result names PASS=200, FAIL=0, SKIP=0.

latency_leg: complete, measured under a clear farm probe (wait30s, threshold50, load1=45.04); both budget tests PASS, no latency test in the main integration leg.
floor_topup: complete, 16/16 steps exit0; beadstest/native environment-absence top-ups, Darwin compile and both runner self-tests retained in c3/remote/floor-logs.
owned_topup: none required.
load_threshold: 15; load_waited_seconds: 60; load_wait_timed_out: 0; run_start_load: 14.67; run_max_load: 14.40; run_mean_load: 13.62; run_readings: 6 (native floor).
remote_driver_host_load: threshold15/wait0/timedout0/start70.70/max87.46/mean48.91/readings45; context only, not farm load and not a native test run.

## Failure attribution and skipped tests

failure_attribution: TestGoTestObservableConcurrentRunsKeepDistinctLogs -> ga-37v8laxt; clause 3(a) MECHANISM.
- Clause 1: not owned; recomputed owned list names only the two API tests. Failure site scripts/go_test_observable_test.go:985 is UNCHANGED.
- Clause 2: opened matching tracker created 2026-11-11, before this run; exact expired.jsonl concurrent-prune race. Sighting comment a4ce7e79-8aba-5f7b-be77-9188b6be2227 read back.
- Clause 3: scripts test import closure has zero changed non-test inputs; API _test.go code is absent from the scripts binary and its wrapper subprocess.
- Clause 4: scripts and internal/api have no path overlap. First hit for this deploy; no failed-test retry.
- Raw execution remains exit80, criterion3_valid=false and criterion3_valid_if_attributed=true. Independent attribution is bound to its SHA-256 in attribution-approved.json; counts are unchanged. waiver_ref: none.

skip_inventory: 599 reason records (509 other, 90 environment-absence), retained verbatim; raw SKIP tally613 also includes native/top-up events.
skip_justification: all325 resolved source sites UNCHANGED, zero unlocated rows. Other binaries cannot import the changed API test files. Platform/provider limitations, subprocess helpers, opt-in/manual measurements and unavailable native facilities are inventoried; all environment-absence parents received the required native top-up.
The3 API signal-test SKIPs match the pinned base exactly: pre-existing3/new0/reason-changed0/diff-owned0. The base control deliberately skipped all3 bodies, ran in linux-sandbox, and is not credited as remote/full-suite execution.
SHARED-HUNKS: one atomic-writer hunk; eight promotion-file hunks (sync import, shared promotion helper, gate type/factory/Peek/Pending/Release). Only the two owned tests reference those helpers; no init/TestMain edit. Base comparison independently confirms the three untouched API skips.

## Required CI closure

ci_required_closure: /usr/bin/python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-9l1938go.l5Tjq81f/merge 9039b3a61f0d70ef3ca9f03838bfd7b14e0d0ebe e814e8705a8bd6799933de23a263bfb0f891c77f origin
Actual-origin PR classification is byte-identical to the earlier fork plan; the same six exact-tree job accounts apply. The final push probe confirmed origin writable.
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.

| Job | Account | Evidence |
|---|---|---|
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| changes | LOCAL-PASS | exit0; closure.txt |
| runner-policy | LOCAL-PASS | exit0; runner-policy.log |
| windows-lane-test | DEFERRED-TO-PR-CI | non-linux-runner; exact job test import closure, changed paths none |
| windows-lane | LOCAL-PASS | exit0; own workflow commands in windows-lane.sh and terminal logs |

windows_reach: windows-lane-test none (GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps over .github/windows-lane/test.txt; exact module-package closure).
ci_windows_compile: own job commands, fresh view plus isolation, exit0; Windows build/test compile, nogo Windows compile, and all-package macOS build/test compile. No native Windows test execution claimed.
ci_load: threshold15; waited630s; timedout0; run_start13.77/max15.29/mean14.48/readings3.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge only after CI / required is green on the PR head and the deferred Windows job succeeds (or its own path filter is false).

## Retained interruptions

Original c3-run/evidence was an incomplete environment run: Bazel server lost during full integration, no terminal BEP; no result credited or stitched into this gate. One fresh complete canonical run supplied all evidence above. Two supplemental setup-only attempts were scoped-stopped before any testResult; their logs remain, and neither was a test verdict. The successful configured supplemental has separate evidence.

The named PROJECT_MANIFEST file is absent from this tree; the supplied release-gate contract and canonical TESTING.md define the evaluated criteria.
