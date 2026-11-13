**Verdict:** **PASS**

# tmux pipe-at-death integration test release gate

Bead: ga-c8rmejcf; build: ga-5czv4cld; review: ga-d1m0qf6y. Evaluated 2026-11-13T07:24:41.250554+00:00.

Reviewed commit: 39ab221073fe98a461d4b8620a23b8950eeb8988
Deploy mode: remote; isolated branch: deploy/ga-c8rmejcf-gate; base: main.
merge_state: clean (origin/main@dba20336edcde787ab8c50a2ba3c6b99590f73d6; files: none)
gate_base_kind: current-tip
gate_base_sha: dba20336edcde787ab8c50a2ba3c6b99590f73d6
merge_head: 23ef79dd2dbcfff8d7c606f25543cccf59955a8c
merge_tree: 7ca72e0d9e2dfd553a1b96fb8eb92b93b49a91a4
merge_validation: MPR owns exact-head merge-time validation and PR CI. This gate authorizes PR-open; every deferred job and CI / required must be green on the PR head before merge.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-d1m0qf6y closed PASS on the resolved reviewed commit; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS | Closed-pipe immediate disarm and open-pipe deferral/revival assertions checked in source; owned root and both subcases PASS in the full suite. Local tmux 3.8:30 iterations of each PASS. Existing fake-server deferral unit coverage remains. |
| 3 | Tests, policy, drift and CI accounting | PASS | 153754 PASS,0 FAIL,604 SKIP (top-level Go result lines);537 uncached remote results,16/16 native floor steps exit0; owned root PASS. All skip source/measurement proofs and all6 CI leaf accounts verified. |
| 4 | No unresolved HIGH review findings | PASS |0; the reviewer recorded one nonblocking message-text nit. |
| 5 | Clean implementation tree | PASS | Gated merge tree clean after runtime/audit; publication verifies the reviewed source plus the record-only commit and a clean isolated deploy branch. |
| 6 | Merge state and build/vet | PASS | Clean frozen merge; whole-tree Bazel build and make vet/nogo terminal SUCCESS, parents and predicted tree independently matched. |
| 7 | One feature theme | PASS | One existing tmux integration test and its doc comment; no production-code or independent feature changes. |

test_cmd_scope: full-suite
test_counts: PASS=153754 FAIL=0 SKIP=604
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/c3-run/evidence/remote/logs
diff_tests_executed: TestDisarmSweepDefersDeadPaneUntilRevived PASS; /relaunch PASS; /respawn-pane PASS (bound to its own package artifact).
test_exec_mode: remote-exec
test_isolation: isolation wrapper PASS-THROUGH -- NOT ISOLATED; owned merge scratch and separate fresh lane views; pinned test bd v1.3.4; ICU74 host check PASS; containerhost uses emulated tools, no testcontainers Go harness.
waiver_ref: none
failure_attribution: n/a (zero failed tests)
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
fast_lane:21/21 targets PASS before runtime setup (policy13, generated drift7, OpenAPI1), each in a separate fresh view.
policy_lane: pinned fmt-check-changed; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks;13 Bazel policy targets PASS; whole-tree vet/nogo supplied by criterion6.
drift_lane: make bazel-sync + git diff --exit-code + empty git status;7 generated checks PASS; pinned-base OpenAPI breaking check PASS.
skip_justification: All skips lie outside the owned test body and have unchanged source decision sites. The sole shared hunk is its two-line doc comment. Every reached skip was measured with the same full name/reason at the frozen base; un-reached skips have source/import-closure proof. Remaining native skips are optional backend/live-provider, synthetic fixture variants, or context requirements. Non-Go bridge skip is its unconfigured TypeScript-version check; all its inputs match base. No owned test or child skipped.

test_cmd (actual captured flags):
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
Runtime entry point:
```text
/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/merge --base dba20336edcde787ab8c50a2ba3c6b99590f73d6 --bead-json /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/bead.json --owned /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/owned.tsv --run-dir /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence
```
Phases: make -k check; acceptance (acceptance_test + acceptance_solo_tests); integration_packages; full integration excluding the canonical latency list; isolated integration-latency with the canonical list, sharding disabled, and farm probe.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/merge dba20336edcde787ab8c50a2ba3c6b99590f73d6 39ab221073fe98a461d4b8620a23b8950eeb8988 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; output /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/ci-required/closure.stdout
ci_required_accounting:
- changes | LOCAL-PASS | /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/closure.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- runner-policy | LOCAL-PASS | /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/runner-policy.stdout
- windows-lane | LOCAL-PASS | rc=0 | /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-lane.stdout
  command (cwd=.): GOOS=windows go build $(cat .github/windows-lane/build.txt); rc=0; log=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-lane-3.stdout
  command (cwd=.): GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt); rc=0; log=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-lane-4.stdout
  command (cwd=tools/nogo): GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...; rc=0; log=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-lane-5.stdout
  command (cwd=.): GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...; rc=0; log=/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-lane-6.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/ci-required/windows-native-reach.json
windows_reach: {"credential": [], "native": []}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

c3-run/units/gate.w1.stderr.log: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=91.91 run_max_load=125.44 run_mean_load=94.43 run_readings=106
c3-run/evidence/remote/floor-topup.stdout: load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=1 run_start_load=66.67 run_max_load=87.23 run_mean_load=76.10 run_readings=11
ci-required/windows-lane.stderr: load_threshold=15 load_waited_seconds=1806 load_wait_timed_out=1 run_start_load=70.60 run_max_load=76.88 run_mean_load=75.08 run_readings=7
local-smoke-run/units/gate.w1.stderr.log: load_threshold=15 load_waited_seconds=1800 load_wait_timed_out=1 run_start_load=119.10 run_max_load=115.58 run_mean_load=115.58 run_readings=1
skip-base-evidence/native.stderr: load_threshold=15 load_waited_seconds=930 load_wait_timed_out=0 run_start_load=14.86 run_max_load=13.99 run_mean_load=13.98 run_readings=2

Full complete execution evidence retained: 5927 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/c3-run/evidence/execution.json; SHA256 713a290b22f700700046451a35b8ff965ffd2685f722828c3aee4dce8da4e103.
Full source/acceptance review retained: 19 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/acceptance-source-proof.json; SHA256 c333310e6bf23e8cdcedbe2bd80b975a6baeb85b97a48d022bb3c9d50ddc63c5.
Full whole build/vet proof retained: 49 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/c6-verified.json; SHA256 2461e40794a4ac5bc7c63d8e3b5a6fa67e2201008906552b7d94a98432d92da8.
Full fast-lane proof retained: 35 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/fast-verified.json; SHA256 c1b369a76114bdc192da8637eb9d15b21bfae08a78f31d1476e81c7304c58870.
Full full-suite independent audit retained: 122 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/resume-c3-audit.json; SHA256 0cb0df5c88846a9055010268ab8c71e4de039950b2f6a71856b505f490ebdcb8.
Full owned package proof retained: 49 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/owned-package-verified.json; SHA256 2e8c1e83235603fc32ac6243bf404e878a4eeaef8cac41b273489b904b39a9ba.
Full existing fake-server deferral unit proof retained: 13 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/fake-server-deferral-proof.json; SHA256 5a8b58085072c3e57940c9a6e662e1eeac6969f8bdafd73176addfba62f3b60b.
Full native skip static proof retained: 24 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/native-skip-static-clauses.json; SHA256 42d4f5b74de7365b8073903a3780bb6bc594ae6375298f9ed8274d5bc38179ee.
Full remote skip inventory retained: 4722 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/skip-audit-inventory.json; SHA256 cdc43fd54d914116bedc0e9424f7473949be4f10a57494f4cef8ef7cfb263dab.
Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/floor-skip-inventory.json; SHA256 d89bef046dfa16e17a0a89f680eb1133dd728af8cc76fb3954069c511d7450ac.
Full skip source/measurement proof retained: 2230 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/skip-proof-automated.json; SHA256 3718b12f81f8b67609b990dc62022fbfe1b89960d323cdb39e8f2bc4676931ac.
Full shared-hunk static proof retained: 1879 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/skip-static-clauses.json; SHA256 c41e242eb7f9b45e420cb5aa99a35953ea46064c94676c93526b20a4b7e15b70.
Full non-Go skip proof retained: 34 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/non-go-skip-reviewed.json; SHA256 f3752769170624911aa85995dc79ab35183bccdfe4786b7a43686e899850a12f.
Full CI accounting proof retained: 69 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/ci-required/accounting-verified.json; SHA256 5805b8c1569f066e6653c9a732c1f8a8d9d31f9fba2758c0076185a21861e5f4.
Full local tmux3.8 proof retained: 66 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/local-tmux-smoke-verified.json; SHA256 392d3b0f98d2bdeacea91e47c0d07cf0c4240a1c69b3a0bec7756f5de91b98e5.
Full raw log SHA256 manifest retained: 8080 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c8rmejcf/gate-20261113T072440Z/MANIFEST.json; SHA256 c1fb09b177ef4b38f75b3c4a0f77b8c3462abf2c1cb71e97b6b63f29ca359e7e.

Execution record (compact; arrays retained at the pointers above):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":63.994014739990234,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":411,"remote_executed":411,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":11381.096,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":87.965,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=8.88 load5=15.83 load15=12.69 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 15.03 16.50 13.04 11/1176 56695"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39676}},"log_dir":"/var/tmp/gc-gate-ga-c8rmejcf-20261113._2ig28r4/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":500,"environment-absence":90},"top_level_counts_by_reason_class":{"other":242,"environment-absence":43},"tests":{"elided_array":{"items":590,"bytes":217246}}},"test_cmd":{"elided_array":{"items":5,"bytes":2109}},"test_counts":{"PASS":153754,"FAIL":0,"SKIP":604},"diff_tests_executed":"diff_tests_executed:\n  TestDisarmSweepDefersDeadPaneUntilRevived PASS (integration-packages-f1416b63f1c12109ea7e.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```
