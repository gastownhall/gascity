**Verdict:** **PASS**

# Assignee guard global-flags release gate

Bead: ga-73hq8v6t; build: ga-w9sufw; review: ga-nk0akgp. Evaluated 2026-11-13T09:29:36.618488+00:00.

Reviewed commit: ae2cf2f835d8629556d7367e4edc4e312fb68c38
Deploy mode: remote; isolated branch: deploy/ga-73hq8v6t-gate; base: main.
merge_state: clean (origin/main@544beb8d6d7deeaeed5fae7a553e5aaabf2d0a99; files: none)
gate_base_kind: current-tip
gate_base_sha: 544beb8d6d7deeaeed5fae7a553e5aaabf2d0a99
merge_head: b2478c98c7c318f28761b95e0d321a0afd01fd5d
merge_tree: 188928563a49e1b499f046dd16acfb19799119d2
merge_validation: MPR owns exact-head merge-time validation and PR CI. This gate authorizes PR-open; every deferred job and CI / required must be green on the PR head before merge.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-nk0akgp closed PASS on the resolved reviewed commit; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS | Shared global-flag parsing and early refusal verified in source. Both owned roots and their global value/bool/inline flag and class-store subcases PASS in the full suite; clearing and valid assignments remain accepted. A separate replacement PR is authorized; mayor retires PR5358 after this replacement lands. |
| 3 | Tests, policy, drift and CI accounting | PASS | 154089 PASS,0 FAIL,604 SKIP (top-level Go result lines);537 uncached remote results,16 native floor steps exit0; both owned roots PASS. All skip source/measurement proofs and all6 CI leaf accounts verified. |
| 4 | No unresolved HIGH review findings | PASS |0; create-assignee and attached-short-form extensions are separately tracked as nonblocking follow-ups. |
| 5 | Clean implementation tree | PASS | Gated merge tree clean after runtime/audit; publication verifies the reviewed source plus the record-only commit and a clean isolated deploy branch. |
| 6 | Merge state and build/vet | PASS | Clean frozen merge; whole-tree Bazel build and make vet/nogo terminal SUCCESS, parents and predicted tree independently matched. |
| 7 | One feature theme | PASS | Five cmd/gc files implement one structural-assignee refusal and its global-flag bypass fix, with regression tests and one BUILD source entry. |

test_cmd_scope: full-suite
test_counts: PASS=154089 FAIL=0 SKIP=604
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/c3-run/evidence/remote/logs
diff_tests_executed: cmd/gc TestBdAssigneeShapeRefusal PASS; cmd/gc TestBdByIDRefusesAMalformedAssigneeOnAClassOwnedID PASS; all observed owned subcases PASS, no SKIP or FAIL (bound to own package artifacts).
test_exec_mode: remote-exec
skip_measurements: 590 remote and 72 native full-name/reason observations compared at the frozen base (nested/duplicate phase observations; top-level counts remain separate)
test_isolation: isolation wrapper PASS-THROUGH -- NOT ISOLATED; owned merge scratch and separate fresh lane views; pinned test bd v1.3.4; ICU74 host check PASS; containerhost uses emulated tools, no testcontainers Go harness.
waiver_ref: none
failure_attribution: n/a (zero failed tests)
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
fast_lane:21/21 targets PASS before runtime setup (policy13, generated drift7, OpenAPI1), each in a separate fresh view.
policy_lane: pinned fmt-check-changed; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks;13 Bazel policy targets PASS; whole-tree vet/nogo supplied by criterion6.
drift_lane: make bazel-sync + git diff --exit-code + empty git status;7 generated checks PASS; pinned-base OpenAPI breaking check PASS.
skip_justification: All skips lie outside both owned function bodies. Each skip decision function is identical to its frozen-base source and its printed site is unchanged. No SHARED-HUNKS or HELPER-HUNKS occur. The changed production guard is called only from bd update routing; all non-owned observed Go skips are independently compared by name/reason against the frozen base to cover both import and subprocess reach. See manual source review for each remaining environment, fixture, platform or optional-feature reason. Non-Go bridge skip is its unconfigured TypeScript-version check; all its inputs match base. No owned test or child skipped.

test_cmd (actual captured flags):
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
Runtime entry point:
```text
/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/merge --base 544beb8d6d7deeaeed5fae7a553e5aaabf2d0a99 --bead-json /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/bead.json --owned /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/owned.tsv --run-dir /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence
```
Phases: make -k check; acceptance (acceptance_test + acceptance_solo_tests); integration_packages; full integration excluding the canonical latency list; isolated integration-latency with the canonical list, sharding disabled, and farm probe.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/merge 544beb8d6d7deeaeed5fae7a553e5aaabf2d0a99 ae2cf2f835d8629556d7367e4edc4e312fb68c38 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; output /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/ci-required/closure.stdout
ci_required_accounting:
- changes | LOCAL-PASS | rc=0 | /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/closure.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- runner-policy | LOCAL-PASS | rc=0 | /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/runner-policy.stdout
- windows-lane | LOCAL-PASS | rc=0 | /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-lane.stdout
  command (cwd=.): GOOS=windows go build $(cat .github/windows-lane/build.txt); rc=0; log=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-lane-3.stdout
  command (cwd=.): GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt); rc=0; log=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-lane-4.stdout
  command (cwd=tools/nogo): GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...; rc=0; log=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-lane-5.stdout
  command (cwd=.): GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...; rc=0; log=/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-lane-6.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/ci-required/windows-native-reach.json
windows_reach: {"credential": [], "native": []}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

c3-run/units/gate.w1.stderr.log: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=11.47 run_max_load=99.83 run_mean_load=56.53 run_readings=110
c3-run/evidence/remote/floor-topup.stdout: load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=1 run_start_load=90.83 run_max_load=100.00 run_mean_load=96.00 run_readings=12
ci-required/windows-lane.stderr: load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=0 run_start_load=14.15 run_max_load=13.72 run_mean_load=13.23 run_readings=4
skip-base-evidence/native.stderr: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=10.57 run_max_load=11.94 run_mean_load=11.58 run_readings=3

Full complete execution evidence retained: 5928 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/c3-run/evidence/execution.json; SHA256 ec41723496da7f3b97e2befc8b2e3a36d221a2fc144609ce9cd45bdf548cb82a.
Full source/acceptance review retained: 24 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/acceptance-source-proof.json; SHA256 eac009a077b67fb95dcd085994283fc8dc963de9b65c64840e0f57699dcd0d88.
Full whole build/vet proof retained: 53 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/c6-verified.json; SHA256 dd371939d5fda9d98872a53fb608ffdaf1bd3ec5ea5dfeda977ec9bdb5a2aacb.
Full fast-lane proof retained: 35 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/fast-verified.json; SHA256 17a20ee2694be3cf1cb244e0bc8dc2d9a49760a8cfe66fa4cf1019cc6bd096c4.
Full full-suite independent audit retained: 122 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/resume-c3-audit.json; SHA256 40856cb115816dee6051871912fff6ca97e8d43239507f47bb6d55c3f1ec588d.
Full owned package proof retained: 967 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/owned-package-verified.json; SHA256 d72495b20379aaa0076b8fb690ef6d5ce72c447d75446d5efd1b9405ac14f2e5.
Full remote skip inventory retained: 4722 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/skip-audit-inventory.json; SHA256 a6a12c31999399bc3abf88fb166a108e0d823c8152353cce3824d68d6706eab8.
Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/floor-skip-inventory.json; SHA256 48ffb8f45e98cea263a232caa71802b216e76763af89c6d3aea5e9f526b5696d.
Full skip source/measurement proof retained: 2426 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/skip-proof-automated.json; SHA256 b83b40deea8b9e0a600882ca8afac50191cf3b8ff2c93722d59157bdcbf55064.
Full skip decision functions retained: 12230 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/skip-source-functions.json; SHA256 fa4ef527ca54cd1498a7c4e98a90eff856793d67fd77329470eb6013adc5375f.
Full manual source and skip review retained: 27 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/manual-source-review.json; SHA256 20921282efe4c31bbca7051ccf60477142c6a4acb7825bdad87bfe686194497b.
Full non-Go skip proof retained: 34 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/non-go-skip-reviewed.json; SHA256 7e7569e5da792ce039ee7ee467dc22ab60eeb09facb001068f05a26233a665e5.
Full CI accounting proof retained: 91 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/ci-required/accounting-verified.json; SHA256 e2d1935ef2059876088445a88879ac69b4f66bf3843edbfbfdf145656ea9b321.
Full raw log SHA256 manifest retained: 9120 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-73hq8v6t/gate-20261113T092936Z/MANIFEST.json; SHA256 68aadc92a32c3a5bd8b9139680e8205e6531d4eaa5b7a4365199036752ff1ace.

Execution record (compact; arrays retained at the pointers above):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":64.63783264160156,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":411,"remote_executed":411,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13440.564,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":110.063,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=69.64 load5=81.54 load15=46.83 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=44.18 load5=74.30 load15=45.53 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.34 68.27 44.44 10/1084 54703"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40312}},"log_dir":"/var/tmp/gc-gate-ga-73hq8v6t-fresh-20261113.dcmo76iw/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":500,"environment-absence":90},"top_level_counts_by_reason_class":{"other":242,"environment-absence":43},"tests":{"elided_array":{"items":590,"bytes":220786}}},"test_cmd":{"elided_array":{"items":5,"bytes":2139}},"test_counts":{"PASS":154089,"FAIL":0,"SKIP":604},"diff_tests_executed":"diff_tests_executed:\n  TestBdAssigneeShapeRefusal PASS (integration-packages-75af79934639abaae69c.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestBdByIDRefusesAMalformedAssigneeOnAClassOwnedID PASS (integration-packages-9326181a736eb30b71ca.log, unit-637c74c578a2b17fc654.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```
