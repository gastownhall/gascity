# Release gate: nudges over runtime error text

**Verdict:** **PASS**

Deploy bead: ga-bwlr4eif; build/investigation: ga-c2zv71a2; review: ga-8e1ngt09.
Reviewed source: 45b870c2a8b20b5cb4c3e5dc26bfa19b6825002c (resolved as a commit).
Deployment: remote; base ref: origin/main; base branch: main.
Gate merge: 2c38a43efd6fb145584c5d3fe1711f5675d48e63; tree: 6e7f8b258e5b06e6f63a8b64d2657a0f28d2cb4a.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS | PASS | ga-8e1ngt09 explicitly PASSes the exact reviewed source. No carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | Captured red runtime-error rows read as empty, a nudge delivers without clearing keys, and colored typed drafts remain readable. All three owned tests passed in unit and integration lanes. |
| 3 | Full suite and required jobs | PASS | Five fresh remote phases, native floor, clear-probe latency lane, all six CI leaves accounted below, and final shell guards exit 0. |
| 4 | Unresolved HIGH findings | PASS | Zero HIGH findings. Optional RGB edge-case test is non-blocking. |
| 5 | Clean final branch | PASS | Pinned merge scratch clean before publication. Only this evidence record is added to the isolated deploy branch; final clean status is checked after its commit and before push. |
| 6 | Merge state and build/vet | PASS | Clean merge at the pinned base; make lint completed build/nogo/vet with exit 0. |
| 7 | Single feature theme | PASS | One composer-reader correction with its regression tests and runtime test manifest; no unrelated source changes. |

merge_state: clean (origin/main@47104a9c9ec4)
gate_base_kind: current-tip
gate_base: 47104a9c9ec4337bd093dd1ad5573c5aa024697c
merge_validation: clean merge result built and vetted; merge authority verifies current PR CI and head before landing.
policy_lane: fresh-tree-run.sh -> gate-base.sh run -> isolated-test-run.sh --rbe -- make lint; exit 0.
format_lane: separate fresh-tree view -> gate-base.sh run -> pinned golangci-lint 2.14.0 fmt --diff ./...; exit 0, no mismatch.
drift_lane: separate fresh-tree view -> make bazel-sync -> git diff --exit-code; exit 0.
check_hooks: core.hooksPath OK: .githooks gates are active.

Criterion 2 checks: basic-red foreground SGR 31 is ignored; default/extended/RGB/bright foreground and backgrounds remain readable. Source and end-to-end tests confirm runtime text cannot cause composer-clear refusal. A draft consisting entirely of basic-red letters in an ANSI theme can read empty; this accepted residual is documented in the code. Attached-client draft protection remains in force. The scripts manifest hunk needs admin review.

## Full test evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.aKxkNB --base 47104a9c9ec4337bd093dd1ad5573c5aa024697c --bead-json /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/bead-c3.json --owned /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/owned-all.tsv --run-dir /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence; GC_GATE_REMOTE_ENABLED=1 GC_GATE_DELTA=shadow ISOLATED_TEST_BEAD=ga-bwlr4eif DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1; exit 0.
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: unit=422, acceptance=25, integration-packages=86, integration-full=14, integration-latency=1; all fresh remote attempt 1, no cached results, all phase windows valid.
rbe_state: enabled; certificate valid; worker-env tree matches pinned base. Exact metadata in compact record below.
fallback_reason: none
admission: remote class, floor 25 GiB, cap 2; 0.001 seconds wait.
farm_test_seconds: 14435.281
test_counts: 157477 PASS / 0 FAIL / 617 SKIP (top-level results summed over retained logs).
test_log_dir: /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/logs
heavy_mode: none (heavy classifier exit 0)
waiver_ref: none
failure_attribution: none (no failing test)
ci_lane_run: n/a (no CI-config change)
owned_topup: none needed
floor_topup: complete; 16/16 steps exit 0, including environment-absence packages, beadstest, Darwin compile and runner self-tests.
latency_leg: measured with clear farm probe; both latency-budget tests PASS by name; exit 0.

GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/logs logs=564 logs_with_results=497
test_counts: PASS=157477 FAIL=0 SKIP=617
  (top-level result lines, column 0, summed over the 564 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+55 more)
diff_tests_executed:
  TestClaudeInputIgnoresRuntimeErrorInPromptRow PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ce5f199695f6832daef7.log)
  TestClaudeInputKeepsTypedTextClaudeColors PASS (integration-packages-15c091b746137e5c2921.log, unit-ef2749222a71266756ae.log)
  TestNudgeSessionDeliversOverRuntimeErrorInPromptRow PASS (integration-packages-5a078b24388ef3c79748.log, unit-4bcc997199c6a7426526.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name

skip_inventory: suite inventory retained; every environment-absence package retried by native floor; remaining skips inventoried.
skip_justification: All named skip sites are UNCHANGED and line_in_hunk=no (318 unique sites plus three common fenced-input helper sites). Three SHARED-HUNKS are confined to composer_clear_test.go: fakeClaudePane.stray field, renderStyled, sendKeys. They are test-local and cannot be imported into another package. In tmux, every not-owned unit/integration skip was rerun by name with the same flags at this gate pinned base; compare reports 2/2 and 13/13 pre-existing with identical file/line and reasons. These early environment, GOOS, provider-fixture and opt-in skip branches execute before composer rendering. The production color parser does not govern any of those skip conditions. Remaining suite skips belong to unchanged platform/opt-in/capability/no-fixture tests; every environment-absence skip package was retried by the canonical local floor. Floor skips remain inventoried, with no owned skip, no failure. Original full-suite and baseline logs are retained; console decoding removes only Bazel framing byte 0x16, as gate-rbe-evidence.py does. Baseline helper failures were comparison invocation/decoding errors, not test failures; no full-suite rerun was inferred.
Unit and integration base comparisons: 2/2 and 13/13 pre-existing; native-floor tmux comparison: 4/4 pre-existing. Base is this gate's pin, with unchanged reasons and sites. Test-local shared hunks cannot affect other packages and are answered in retained proofs.


Phase command flags (verbatim from execution.json):
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
native_floor_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=15.87 run_max_load=65.99 run_mean_load=22.52 run_readings=90 wait_first_load=15.87 wait_max_load=15.87 wait_mean_load=15.87 wait_readings=1 read_errors=0
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 15.87
run_max_load: 65.99
run_mean_load: 22.52
run_readings: 90

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.aKxkNB 47104a9c9ec4337bd093dd1ad5573c5aa024697c 45b870c2a8b20b5cb4c3e5dc26bfa19b6825002c fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
ci_required_accounting: runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-required/runner-policy.log
ci_required_accounting: changes | LOCAL-PASS | ci_required_triggers.py output /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-required/closure-fork.txt
ci_required_accounting: credential-provider-windows | NOT-TRIGGERED | filter credential_provider no match; own condition false.
ci_required_accounting: pack-gate | NOT-TRIGGERED | filter packs no match; own condition false.
ci_required_accounting: windows-lane | LOCAL-PASS | exact job run commands in /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-windows-lane.sh; exit 0; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-required/windows-lane-output.txt
ci_required_accounting: windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runs-on runner_windows resolves to blacksmith-4vcpu-windows-2025; separate Windows-native test closure contains no changed files.
windows_reach: credential-provider=none; windows-lane-test=none (separate closures, unit and integration tags).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
final_shell_guards: make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; exit 0; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-required/shell-guards-output.txt
windows-lane_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1822 wait_timed_out=1 run_start_load=80.83 run_max_load=83.52 run_mean_load=79.17 run_readings=4 wait_first_load=70.59 wait_max_load=224.97 wait_mean_load=119.91 wait_readings=61 read_errors=0
shell-guards_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=25.96 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=68.23 wait_max_load=141.97 wait_mean_load=73.34 wait_readings=61 read_errors=0

## Retained evidence

Full full execution record retained: 6050 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/execution.json; SHA256 19b7a57d5e6d3ade0e3bff10684b3cd2b9cd85b087ba609763602aa0546af9e6.
Full suite skip inventory retained: 4826 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/skip-inventory.json; SHA256 ee4457609ab0a9779edded6298713ec1ce2a6f6a8c6d8a145bc0b75c30552c28.
Full floor steps and logs retained: 368 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/compact-execution.json; SHA256 fff34a27d53a422b311b31b7a35deccaa64977e5df46c2b3f86fbfd225effb9c.
Full skip-site proofs retained: 318 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/skip-sites.txt; SHA256 12167456de799852669f544255bf8ec11fdab348543af99498115bec48a8adc4.
Full shared helper skip-site proofs retained: 3 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/skip-sites-runtimetest.txt; SHA256 f1fa22038c155c10de38ef14344856d165b4beeddbc37f8f071f820bc47f38dd.
Full native-floor skip inventory retained: 434 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/floor-skip-inventory.json; SHA256 7ba3ca56da1c0f095a4428ea214cbcc31c64462124765916301d08dfbb13b3dd.
Full CI accounting retained: 68 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-required/summary.json; SHA256 fa7cb0d6f99e70a8969d43281a8bb6517e343017ed7e20e791755034eb7d396a.
Full CI job commands retained: 6 lines; /var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/ci-windows-lane.sh; SHA256 1f3ed47aaccacc37e89a79af9247d7bd00d20363ee79ec963482f80564ff0be6.

Compact execution.json:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote","gate_base":"47104a9c9ec4337bd093dd1ad5573c5aa024697c","gate_head":"45b870c2a8b20b5cb4c3e5dc26bfa19b6825002c","admission":{"class":"remote","floor_gib":25.0,"free_gib":64.26272583007812,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"47104a9c9ec4337bd093dd1ad5573c5aa024697c","head":"45b870c2a8b20b5cb4c3e5dc26bfa19b6825002c","merge":"2c38a43efd6fb145584c5d3fe1711f5675d48e63","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"2782872eeba855b053e43068c9fc3bcf39fc4af26179b5c2570a3a1d06798384"},"test_exec_bep":{"unit":{"results":422,"remote_executed":422,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14435.281,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":73.4,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=8.66 load5=5.47 load15=5.25 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 8.07 5.62 5.31 7/879 55871"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38934}},"log_dir":"/var/tmp/gc-deploy-ga-bwlr4eif.n_e5cpmb/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":513,"environment-absence":90},"top_level_counts_by_reason_class":{"other":255,"environment-absence":43},"tests":{"elided_array":{"items":603,"bytes":218246}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":157477,"FAIL":0,"SKIP":617},"diff_tests_executed":"diff_tests_executed:\n  TestClaudeInputIgnoresRuntimeErrorInPromptRow PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ce5f199695f6832daef7.log)\n  TestClaudeInputKeepsTypedTextClaudeColors PASS (integration-packages-15c091b746137e5c2921.log, unit-ef2749222a71266756ae.log)\n  TestNudgeSessionDeliversOverRuntimeErrorInPromptRow PASS (integration-packages-5a078b24388ef3c79748.log, unit-4bcc997199c6a7426526.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```
