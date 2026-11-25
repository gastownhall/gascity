# Doctor embedded-bd test mode release gate

**Verdict:** **PASS**

Evaluated 2026-11-25T01:34:21Z.

Deploy bead: ga-9am4t0hz; build: ga-u5azpz; review: ga-cbpaqpmu.

DEPLOY_MODE: remote; BASE_REF: origin/main.

reviewed_commit: 09a195126dad68b66fb665e8e081e1a67a7312a4

gate_base: e5727ef42bf9e0ed59cfecf64df31aeb89a184ef

merge_scratch_head: bec2a3e5ba2eb2378640ea48674f709ec861fd89

gated_tree: e79c0d8ccf31edac302b80e787e29ae7c155f855

merge_state: clean (origin/main@e5727ef42bf9e0ed59cfecf64df31aeb89a184ef)

gate_base_kind: current-tip

merge_validation: gastownhall/gascity merge queue tests the merged result; MPR owns merge-time validation and the merge. No rebase performed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review ga-cbpaqpmu is closed with verdict pass for the resolved reviewed commit; no carryover or reviewer-added commit. |
| 2 | Acceptance criteria | PASS | Three embedded real-bd fixtures call pinEmbeddedBDHome: t.Setenv(BEADS_TEST_MODE, "1") and a test-owned HOME. The server-backed fixture retains testOwnedHome with its sentinel explanation. The pinned-bd fixtures use stub binaries. Both helper contract tests and all three real-bd top-ups passed. |
| 3 | Full tests and required jobs | PASS | Five remote phases and all 16 floor steps passed. The exact Windows/macOS CI compile commands passed. Native Windows tests are deferred to PR CI; the table below accounts for all six LEAF jobs. |
| 4 | Unresolved HIGH findings | PASS | Review reports no blocking, major, minor, security or style finding; no unresolved HIGH findings. |
| 5 | Clean final branch | PASS | The isolated source checkout was clean before writing this record. This record is the only deployer addition; the push is guarded by a clean-tree check after its commit. |
| 6 | Merge state and build/vet | PASS | Clean scratch merge onto the pinned base; make lint (Bazel nogo over //..., 18,451 actions) exit 0. |
| 7 | Single feature theme | PASS | Four changed files under internal/doctor: one test helper, two contracts, three embedded-fixture call sites and their BUILD source entry. All three source commits cite ga-u5azpz; no unrelated feature or internal agent configuration. |

## Test evidence

test_cmd_scope: full-suite

test_counts: {"FAIL": 0, "PASS": 159168, "SKIP": 618} (top-level result lines summed across retained jobs; repeated executions count per job).

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.4NwohJ --base e5727ef42bf9e0ed59cfecf64df31aeb89a184ef --bead-json /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9am4t0hz/bead.json --owned /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9am4t0hz/owned.tsv --run-dir /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence; service exit 0. Commands and flags below are the executed full-scope lanes from TESTING.md and the gate runner.

```text
unit: env -u MAKEFLAGS -u GNUMAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEOVERRIDES -u MAKEFILES LANGUAGE=C make -k check (includes bazel test //...)
acceptance: bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
integration-packages: bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
integration-full: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ //test/integration:integration_test
integration-latency: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ --test_sharding_strategy=disabled --run_under=<recorded farm-load probe> //test/integration:integration_test
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/logs; gate-test-evidence.py read 570 logs, 502 with Go result lines.

```text
diff_tests_executed:
  TestCustomTypesCheck_MissingTypes PASS (unit-7bce503b7c011c7475f3.log)
  TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint PASS (floor-6.log; also SKIP: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint in unit-7bce503b7c011c7475f3.log (PASS in another job))
  skip_reason: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:491: bd binary not on PATH
  TestCustomTypesCheck_TableDrift PASS (floor-6.log; also SKIP: TestCustomTypesCheck_TableDrift in unit-7bce503b7c011c7475f3.log (PASS in another job))
  skip_reason: TestCustomTypesCheck_TableDrift (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:321: bd binary not on PATH
  TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext PASS (floor-6.log; also SKIP: TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext in unit-7bce503b7c011c7475f3.log (PASS in another job))
  skip_reason: TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:423: bd binary not on PATH
  TestPinEmbeddedBDHomeOwnsHome PASS (unit-7bce503b7c011c7475f3.log)
  TestPinEmbeddedBDHomeRunsBDSubprocessesInTestMode PASS (unit-7bce503b7c011c7475f3.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name
```

skip_justification: the three doctor environment SKIPs are repaired by actual PASS results in floor-6.log (3.21s, 1.86s and 7.09s). There are no native doctor SKIPs. Other SKIPs are existing platform, unavailable-provider/service, opt-in or explicit lane conditions in other packages. The diff changes only doctor test code and its test source list; neither the helper nor these tests is compiled into another test binary. SHARED-HUNKS: pinEmbeddedBDHome is called only by the three owned embedded fixtures and the two owned helper contracts, all PASS; the server fixture owns the explanatory comment and also PASSes. The full remote and native inventories retain each reason.

policy_lane: fresh-tree-run.sh -- bash lint-lane.sh; gate-base.sh run -- make lint NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback"; exit 0.

format_lane: fresh-tree-run.sh -- run-pinned-lint.sh -- make fmt-check; pinned golangci-lint 2.14.0; exit 0.

drift_lane: fresh-tree-run.sh -- bash drift-lane.sh; make bazel-sync then git diff --exit-code; exit 0. Each fast lane used its own view before the suite.

failure_attribution: none (0 FAIL).

waiver_ref: none (no gascity waiver path).

ci_lane_run: n/a (no CI configuration change).

heavy_mode: none (classify: HEAVY_MODE mode=none shard_required=- watch=-).

test_exec_mode: remote-exec; test_exec_bep: all 554 results executed remotely, no cached result, retry or other strategy; all phase windows verified.

rbe_state: worker-env matches the pinned base; certificate valid; remote enabled; kill switch absent.

fallback_reason: none.

admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 45.037841796875, "waited_seconds": 0.001}.

farm_test_seconds: 14251.741.

owned_topup: no targeted remote repair required; the three owned environment SKIPs passed in the native floor.

floor_topup: complete; all 16 steps exit 0 (environment-absence top-ups by package, pinned beadstest module-cache proof, Darwin compile and both runner self-tests). Store selector variables are scrubbed for every native command.

latency_leg: measured, first attempt, farm probe clear after 50s; both latency tests PASS, omitted from the integration-full leg as prescribed.

delta_gate: shadow, full suite, reason=not-gate-pr; no narrowing applied.

load_threshold: 15

load_waited_seconds: 0 (outer remote class; no host-load admission wait)

load_wait_timed_out: 0

run_start_load: 16.56

run_max_load: 32.58

run_mean_load: 20.04

run_readings: 63

local_floor_load: threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.61 run_max_load=19.77 run_mean_load=18.22 run_readings=9.

## Required CI jobs

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.4NwohJ e5727ef42bf9e0ed59cfecf64df31aeb89a184ef 09a195126dad68b66fb665e8e081e1a67a7312a4 fork; exit 0; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status. Runs-on outputs resolve to Blacksmith Ubuntu 24.04 or Windows 2025.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/runner-policy.log |
| changes | LOCAL-PASS | Closure tool exit 0: four changed files, suite_mode filtered; packs, credential_provider and shared filters all no match. /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX; needs.changes.outputs.credential_provider == true did not match. |
| pack-gate | NOT-TRIGGERED | Tool row: needs.changes.outputs.packs == true did not match. |
| windows-lane | LOCAL-PASS | Exact run commands in /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/windows-lane.sh; fresh-tree-run outermost, load-gate-run threshold 15/max-wait 1800, isolated-test-run; exit 0, WINDOWS_LANE_PASS; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/windows-output.log |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows runner unavailable locally. GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps -json over .github/windows-lane/test.txt; exit 0, no changed file in its own test-binary closure; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/native-windows-reach.json |

windows_reach: none (windows-lane-test, independently derived for its complete allowlist; credential-provider-windows is NOT-TRIGGERED).

deferred_to_pr_ci: windows-lane-test (non-linux-runner)

not_covered: none

host_gaps: none

windows_lane_load: threshold=15 waited_seconds=210 wait_timed_out=0 run_start_load=14.64 run_max_load=16.66 run_mean_load=15.85 run_readings=3.

This PASS authorizes PR-open. Merge requires current-head CI / required and native Windows tests to be green; MPR owns merge authority.

## Retained evidence

Full execution.json retained: 6059 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/execution.json; SHA256 a3c1338fd0b7c3118b123c836dba5848b21b46bba0640aff183501dffbd3dd18.

Full remote skip inventory retained: 4834 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/skip-inventory.json; SHA256 1d79ede88c38f35ee17f1324a8ccfa2fc2106bf3b42448b9baad4c97908ca464.

Full native skip inventory retained: 578 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/native-skip-inventory.json; SHA256 85a79fcded02231243ee665c2afd14475576c9cfdac4379b15acc207961b4d18.

Full owned-test proof retained: 34 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/owned-explain.txt; SHA256 dd723d86723f5dde662a0bff52d00481e27f948477bce9469953b5ba23f63238.

Full by-name test evidence retained: 15 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/test-evidence.txt; SHA256 8dcd74baa566b76859c40db2b2da4a0ac2670204083b14afef219e4faec6fc3e.

Full floor steps retained: 1056 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/floor-topup.json; SHA256 c0cecfda8ec2fd6ecb1f06698a9ec6fe7cc5d1b938d271bff432e6ce793fcb60.

Full CI closure retained: 17 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/closure.txt; SHA256 5f09555a56193bbcb00a5d6277e947baf9bbb86eb7a9c9c96ab67c41923322d4.

Full cross-platform CI commands retained: 10 lines; /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.ci-required/windows-lane.sh; SHA256 a4822ab4553912550098831de8d9daacc38bdbadb960b9bd7924342682480356.

Fast-lane build/vet, policy, format and drift logs: /var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/fast-lane.

Compact execution record (large arrays retained above):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote","gate_base":"e5727ef42bf9e0ed59cfecf64df31aeb89a184ef","gate_head":"09a195126dad68b66fb665e8e081e1a67a7312a4","admission":{"class":"remote","floor_gib":25.0,"free_gib":45.037841796875,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"e5727ef42bf9e0ed59cfecf64df31aeb89a184ef","head":"09a195126dad68b66fb665e8e081e1a67a7312a4","merge":"bec2a3e5ba2eb2378640ea48674f709ec861fd89","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"c05ed146e000cdc81fb79638ac78521bca1b423076d41cc410a3e53c3e831017"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14251.741,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":180.797,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=89.72 load5=103.35 load15=57.80 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=50s load1=49.68 load5=90.28 load15=55.74 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 42.04 77.15 54.08 16/2262 58927"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38511}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-9am4t0hz.c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":216208}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":159168,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestCustomTypesCheck_MissingTypes PASS (unit-7bce503b7c011c7475f3.log)\n  TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint PASS (floor-6.log; also SKIP: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint in unit-7bce503b7c011c7475f3.log (PASS in another job))\n  skip_reason: TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:491: bd binary not on PATH\n  TestCustomTypesCheck_TableDrift PASS (floor-6.log; also SKIP: TestCustomTypesCheck_TableDrift in unit-7bce503b7c011c7475f3.log (PASS in another job))\n  skip_reason: TestCustomTypesCheck_TableDrift (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:321: bd binary not on PATH\n  TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext PASS (floor-6.log; also SKIP: TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext in unit-7bce503b7c011c7475f3.log (PASS in another job))\n  skip_reason: TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext (unit-7bce503b7c011c7475f3.log): checks_custom_types_test.go:423: bd binary not on PATH\n  TestPinEmbeddedBDHomeOwnsHome PASS (unit-7bce503b7c011c7475f3.log)\n  TestPinEmbeddedBDHomeRunsBDSubprocessesInTestMode PASS (unit-7bce503b7c011c7475f3.log)\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```
