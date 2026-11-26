**Verdict:** **PASS**

# Release gate: nudge status observes named sessions

Bead: ga-47xdfaui. Build refresh: ga-5o558w8l; original: ga-y53wjn. Review: ga-6rkckm6g.

Reviewed source: `9d6266cae782eaebd106a619947680beb41ec4ea`.
Pinned base: `77143ecd18ff6f0199dfee3a412825f1fd9fa8a2`.
Merge result tested: `60c42f2cfc29296637b4a69b5eda00db1041c45d`.
Evidence root: `/var/tmp/gc-gate-ga-47xdfaui.IrkF5Y`.
Suite run: `/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence`.

There is no `docs/PROJECT_MANIFEST.md`; the documented commands come from TESTING.md, Makefile and the merged workflows.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS. Fresh review ga-6rkckm6g; reviewer-added-tests.sh independently verifies the exact parent-to-reviewed-head relationship and only additive changes to cmd/gc/cmd_nudge_test.go. |
| 2 | Acceptance met | PASS. The eight-scenario command fixture and all four owned tests pass by name. Public bug #9509 is created and verified; the prepared PR carries Closes #9509 and end-to-end evidence. Publication is verified immediately after PR creation. |
| 3 | Tests pass | PASS. Full five-phase suite plus 16 native checks: 159347 PASS, 0 FAIL, 618 inventoried SKIP; six required leaf jobs accounted for below. |
| 4 | No unresolved HIGH review finding | PASS. No blocker/major/HIGH remains; the three LOW follow-ups are separately scoped on ga-c3oyx3m8. |
| 5 | Final branch clean | PASS. Source clean; this gate record is the only added file. Committed tip is checked clean before push. |
| 6 | Merge state; build and vet | PASS. Clean merge result; both CLI binaries built with Bazel and nogo, including vet. Detached criterion6 RESULT complete, units=1/1 failed=0. |
| 7 | Single feature theme | PASS. Three commits, seven source/test/registration paths: status target resolution without named-session materialization. Accepted ancestry IDs ga-47xdfaui, ga-5o558w8l, ga-y53wjn; no stack. |

merge_state: clean (origin/main@77143ecd18ff6f0199dfee3a412825f1fd9fa8a2)
gate_base_kind: current-tip
gate_base: 77143ecd18ff6f0199dfee3a412825f1fd9fa8a2
gate_head: 9d6266cae782eaebd106a619947680beb41ec4ea
merge_validation: clean merge result built and vetted; PR CI and MPR retain merge-time authority
heavy_mode: none
waiver_ref: none
ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list change; the integration manifest registers the new test)
reviewer_tests_verified: 49633c27ea692ee8cc26642eb59768c795ac7765 -> 9d6266cae782eaebd106a619947680beb41ec4ea files=cmd/gc/cmd_nudge_test.go

## Acceptance evidence

1. `cmdNudgeStatus` calls `resolveNudgeStatusTarget`, the sole production observe-only resolver caller. It reads the existing session row through `resolveSessionIDWithConfig`, or addresses an unmaterialized/closed configured named session by configured identity without fabricating a persisted session ID. Poll, delivery and mutating callers still pass false. The integration fixture executes the real Cobra command with an available file-backed store and fake provider, never the live factory.
2. `TestCmdGCIntegrationNudgeStatusPreservesAvailableStore`: eight rows PASS (unmaterialized_named, unmaterialized_named_empty_queue, existing_named, existing_named_empty_queue, closed_named, explicit_session, unknown, unavailable_read). Each runs JSON/text/JSON/text, compares complete persisted records including closed rows and queue bytes after every read, trips Create/Update/Reopen/SetMetadata/SetMetadataBatch, and checks provider mutation calls. Bucket IDs, counts, empty arrays, unrelated-agent exclusion and dispatch-skip totals are asserted.
3. The two reviewer-added tests PASS in unit and integration: `TestCmdNudgeStatusReportsAnUnopenedStoreAsUnavailable` checks exit 1, unavailable-store diagnostic and no healthy stdout for text/JSON; `TestResolveNudgeTargetInStoreAnswersAZeroStoreByCaller` keeps the mutating caller's session-not-found error distinct from observation's unavailable-store error. The fourth owned test, `TestDeliverMailNotifyRegisteredCitySupervisorDownRequestsWake`, also PASSes in both tiers.
4. No nudgequeue implementation, queue snapshot/maintenance or lock-wait policy changed. Existing expiration maintenance remains outside this scope (ga-qkksr9); the fixture deliberately uses non-expired items. No claim of universally mutation-free queue maintenance is made.
5. Original RED/GREEN and reviewer mutation evidence are retained on ga-y53wjn/ga-6rkckm6g: making status materialize again fails the unmaterialized and closed named-session rows; deleting the zero-store branch fails both reviewer-added tests. The deploy independently reran all four tests on the merged tree in the full suite. No owning AGENTS.md states a changed invariant/boundary here. Public issue https://github.com/gastownhall/gascity/issues/9509 was verified for URL, title and body; unknown production frequency is marked NOT_ENOUGH_INFO. The PR description explains the behavior, the tradeoff and this fixture evidence and closes that issue.

## Fast lane and evidence isolation

- Criterion6: `isolated-test-run.sh --rbe --bead ga-47xdfaui -- bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501`, exit 0, completed before all other checks.
- Policy: fresh-tree-run.sh, gate-base.sh run and isolated-test-run.sh --rbe around go_format_test, `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary`, then `make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'`: PASS, exit 0. Logs `policy-2/units/gate.w1.*.log`.
- Drift: independent fresh view, same pinned-base wrapper; `make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'` then `git diff --exit-code`: PASS, exit 0. This is the workflow's generated-file drift command. Logs `drift-2/units/gate.w1.*.log`.
- Initial policy/drift launches stopped before executing any check: wrapper path was mistyped (exit 127). The corrected services above ran the actual checks; initial logs remain retained, and no suite ran before their PASS.
- Each FRESH_TREE_RECORD names the same merge head/tree and zero modified/untracked inputs. GATE_BASE_RECORD names the pinned first parent. The lanes did not modify the suite scratch. Canonical isolation-wrapper mode is PASS-THROUGH for the temporary scratch; it pins bd v1.3.4 and noninteractive credentials. The remote runner builds explicit test environments, and the native floor scrubs GC_/BD_/BEADS_/DOLT_ selectors before executing tests; no claim that the pass-through wrapper cloned the scratch is made.
- known-red-precheck: proceed, nothing-known-red; the routed-row guard passes. Classifier: HEAVY_MODE mode=none. ICU74 check OK with recorded loader provenance. Rootless podman /_ping OK, Ryuk-disabled plus opt-out paired. Disk floor 40GiB passed with 48.8GiB free before launch. make check-hooks confirms .githooks active.
- All long work runs in detached systemd services. Raw logs/evidence are retained. The supplementary pinned-base worktree was released and removed after its diagnostic completed; its logs remain.

## SKIP verification

The sole SHARED-HUNKS entry is the new integration-only fixture file: newNudgeStatusReadonlyFixture, seedQueue/records/queueBytes and nudgeStatusMutationSpy methods. There are no HELPER-HUNKS and no edited skip conditions. The fixture is absent from the default cmd/gc build (go list -test: 0 occurrences; integration: 2), and no other file references its private helpers. Therefore the unit binary that skipped cannot execute those helpers.

All remote SKIP records are inventoried in skip-rows.json and skip-inventory.json. Site audit: 604 rows, 318 unique source sites, every site UNCHANGED with line_in_hunk=no and func_touched=no; no unresolved source site. All existing tests in changed files are delimited in diff-owned-explain.txt. The two appended reviewer tests introduce no shared hunk.

The integration package does compile the new fixture, so all 68 cmd/gc SKIPs were rechecked at the pinned base using the same integration/CI/remote configuration and the 36 parent names (including nested rows). `--compare-skips` reports judged=68, pre-existing=68, new=0, reason-changed=0, diff-owned=0, no-reason=0, not-judged=0. Logs and integration-skip-compare.txt are retained. This focused diagnostic is supplementary, never the suite's scope evidence.

After the cmd/gc unit and integration inventories above, the other remote skips are outside cmd/gc and cannot import this main package or the new test fixture. Reasons are unchanged lane gates, platform/capability exclusions, storage-shape controls, legacy-binary/live-registry opt-ins, absent herdr/tmux/Postgres prerequisites and explicit placeholder tests. The script registration adds no Go helper or skip condition in those packages. Environment-absence cases are also selected mechanically for the required native floor, whose inventory follows below. No diff-owned test skipped or failed.

## Suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
criterion3_suite_valid: true
test_counts: {'PASS': 159347, 'FAIL': 0, 'SKIP': 618}
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/logs (571 retained logs; top-level result events summed across jobs, not unique names; supplementary base diagnostics excluded)
failure_attribution: none (zero FAIL)
waiver_ref: none

The canonical gate-criterion3.sh command ran unit `make -k check`, acceptance, integration-packages, integration-full minus the latency list, and the dedicated latency list. Every remote test invocation used --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 and its own BEP. Build/nogo/sync may use cache. The exact command strings follow; phase BEP paths and predicates are retained under the suite evidence directory.

| Phase | Fresh remote verdicts | Farm test seconds | Result |
|---|---:|---:|---|
| unit | 427 | 5028.506 | PASS |
| acceptance | 25 | 1752.38 | PASS |
| integration-packages | 87 | 4750.423 | PASS |
| integration-full | 15 | 3521.48 | PASS |
| integration-latency | 1 | 94.119 | PASS |

All 555 verdicts satisfy remote strategy, both cache flags false, attempt 1 and the phase time window. Farm test seconds total 15146.908. No transport fallback, owned top-up or delta narrowing: shadow plan selects full because no prior gate PR exists. The latency leg completed on attempt 1: both latency tests PASS; farm-load probe held 20s, then clear; max measured load1 49.80 below threshold50 and zero over-threshold readings. The main integration leg ran neither latency test.

diff_tests_executed:
  TestCmdGCIntegrationNudgeStatusPreservesAvailableStore PASS (integration-packages-9326181a736eb30b71ca.log)
  TestCmdNudgeStatusReportsAnUnopenedStoreAsUnavailable PASS (integration-packages-b72a67a2f9444af705e3.log, unit-64014bc0bb3833bc198c.log)
  TestDeliverMailNotifyRegisteredCitySupervisorDownRequestsWake PASS (integration-packages-830685c60d0c2dd88567.log, unit-4d20b3d28497f844aa39.log)
  TestResolveNudgeTargetInStoreAnswersAZeroStoreByCaller PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-493754ea2d5b4d8858c8.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name

Native floor complete: 16/16 steps exit 0. It mechanically selected every environment-absence SKIP package plus beadstest, Darwin compile, push-gate-lock-selftest and local-concurrency-selftest. Its 14 top-level SKIPs (plus nested storage-shape/capability-control SKIPs) are explicitly inventoried in floor-skips.json and floor-topup.json: absent replay/coverage fixtures, live-herdr lane opt-in, Postgres DSN, br executable and synthetic provider-environment/capability refusals. These are non-owned native top-up exclusions, accepted and inventoried by the gascity remote-floor contract; they are not asserted to be fresh PASSes or identical to the remote environment. No diff-owned test was missing.

Native load observation: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1261 wait_timed_out=0 run_start_load=14.89 run_max_load=35.82 run_mean_load=29.96 run_readings=10 wait_first_load=28.28 wait_max_load=30.68 wait_mean_load=21.56 wait_readings=43 read_errors=0

Exact test_cmd (all five phases, unabridged):

```json
[
  "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
]
```

Execution (compact):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote","gate_base":"77143ecd18ff6f0199dfee3a412825f1fd9fa8a2","gate_head":"9d6266cae782eaebd106a619947680beb41ec4ea","admission":{"class":"remote","floor_gib":25.0,"free_gib":48.81131362915039,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"77143ecd18ff6f0199dfee3a412825f1fd9fa8a2","head":"9d6266cae782eaebd106a619947680beb41ec4ea","merge":"60c42f2cfc29296637b4a69b5eda00db1041c45d","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"bacf9b1a7f378462acc4f8bb72128cf7cd62a7938dfbd632a51119dfff15a021"},"test_exec_bep":{"unit":{"results":427,"remote_executed":427,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15146.908,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":94.119,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=69.43 load5=89.66 load15=53.25 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=49.80 load5=83.87 load15=52.12 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=9 max_load1=49.80 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 33.42 76.64 50.73 15/898 56234"],"window":{"readings":9,"max_load1":"49.80","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38511}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":216208}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":159347,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestCmdGCIntegrationNudgeStatusPreservesAvailableStore PASS (integration-packages-9326181a736eb30b71ca.log)\n  TestCmdNudgeStatusReportsAnUnopenedStoreAsUnavailable PASS (integration-packages-b72a67a2f9444af705e3.log, unit-64014bc0bb3833bc198c.log)\n  TestDeliverMailNotifyRegisteredCitySupervisorDownRequestsWake PASS (integration-packages-830685c60d0c2dd88567.log, unit-4d20b3d28497f844aa39.log)\n  TestResolveNudgeTargetInStoreAnswersAZeroStoreByCaller PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-493754ea2d5b4d8858c8.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```

Full execution.json retained: 6066 lines; /var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/execution.json; SHA256 17bfc5e6d8ea4ccdcdb9bd6a3e630cc9307b6bf3304fe79ff601db2e2cace524.

Full remote phase predicates retained: 87 lines; /var/tmp/gc-gate-ga-47xdfaui.IrkF5Y/remote-phase-summary.json; SHA256 51a48c74017a5b27965c35b949bf9a5f148a550f90004a997ec0c3e62466ba97.

Full SKIP sites retained: 318 lines; /var/tmp/gc-gate-ga-47xdfaui.IrkF5Y/skip-sites.txt; SHA256 564c0676497bad4889c1475fb6725915ad4a4a6e68c89b391b4b9b9c71c6c54f.

Full SKIP inventory retained: 4834 lines; /var/tmp/gc-heavy-gate/runs/ga-47xdfaui.c3/evidence/remote/skip-inventory.json; SHA256 8a3a30029c1046cab55041d517f6f829146ba3f92234b44caf6388e4bf4626f4.

Full base SKIP comparison retained: 70 lines; /var/tmp/gc-gate-ga-47xdfaui.IrkF5Y/integration-skip-compare.txt; SHA256 cd5061c048315f0136f013ac573bc42a8d7f846f815c0dcf0a7fbff36ea866ac.

Full native floor SKIPs retained: 794 lines; /var/tmp/gc-gate-ga-47xdfaui.IrkF5Y/floor-skips.json; SHA256 53283e11ffbb574591bab79640ce5d6d5eb2f033ecbcf0d1c3951c3df3c9bd36.


All 72 native SKIP events (including nested rows) name source sites already covered by the remote site audit; floor-extra-sites.txt confirms no additional site.

## Required-CI accounting

ci_required_closure: ci-required: runner-policy, changes, check (aggregator), credential-provider-windows, pack-gate, windows-lane, windows-lane-test
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612

Derived after the full suite from the merged workflow using ci_required_triggers.py; check is an aggregator, so it has no independent status. Six leaf jobs are accounted for:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | ci-accounting/runner-policy.log; runner-policy.out resolves Linux and Windows runner outputs |
| changes | LOCAL-PASS | ci-accounting/closure.txt; suite_mode=filtered; packs/credential_provider/shared do not match |
| credential-provider-windows | NOT-TRIGGERED | credential_provider filter=false |
| pack-gate | NOT-TRIGGERED | packs filter=false |
| windows-lane | LOCAL-PASS | windows-lane/units/gate.w1.stdout.log; four exact run steps in ci-windows-lane.sh |
| windows-lane-test | DEFERRED-TO-PR-CI (non-linux-runner) | runner_windows=blacksmith-4vcpu-windows-2025; own GOOS=windows test import closure from .github/windows-lane/test.txt; reach=none; windows-native-reach.txt |

runner-policy: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained output> python3 .github/workflows/scripts/runner_policy.py, exit 0. The output resolves Linux runners and blacksmith-4vcpu-windows-2025; no dynamic runner expression is guessed. changes uses closure.txt (all three filters false).

windows-lane ran the four workflow run steps verbatim, including tools/nogo working-directory semantics, behind fresh-tree-run.sh, load-gate-run.sh and isolated-test-run.sh in a detached service. All four CI_STEP_PASS markers and RESULT complete/failed=0 are verified. No other leaf lane ran concurrently. These are Windows/Darwin builds and test compilation, not native Windows test execution.

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

LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=23.93 run_max_load=26.74 run_mean_load=25.28 run_readings=5 wait_first_load=31.38 wait_max_load=89.07 wait_mean_load=39.98 wait_readings=61 read_errors=0

windows_native_test_packages: ./internal/credentialprovider ./internal/genshell/... ./internal/processgroup
windows_native_closure: internal/bazeltest,internal/credentialprovider,internal/genshell,internal/genshell/genshelltest,internal/loopbackport,internal/pathutil,internal/processgroup,internal/shellquote,internal/testenv,internal/testutil
windows_native_reach: none

deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

This PASS authorizes PR creation. Merge-ready only when CI / required is green on the PR head and windows-lane-test succeeds there. MPR watches that CI and pins the merge to the cleared head.

## Landing coordination

PM mail gcg-17175553: mayor owns parent-record ga-2l9yjvzt. Leave it open until this PR actually lands on main, with PR/main squash SHA and squash-tree proof, plus inspection of resolveNudgeStatusTarget and TestCmdGCIntegrationNudgeStatusPreservesAvailableStore cases on main. Then notify gascity/pm --notify naming ga-2l9yjvzt and follow-up ga-c3oyx3m8. The three LOW cleanups stay that separate P4 scope; this reviewed head is not amended. Gate PASS/deploy close is not landing.
