**Verdict:** **PASS**

# Acceptance rig-add port recovery — ga-2o54uru

Gate evaluated 2026-11-09T04:56:06.225530+00:00. This authorizes PR opening; merge authority must check the exact PR head and required CI.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-ti2m6if records verdict pass on the resolved reviewed source. No carryover or reviewer-added commit is involved. |
| 2 | Acceptance criteria met | PASS | Both M1 rig-add call sites use the existing TopologyRun.RigAdd helper. Its proof-gated, once-only retry and blocked-repair one-event assertion remain intact. Both changed tests PASS by name. |
| 3 | Tests pass | PASS | Canonical five-phase full-suite chain plus required native floor exited 0. 533 first-attempt uncached remote results, 16 native floor steps all exit 0; policy/generated drift and all six ci-required leaf accounts complete. Native Windows execution is deferred to PR CI. |
| 4 | No unresolved HIGH review findings | PASS | Review security/style/spec findings report none; test-only change with no new production input surface. |
| 5 | Final branch clean | PASS | Source checkout clean before the gate record; only this record is added in its own commit. Final committed-head cleanliness is verified before push. |
| 6 | Merge state recorded; builds/vets on its base | PASS | Clean canonical merge; exact predicted-tree equality, full Bazel build/nogo and make vet exit 0. Gate base is pinned below. |
| 7 | Single feature theme | PASS | One commit changes two acceptance-test bodies (+5/-2), both using the same existing port-collision recovery helper. No independent feature or internal-doc contamination. |

deploy_mode: remote; base_ref: origin/main
reviewed_source: 0a49ebdaa0df809f4d2f00bd3f328fe3a1a6765a
gate_base_kind: current-tip
gate_base_sha: 0629c3fce24f471873a5945869afed1f0074fc93
merge_state: clean (origin/main@0629c3fce24f471873a5945869afed1f0074fc93)
merge_head: 7a01f748be01f2204c25577183f542183207332c
merge_tree: 89225d73754c3a2864111c5a4e17ab8dee40fab5
merge_validation: merged result built/nogo-vetted and independently tested at the pinned base; no rebase. Merge-time freshness and required checks belong to MPR.
preflight: no PR associated with the reviewed commit; fetched origin/main still equals the pinned base before publication.
ancestry_scope: stack-aware denylist/citation check exit 0 with ga-2o54uru, ga-f4txy0m and ga-ti2m6if; no stack.
review_findings: unresolved HIGH=0; uncovered_criteria=none; descoped_criteria=none; deploy_stage_criteria=none.

## Tests

test_cmd: env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2o54uru DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/merge --base 0629c3fce24f471873a5945869afed1f0074fc93 --bead-json /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/bead-c3.json --owned /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/owned.tsv --run-dir /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-run/evidence -- python3 /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/local-bazel.py
test_cmd_scope: full-suite
phase_commands: unit = make -k check (Bazel //... plus shell guards); acceptance = bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages = bazel test --config=integration //test:integration_packages; integration-full = bazel test --config=integration //test/integration:integration_test minus only gate-latency-tests.txt; integration-latency = the same target filtered to that registry, unsharded and under the worker load probe.
phase_flags: --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1, with one build_event_json_file per phase; integration legs add -test.v. Full flags and phase windows are retained below.
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none (no local restart; native floor is ancillary evidence)
admission: {"class":"remote","floor_gib":25.0,"free_gib":74.36247634887695,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 10624.693
test_counts: {"PASS":149403,"FAIL":0,"SKIP":512}; top-level result lines summed across 549 retained job logs, including repeated names across jobs.
test_log_dir: /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-run/evidence/remote/logs
diff_tests_executed: test/acceptance TestBlockedRepairOnProxiedCityAndRig PASS; test/acceptance TestDoctorLeavesAStoppedProxiedCityStopped PASS (actual acceptance-phase logs).
owned_topup: [] (both owned tests already PASS in the full remote suite)
floor_topup: complete=true; 16/16 command steps exit 0; 13 selected package runs, beadstest included, plus Darwin compile and two runner self-tests. Selection independently equals the canonical environment-absence selection. Native commands issued unmodified by canonical scripts under mayor ruling gm-wisp-tnijak3; no main-suite substitution.
latency_leg: complete=true; measured under clear farm probe; both registry tests PASS; attempt 1; no deferred leg and neither test executed in integration-full.
skip_inventory: remote 498 events; native floor 39 events including nested subtests. All audited sites UNCHANGED; no shared/helper hunk or production input changed.
skip_justification: Unchanged lane exclusions, optional tools/services/platform fixtures, provider capability mocks, helper entry points and existing characterization/debt do not bear on the two edited acceptance-test bodies. Every environment-absence package was selected for the native floor. Remaining native SKIPs are existing opt-in live/replay/coverage fixtures, optional br/Postgres, capability mocks or binding tests requiring a parent tmux session; neither owned test skipped.
failure_attribution: none; waiver_ref: none; heavy_mode: none; heavy_units: n/a; coverage_check: n/a (not a heavy composite).
ci_lane_run: n/a (no CI configuration change)

policy_lane: fresh-tree-run.sh -> gate-base.sh run -> fast-lane.sh policy; exit 0; fmt-check-changed, make lint/nogo, four shell guards, check-hooks and 14 explicit policy/format/docsync targets PASS.
drift_lane: separate fresh-tree/gate-base view; make bazel-sync with remote-exec/no-local-fallback, git diff --exit-code and empty git status, then seven generated/lockfile drift targets; exit 0. OpenAPI breaking check in a third fresh view, exit 0. 22 total explicit fast-lane summaries PASS before suite launch.
runtime: ICU74 verdict=OK provenance=recorded; rootless podman socket active/ping OK; pinned Dolt 2.6.0 image present; pinned bd1.3.4 resolved; testcontainer-sweep enabled. Wrapper reports PASS-THROUGH / NOT ISOLATED; no clean-environment isolation claim.
runtime_probe_load: threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=32.94 (remote class, no host-load wait).
suite_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=29.50 run_max_load=87.39 run_mean_load=57.51 run_readings=115 wait_first_load=29.50 wait_max_load=29.50 wait_mean_load=29.50 wait_readings=1 read_errors=0
native_floor_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1321 wait_timed_out=0 run_start_load=14.07 run_max_load=14.71 run_mean_load=13.67 run_readings=7 wait_first_load=77.07 wait_max_load=87.51 wait_mean_load=53.96 wait_readings=45 read_errors=0
windows_cross_compile_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.03 run_max_load=19.14 run_mean_load=16.68 run_readings=5 wait_first_load=12.03 wait_max_load=12.03 wait_mean_load=12.03 wait_readings=1 read_errors=0

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/merge 0629c3fce24f471873a5945869afed1f0074fc93 0a49ebdaa0df809f4d2f00bd3f328fe3a1a6765a fork; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status. check is an AGGREGATOR and takes no status.
ci_required_accounting:
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/runner-policy.outputs python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/runner-policy.log.
- changes | LOCAL-PASS | closure tool above, exit 0; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/closure.txt.
- credential-provider-windows | NOT-TRIGGERED | credential_provider filter no match; closure row retained.
- pack-gate | NOT-TRIGGERED | packs filter no match; closure row retained.
- windows-lane | LOCAL-PASS | GOOS=windows go build $(cat .github/windows-lane/build.txt); GOOS=windows go test -count=1 -vet=off -run ^$ -exec /bin/true $(cat .github/windows-lane/test-build.txt); tools/nogo: GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run ^$ -exec /bin/true ./analyzers/...; GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run ^$ -exec /bin/true ./...; all four exact workflow steps exit 0; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/windows-lane-terminal.log. Compile-only commands are not runtime test PASSes.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner-policy resolves runner_windows to blacksmith-4vcpu-windows-2025; native allowlist is ./internal/credentialprovider. Independent Windows dependency-closure reach is none; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/windows-native-reach.json.
windows_reach: none (native allowlist dependency closure independently computed; no changed path in that closure).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
merge_ready: only when CI / required is green on the exact PR head and windows-lane-test succeeds. Gate PASS alone does not authorize merge.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":74.36247634887695,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":10624.693,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":77.726,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=46.58 load5=41.62 load15=22.36 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 37.93 39.85 22.57 55/1794 55825"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":31428}},"log_dir":"/var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":179882}}},"test_cmd":{"elided_array":{"items":5,"bytes":2084}},"test_counts":{"PASS":149403,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed:\n  TestBlockedRepairOnProxiedCityAndRig PASS (acceptance-849e536d828bc5eaea1e.log)\n  TestDoctorLeavesAStoppedProxiedCityStopped PASS (acceptance-a5e8b4dd0a96f0473af0.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```

## Retained evidence

Full full execution.json retained: 5044 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-run/evidence/execution.json; SHA256 11298c5a720c27029bc503950b155cb95335e8d8064da8a5b509bf8f7858504a.
Full remote phase and artifact audit retained: 94 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/remote-resume-audit.json; SHA256 68626303ced9081c095d0603c82f441d63d3dd467a4264d6190afe8dc01613e4.
Full suite terminal audit retained: 19 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/suite-terminal-audit.json; SHA256 e384bf81bb6258724b95c37a1fdca2f3e3e91e3039c532a4bb7aa395233f2a85.
Full remote skip inventory and site audit retained: 5994 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/skip-site-audit.json; SHA256 9be13ff2fddd9b2da7ab81c20dea78a41cdb1e95d72d6679840aedb2b3aa0837.
Full remote skip site proof retained: 300 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/skip-sites-evidence.txt; SHA256 a12c8f56daf1c27f231d362c28614250e64782cdd10ab0952c22efc135020ae9.
Full native skip inventory retained: 392 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/native-floor-skips.json; SHA256 4125bafc3ae2441af6ed8df083e0b5f276ffc9ad595220359f7078449e5da853.
Full native skip site proof retained: 46 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/native-floor-sites.txt; SHA256 a4fbd9f8331743d11c0fcc89c74addc80d0e6b9b88f49b4497c56d7900c608dc.
Full log and artifact SHA256 manifest retained: 8157 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/retained-evidence-manifest.json; SHA256 beb22a721ce2109af252c464dca9a955521b183f588bac26e8de6beb22f01d11.
Full criterion6 proof retained: 23 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c6-verified.json; SHA256 eded1cac0a9d41ead3667729cb54fd299a4642a4e70e8315b1323cab6be714b6.
Full fast lane proof retained: 443 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/fast-verified.json; SHA256 3732be18acb4dc0baa380ca227f0d542690bb324050bd153c78e4de2747a890c.
Full CI accounting retained: 57 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-required/accounting.json; SHA256 077b5773de778be9358d7e24c1cc42cd5b759ee08d82baef71607989159ec5fd.
Full CI audit retained: 14 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/ci-audit.json; SHA256 125874d23a5149b9514385c1f7390b865bf7fc97ff7674887eb7b054b7ee7a7d.
Full by-name counts and results retained: 8 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/final-test-evidence.txt; SHA256 6ed1d62186831758203b695c03731b7970c550518194428dbf040aac55ffc67b.
Full full suite commands and terminal output retained: 13 lines; /var/tmp/deploy-ga-2o54uru-fresh.lg79zq7c/c3-terminal-recovered.log; SHA256 8ec3ee1e18a685f4c3343a47edd0c92158d4a86c8ce965b12a35a49ab6613dfb.
