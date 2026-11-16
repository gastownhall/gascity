# Release gate: remove dead pool-counting helper

**Verdict:** **PASS**

Gate PASS authorizes PR-open only. Merge requires the current head’s CI / required and deferred Windows job to pass.

Bead: ga-buojy2e; build: ga-d55dlw; review: ga-ta93dat.
Reviewed source: cd55ec2ff711cbc3a82e13eeee7b8d37c78721c4
Gated base: 6c095aace30bdeba90a837fb7ed6993d60ca157e
Gated merge commit: 9d51fad521b80b4bb41fc221a8418d24ced2ec3d
Gated tree: 52220269e14b5d1eca748ca9bd1e9efa28bc7950
deploy_mode: remote; push_remote: fork; base_ref: origin/main
merge_state: clean (origin/main@6c095aace30bdeba90a837fb7ed6993d60ca157e)
gate_base_kind: current-tip
merge_validation: merge result tested at the pinned base; merge authority must validate the current merge result.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-ta93dat records pass for the exact resolved reviewed source. |
| 2 | Acceptance criteria met | PASS — only the archived backlog references the deleted symbol; full build and nogo vet pass. The partial provider remains; all five retained TestDiscoverPoolInstances tests pass by name, including UnlimitedFailsClosedOnPartialResults. |
| 3 | Full suite, policy, drift and required CI | PASS — full-suite evidence: 149648 PASS / 0 FAIL / 512 SKIP. All non-owned SKIPs justified, including the completed native-base comparison. Policy, drift and required-CI accounting complete; Windows native execution explicitly deferred below. |
| 4 | No unresolved HIGH review findings | PASS — live review reports no style or security findings; no unresolved high findings. |
| 5 | Final branch clean | PASS — source and merge views clean. Publication checks require a gate-only commit on the isolated deploy branch and clean git status before push. |
| 6 | Merge state, build and vet | PASS — clean pinned merge; bazel build //... and make vet completed SUCCESS, independently verified from BEP. |
| 7 | Single feature theme | PASS — two cmd/gc files, 0 additions and 109 deletions; remove one dead helper and its five test callers. |

test_cmd_scope: full-suite
test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh start --run-dir /var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run --key 52220269e14b5d1eca748ca9bd1e9efa28bc7950 -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-buojy2e DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-buojy2e.2p0r3g1a/merge --base 6c095aace30bdeba90a837fb7ed6993d60ca157e --bead-json /var/tmp/deploy-ga-buojy2e.2p0r3g1a/bead-criterion3.json --owned /var/tmp/deploy-ga-buojy2e.2p0r3g1a/owned.tsv --run-dir /var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run/evidence -- python3 /var/tmp/deploy-ga-buojy2e.2p0r3g1a/local-bazel.py
test_counts: PASS=149648 FAIL=0 SKIP=512 (top-level result lines, each name counted once per job; native floor is already included).
diff_tests_executed: none (no diff-owned tests; whole functions deleted).
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI configuration change)
heavy_mode: none; heavy_units: n/a; assigned_total: n/a; executed_total: n/a; coverage_check: n/a (ordinary canonical remote gate)
test_log_dir: /var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":408,"remote_executed":408,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: None
admission: {"class":"remote","floor_gib":25.0,"free_gib":66.64378356933594,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 12508.358
owned_topup: none; floor_topup: complete, 16/16 steps, all exit 0.
latency_leg: clear farm probe, both configured latency tests PASS by name.
policy_lane: run-pinned-lint.sh -- make fmt-check-changed; make lint (nogo); shell guards; 13 Bazel policy targets — all exit 0.
drift_lane: make bazel-sync then git diff --exit-code and clean untracked status; 7 generated-artifact targets — all exit 0.
OpenAPI compatibility: make openapi-breaking-check OPENAPI_BREAKING_BASE=<pinned-base> — exit 0.
Policy/drift/OpenAPI each ran in a separate fresh-tree-run.sh view, through gate-base.sh.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-buojy2e.2p0r3g1a/merge 6c095aace30bdeba90a837fb7ed6993d60ca157e cd55ec2ff711cbc3a82e13eeee7b8d37c78721c4 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:
- runner-policy | LOCAL-PASS | exact YAML runner_policy.py with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH=; rc0; ci-required/runner-policy-result.json.
- changes | LOCAL-PASS | closure tool above exited 0; ci-required/closure.txt.
- credential-provider-windows | NOT-TRIGGERED | needs.changes.outputs.credential_provider == true; filter no match.
- pack-gate | NOT-TRIGGERED | needs.changes.outputs.packs == true; filter no match.
- windows-lane | LOCAL-PASS | exact four YAML command groups, including Windows build/test compilation, nogo analyzer cross-compilation and full macOS build/test compilation; rc0; ci-required/windows-lane-result.json; logs ci-run/units/gate.w1.stdout.log and gate.w1.stderr.log. The earlier disk refusal ran no YAML command; this completed run is its sole result.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Windows runner expression resolves to blacksmith-4vcpu-windows-2025; actual test.txt allowlist import closure verified; no changed path reached.
windows_reach: none (independent windows-lane-test computation, not credential-only helper).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

Acceptance by name:
- --- PASS: TestDiscoverPoolInstancesUnlimited (0.00s)
- --- PASS: TestDiscoverPoolInstancesCanonicalSingletonUsesBaseName (0.00s)
- --- PASS: TestDiscoverPoolInstancesUnlimitedFailsClosedOnPartialResults (0.00s)
- --- PASS: TestDiscoverPoolInstancesBounded (0.00s)
- --- PASS: TestDiscoverPoolInstancesBoundedWithNamepool (0.00s)

Load/isolation: canonical wrappers report PASS-THROUGH (not filesystem isolation); the remote suite and native floor kept their own environment contracts.
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=25.62 run_max_load=89.12 run_mean_load=48.98 run_readings=132 wait_first_load=25.62 wait_max_load=25.62 wait_mean_load=25.62 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=24.57 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=24.57 wait_max_load=24.57 wait_mean_load=24.57 wait_readings=1 read_errors=0

skip_justification: all remote SKIPs are non-owned, with source-site and import-closure proofs; 186 cmd/gc rows independently reproduced at the pinned base (151 unit + 31 integration + 4 simulation), same names and reasons. Native floor adds 39 nested-inclusive rows, of which all three cmd/gc rows independently reproduce at the pinned base using the exact native floor command (compare rc0: 3/3 pre-existing, no new or changed reason). Non-owned environment/platform/helper SKIPs stay inventoried.

Compact canonical execution (skip and CI accounting completed separately above):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":66.64378356933594,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":408,"remote_executed":408,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12508.358,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":70.859,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=64.25 load5=83.85 load15=44.74 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=46.01 load5=78.42 load15=43.78 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.03 71.42 42.56 10/1057 54987"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30900}},"log_dir":"/var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":176894}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":149648,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"}
```

Full c3-run/evidence/execution.json retained: 5045 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/c3-run/evidence/execution.json; SHA256 6a629889b0595fdd56f0fcf32d126cd518ab50914a2ad7cd186dafc6558b6e80.
Full skip-justifications.json retained: 5566 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/skip-justifications.json; SHA256 fb5613fc96405e5e799974b591421c4c3676ef333d180ce05fe0e4d4fe0bcb0b.
Full skip-sites-all.txt retained: 379 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/skip-sites-all.txt; SHA256 b2b20993ab6f5b23aca8e49822c10a2dafdde748a3d0307b663b70371b0c5eb6.
Full skip-dispatch-site.txt retained: 1 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/skip-dispatch-site.txt; SHA256 e47d9fa175eea52b228a13d9c643dbe70b29357dac2b3dd8128506cb8fa667ad.
Full skip-reach.json retained: 3171 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/skip-reach.json; SHA256 4761201006d1082275fc5aa8a97a8f33b4a1a23b01d30c786ea236fa216cb77d.
Full skip-proof-final.json retained: 602 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/skip-proof-final.json; SHA256 b2f7c50de4a051eccb3691495089ee8ff1021b058f6ab567c6e12b4ae0676c89.
Full floor-skip-justifications.json retained: 552 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/floor-skip-justifications.json; SHA256 248055b87ee2bd3084d9236401c2ef21ea056c5de67dbdd5719f3dfe268146c9.
Full floor-skip-sites.txt retained: 36 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/floor-skip-sites.txt; SHA256 486897452a262755460103db1d969e39db17196a9f7eb13d1cc4b2a08f1bae52.
Full publication-prechecks.json retained: 24 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/publication-prechecks.json; SHA256 f58fda2b31652041f1d3a666da330a509fe5ed706c64dbb66464f47a355fb07c.
Full recovered-audit.json retained: 139 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/recovered-audit.json; SHA256 67fd27868154108ae1f5dd9640bf06bb9ff0c1a4916852275e83584814edd3eb.
Full fast-verified.json retained: 12 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/fast-verified.json; SHA256 b49bee884d6ac1b7d2fa89a69ae74471c0e7c3ecf41c06e95bce4fa8967d0a16.
Full ci-required/closure.txt retained: 17 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/ci-required/closure.txt; SHA256 479133ea72bf5c7d50048fc7332c3d3d7425caee6aca8f50bb24339e1b1b834c.
Full ci-required/windows-native-reach.json retained: 23 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/ci-required/windows-native-reach.json; SHA256 46c4ed5ecf7d0afb883e56903c4659b29bc1f1fd92ac6168326b4593060a9f89.

Environment refusal (no criterion verdict): DISK_FLOOR_REFUSED ts=2026-11-09T14:25:52Z mode=run floor_gb=40 free_gb=37.2 path=/ same_device_as=/var/tmp,/home/jaword bead=ga-buojy2e who=jaword repo=/var/tmp/gc-fresh-tree.ybMFaE episode=new count=1 mail=failed
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=80.59 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=84.38 wait_max_load=90.01 wait_mean_load=77.58 wait_readings=61 read_errors=0

Required CI windows-lane completed in a fresh view at the exact gated merge tree. Its command scrubbed GC_/BD_/BEADS_/DOLT_ workload selectors; PASS-THROUGH wrapper is not filesystem isolation.
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=51.22 run_max_load=76.92 run_mean_load=67.80 run_readings=9 wait_first_load=52.07 wait_max_load=71.46 wait_mean_load=42.27 wait_readings=61 read_errors=0

Native floor and supplemental native-base load evidence (both soft waits, not attribution or waiver):

native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=53.92 run_max_load=86.13 run_mean_load=70.68 run_readings=13 wait_first_load=57.47 wait_max_load=89.08 wait_mean_load=69.00 wait_readings=61 read_errors=0
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 53.92; run_max_load: 86.13; run_mean_load: 70.68; run_readings: 13

native-base-comparison: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=63.07 run_max_load=71.64 run_mean_load=68.54 run_readings=4 wait_first_load=68.72 wait_max_load=86.43 wait_mean_load=67.60 wait_readings=61 read_errors=0
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 63.07; run_max_load: 71.64; run_mean_load: 68.54; run_readings: 4

Native-base command scope: focused supplemental SKIP comparison only; never substituted for full-suite evidence. Its 2 PASS / 0 FAIL / 3 SKIP are not added to the suite counts.

Full native-skip-proof/result.json retained: 30 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/native-skip-proof/result.json; SHA256 d15b62ba89f753fbaaf61261835325ceedc8e4bdf08ba0da474a0bff2600d3b8.
Full native-skip-proof/base.log retained: 15 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/native-skip-proof/base.log; SHA256 71c3aadf888fbb01971df7fa3ebed486c77983526cf0a1a8b8044d4bbaf7bf36.
Full native-skip-proof/compare.txt retained: 4 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/native-skip-proof/compare.txt; SHA256 274653d6c9614e613c17ef622088624f6004c27630f37d37945390624155b681.
Full native-skip-proof/wrapper-command.json retained: 12 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/native-skip-proof/wrapper-command.json; SHA256 5d684a9f222cb6ec1cf91d487b3abdcab1d56fb932181b8ccc6a3d508e1795d1.
Full acceptance-static.json retained: 40 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/acceptance-static.json; SHA256 214f01adc569db84c65fa217fa5a48222460ca3e6c520fcf4ded53ff30afa24a.
Full ci-required/windows-lane-result.json retained: 41 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/ci-required/windows-lane-result.json; SHA256 e86c9929e0881f6624b501fa15b45b362ac330989501cf369779bfe839138698.
Full ci-required/runner-policy-result.json retained: 14 lines; /var/tmp/deploy-ga-buojy2e.2p0r3g1a/ci-required/runner-policy-result.json; SHA256 fdc3525329b6ad9f8a888ea9b4bc70afaee4c7d203ffe9dabd160eeb286937c5.

CI job commands (all four command groups exit 0, retained in windows-lane-result.json):

- Build the Windows allowlist: `GOOS=windows go build $(cat .github/windows-lane/build.txt)`.
- Compile the Windows allowlist's tests: `GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)`.
- Build the nogo analyzers and their tests for Windows: `GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...` in `tools/nogo`.
- Build every package and its tests for macOS: `GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...`.
