# Custom-type test fixture readiness release gate

**Verdict:** **PASS**

Deploy bead: ga-aug0vmz6; build: ga-8mj4m71b; review: ga-5qw5ial6.
Evaluated: 2026-11-18T01:04:44.877878+00:00
Reviewed source: `83dcd47e3dad61335d5a24f8cf21b290889e3430`.
Pinned base: `5b7728de3436779e7e2a0e3230d6f0534517e4bd`; canonical merge: `95e2cd566150f498501fdc0d6805a03cc52fed2a`; tree: `8f097eac97a97647705d5204ab4f567ffe7dc0c1`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review PASS names the resolved source; both source commits reference ga-8mj4m71b. No carryover or reviewer-added commit. |
| 2 | PASS | One test file only; immutable environment helper replaces duplicate readiness overrides with 120 whole seconds; both fixture inits use it and print Dolt logs on setup failure. The new unit test and real-server test pass; two server starts delayed 15 seconds also pass. |
| 3 | PASS | Canonical full-suite chain, required native floor, zero failed tests; changed tests verified by name; CI accounting and skip proofs below. |
| 4 | PASS | Review has no security, style or HIGH finding; unresolved HIGH count 0. |
| 5 | PASS | Isolated deploy branch starts exactly at the reviewed commit, clean before this record. Normal pre-commit hooks validate the record commit; final clean status is read back before push. |
| 6 | PASS | Clean merge at the pinned base; whole-tree Bazel build and nogo succeed. |
| 7 | PASS | Single theme: make the custom-type test fixture tolerate delayed Dolt startup. Only internal/doctor/checks_custom_types_test.go changes before this record. |

```text
deploy_mode: remote; push_remote: fork; base_ref: origin/main
merge_state: clean (origin/main@5b7728de3436)
gate_base_kind: current-tip
merge_validation: clean canonical merge built and tested; merge authority validates current merge state and PR CI before landing
ancestry_scope: accepted deploy/build/review ids only; no unrelated or internal-document commits
criterion6: bazel build --config=fork-cache //...; exit 0; whole-tree nogo includes lint/vet
policy_lane: make fmt-check-changed via run-pinned-lint.sh; shell guards and check-hooks; 13 static Bazel policy targets; exit 0
drift_lane: make bazel-sync then git diff --exit-code and empty git status; 7 generated-artifact targets; exit 0
static_cache_disclosure: policy 7 cached/13 results; drift 6 cached/7 results; permitted static reuse, never runtime-suite evidence
openapi_breaking: make openapi-breaking-check OPENAPI_BREAKING_BASE=5b7728de3436779e7e2a0e3230d6f0534517e4bd; exit 0
complexity: zero-growth form (ii); base and merge have the same 251 rows; pinned complexity-diff prints no threshold changes; baseline untouched
ci_lane_run: n/a (no CI-config change)
failure_attribution: none
waiver_ref: none
heavy_mode: none (classify verified)
test_cmd_scope: full-suite
test_cmd: gate-criterion3.sh --repo <canonical merge> --base 5b7728de3436779e7e2a0e3230d6f0534517e4bd --bead-json bead-criterion3.json --owned owned.tsv --run-dir criterion3/evidence
phase_commands: make -k check; bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; bazel test --config=integration //test:integration_packages; bazel test --config=integration //test/integration:integration_test (full excluding latency, then latency-only)
phase_flags: --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1; per-phase BEP retained
test_counts: 155335 PASS, 0 FAIL, 613 SKIP (top-level lines summed per job)
test_log_dir: /var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: five raw BEPs independently audited; 541 first-attempt remote results; no cached, retried, out-of-window or other-strategy result
farm_test_seconds: 13609.789
fallback_reason: none
rbe_state: eligible own-code source, current worker pin, valid certificate, kill switch absent
admission: remote cap 2, floor 25 GiB, observed 62.1867561340332 GiB, wait 0.001 seconds
floor_topup: 16/16 commands exit 0; native scrubbed store/session selectors; pinned bd v1.3.4; no failed or unfinished native test
owned_topup: none beyond canonical floor repair
latency_leg: measured, clear probe after 50 seconds, both latency tests PASS; no latency test ran in integration-full
test_environment: pinned local bd, podman socket and cached pinned Dolt image verified; Ryuk disabled with opt-out and recent testcontainer-sweep; ICU74 verdict OK
```

Diff-owned evidence:

```text
diff_tests_executed:
  TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint PASS (floor-6.log; also SKIP: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint in unit-7bce503b7c011c7475f3.log (PASS in another job))
  skip_reason: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:491: bd binary not on PATH
  TestCustomTypesServerInitEnvSetsLoadTolerantReadyTimeout PASS (unit-7bce503b7c011c7475f3.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
TestRepositoryLedgerMatchesCensusAndDocumentation PASS (resource census acceptance)
supplemental_slow_start: 15 seconds before each of two real Dolt server starts; 44.58 seconds; exit 0; 2 PASS, 0 FAIL, 0 SKIP; no captured server PID survives
```

The supplemental reproduction uses the explicitly required native-floor command and its scrubbed environment with no ambient readiness override. It supplements the complete Bazel suite; it does not replace it.

Skip justification: remote environment-absence cases received the canonical local floor. The owned server-backed test is a real local PASS. The two unchanged doctor neighbors also pass locally; their remote SKIPs are measured against the pinned base below. Every other skipped test and skip site is untouched; the only changed shared helper is private to doctor test compilation and cannot enter another test binary. Remaining skips cover optional live tools/backends, platform and process-lane selection, fixture sentinels or explicitly disabled scenarios. Printed reasons, per-test sites and root conditions are retained in the pointed audit files. No owned SKIP or failure is excused.

```text
COMPARE pre-existing   TestCustomTypesCheck_TableDrift  head=checks_custom_types_test.go:321  the base run SKIPs it for the same reason  reason="bd binary not on PATH"
COMPARE pre-existing   TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext  head=checks_custom_types_test.go:423  the base run SKIPs it for the same reason  reason="bd binary not on PATH"
COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
not_owned_skip_evidence: remote sites + native sites retained; only the owned doctor site is TOUCHED; two doctor neighbors UNCHANGED with matching measured base SKIPs; every other skip has no SHARED-HUNKS or HELPER-HUNKS in reach
shared_hunks: internal/doctor/checks_custom_types_test.go customTypesServerInitEnv; referenced only by the owned test and its new unit test
helper_hunks: none
skip_measurement_recovery: base artifact recovered byte-for-byte from Bazel output plus its standard header and verified against BEP CAS hash; canonical 0x16 transport framing removal only. Matching scoped head/base probes both contain exactly the two unchanged tests. Original owned-test and full-suite evidence remain unchanged.
```

Host load (canonical chain and required native floor, reported separately):

```text
criterion3/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=29.33 run_max_load=28.45 run_mean_load=14.28 run_readings=43 wait_first_load=29.33 wait_max_load=29.33 wait_mean_load=29.33 wait_readings=1 read_errors=0
criterion3/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.67 run_max_load=12.90 run_mean_load=12.31 run_readings=7 wait_first_load=9.67 wait_max_load=9.67 wait_mean_load=9.67 wait_readings=1 read_errors=0
final-acceptance/slow-start-wrapper.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.83 run_max_load=14.94 run_mean_load=14.94 run_readings=1 wait_first_load=22.03 wait_max_load=22.03 wait_mean_load=18.12 wait_readings=14 read_errors=0
final-acceptance/base-skips.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.77 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=14.77 wait_max_load=14.77 wait_mean_load=14.77 wait_readings=1 read_errors=0
```

CI-required job accounting:

```text
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-aug0vmz6-ia3xy99g/merge 5b7728de3436779e7e2a0e3230d6f0534517e4bd 83dcd47e3dad61335d5a24f8cf21b290889e3430 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; aggregators only roll up needs
ci_required_accounting: changes | LOCAL-PASS | /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/closure.txt
ci_required_accounting: runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/runner-policy.sh; exit 0; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/runner-policy-output.log
ci_required_accounting: credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
ci_required_accounting: pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
ci_required_accounting: windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/run-job.py windows-lane; exit 0; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/windows-lane-output.log
ci_required_accounting: windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner=blacksmith-4vcpu-windows-2025; reach={"files": [], "closure_dirs": 10, "roots": ["./internal/credentialprovider", "./internal/genshell/...", "./internal/processgroup"]}
windows_lane_command: GOOS=windows go build $(cat .github/windows-lane/build.txt); exit 0
windows_lane_command: GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt); exit 0
windows_lane_command: GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...; exit 0
windows_lane_command: GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...; exit 0
windows_reach: native Windows roots computed from workflow allowlist; {"files": [], "closure_dirs": 10, "roots": ["./internal/credentialprovider", "./internal/genshell/...", "./internal/processgroup"]}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
```

Gate PASS authorizes opening the PR. Merge requires green CI / required and success of the deferred native Windows job on the current PR head. No host-precondition deferral or uncovered job.

Compact canonical execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":62.1867561340332,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13609.789,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":117.797,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=75.64 load5=85.83 load15=45.02 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=50s load1=44.66 load5=75.99 load15=43.82 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.53 68.37 42.76 12/1057 55127"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38828}},"log_dir":"/var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":216078}}},"test_cmd":{"elided_array":{"items":5,"bytes":2069}},"test_counts":{"PASS":155335,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint PASS (floor-6.log; also SKIP: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint in unit-7bce503b7c011c7475f3.log (PASS in another job))\n  skip_reason: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:491: bd binary not on PATH\n  TestCustomTypesServerInitEnvSetsLoadTolerantReadyTimeout PASS (unit-7bce503b7c011c7475f3.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```

Retained evidence:

Full complete execution retained: 6000 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/execution.json; SHA256 ff39f9f9429a91238b6bf8828d46aa3093cf2bf774fe32f7bdb909968f327113.
Full remote artifact digests retained: 5412 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/remote-artifact-sha256.json; SHA256 bf3974b13f54e18b43382a623f7ca8b55da40811cea976909f1b112f76a5e6f0.
Full raw farm skip inventory retained: 4794 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/remote/skip-inventory.json; SHA256 892a53ed14b358825f3a505c261536b5865f48178c9548f225ad942c27f92bf4.
Full remote skip mapping retained: 5428 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/skip-site-mapping.json; SHA256 64dac92069e89ba49879f2090a16e98fca76b9bcf0bc0fa71916e4c56a842fde.
Full remote skip sites retained: 315 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/skip-site-evidence.txt; SHA256 dc66c4a5d6417f02c5bb54e20de9812428c8632bbb3d9f48dc72975331c32209.
Full additional skip sites retained: 4 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/skip-extra-sites.txt; SHA256 dbb72fa62a6b51abe01b3a7e2afa4ab0f882f3ae7e313b26e6d5e8f7c15b0c75.
Full native skip audit retained: 586 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/native-skip-audit.json; SHA256 6092147ff9ec1bdcaa83da9ebf66293f10b0c3bd83c6ae9e703ea82867c386d7.
Full native skip sites retained: 45 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/native-skip-sites.txt; SHA256 a8509058ffbf400e9d458173c116a29b43e53377798ee8333cc44d070c50d41f.
Full owned-test derivation retained: 25 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/owned-explain.txt; SHA256 8853277da2b6904e7ae41250660b24445dae79828d28f76dec80cb2aaebd5b6d.
Full fast lane audit retained: 52 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/fast-independent-audit.json; SHA256 d54a925ee8703d1ab75a565f931ab133e6073f3934ee66555b982d2da4b31d3f.
Full native floor steps retained: 1056 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/criterion3/evidence/remote/floor-topup.json; SHA256 bf20ac383680fc3ac894437f68112d3aa746011f7eeb8e7b57ff304dae47ac38.
Full CI accounting retained: 110 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-advance-result.json; SHA256 8acb8e0f1ef88dab30c87c9f2b6dedf7b97e01c75f1b4e66591c060f1b5d43ee.
Full workflow commands retained: 38 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/ci-required/windows-lane-job.json; SHA256 af829fe55a858f202c6eeae2f8ff8890d808facdcb4d231fa02e77e12592404a.
Full supplemental acceptance retained: 60 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/final-acceptance-recovered-result.json; SHA256 092bc81865cfd95d5fa6b54b262f5c97921eaa2a6d252fa337ac2dcfe250e4eb.
Full base artifact recovery retained: 10 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/final-acceptance/base-artifact-proof.json; SHA256 82fc43efca447286a7b32616cec83ddcfbd59efcbf7d3594c599807b2353ac16.
Full head skip artifact manifest retained: 30 lines; /var/tmp/gate-ga-aug0vmz6-ia3xy99g/final-acceptance/head-skips-artifacts.json; SHA256 f64f2707e00a26f7df02f9cf951d4c5ee2c6a5a957fe4489af88b7ec2b8f893d.
