# Release gate: pack command scope refusal

**Verdict:** **PASS**

- Deploy bead: ga-29bi5s9a; build: ga-0l533y; review: ga-mqd3k4d.
- Reviewed commit: 818e37e5ecf071ed48d7247f83e1213ea50ab062 (resolved commit object).
- Deploy mode: remote; base branch: main; base ref: origin/main; push remote: origin.
- Gate base: 7b5614ac70d1031111f072370536bfd440199b34.
- Validated merge: bf02bd15b1468ff703c4eb8609d31a2307c9035f; tree: a7f3542486f5370b4cc862b5bfd5857edcd93ef6.
- Evidence date: 2026-11-19 UTC; retained run: /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi.
- PASS authorizes PR opening; merge requires current-head CI and merge-authority validation.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS | PASS | ga-mqd3k4d verdict pass and deploy_commit match the resolved reviewed source. No carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | Five owned tests passed in unit and integration-packages. Cross-city/empty/unresolvable post-leaf flags refuse before script execution; eager/lazy agree. Same-city scope, help, and terminator arguments retain their contract. Core suspend still honors its city flag. Pack spec 1.2.12 states refusal and remedies. |
| 3 | Full suite and required lanes | PASS | Five fresh remote phases plus local floor, policy/format/generated checks and required-job accounting below. 157131 PASS / 0 FAIL / 613 SKIP; all five owned tests PASS. |
| 4 | No unresolved HIGH findings | PASS | Reviewer records zero HIGH findings. Nonblocking CWD cleanup is ga-2xfrqmxl. |
| 5 | Final branch clean | PASS | Reviewed source plus committed gate record on isolated deploy branch; post-commit status verified clean before push. |
| 6 | Merge state and build/vet | PASS | Clean merged result at pinned current-tip base; full-tree Bazel build and nogo lint/vet both rc0. |
| 7 | One feature theme | PASS | Five paths and three reviewed commits implement pack-command scope refusal. All three cite ga-0l533y. Ancestry guard passes with deploy/build IDs; no internal agent paths or unrelated feature commits. |

merge_state: clean (origin/main@7b5614ac70d1)
gate_base_kind: current-tip
gate_base: 7b5614ac70d1031111f072370536bfd440199b34
merge_validation: merge result built and vetted here; merge authority validates the eventual merged result

## Build and fast lanes

Criterion 6, both exit 0:

~~~text
bazel build --config=remote-exec --noremote_local_fallback --keep_going //...
make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'
~~~

Logs: /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c6-build.log and c6-vet.log. Detached C6 complete, 1/1 units, failed=0.

fast_lane: PASS; four independent fresh views of the pinned merge, detached chain complete rc0
policy_lane: fresh-tree-run.sh -> gate-base.sh run -> bash fast-policy.sh; rc0
format_lane: go_format_test in fast-policy.sh, then make -k check; PASS
drift_lane: fresh-tree-run.sh -> gate-base.sh run -> bash fast-drift.sh, fast-openapi.sh, fast-buildsync.sh; each rc0
policy_attribution: none
ci_lane_run: n/a (no CI-config change)

Policy includes required shell guards, formatting, policy targets and resource census.
Generated checks include genspec, genschema, API client, dashboard dist/shared client and
lockfiles. OpenAPI breaking checks run separately. BUILD sync runs make bazel-sync
BAZEL_SYNC_FLAGS=--config=remote-exec then git diff --exit-code, with no difference.
The unit suite also ran make -k check and its shell guards.

Full policy command retained: 7 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/fast-policy.sh; SHA256 1b18b97ec621f591786bb6c3705982579ab9cfd34c0b5498885386bb81cf8c23.
Full generated command retained: 6 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/fast-drift.sh; SHA256 f0cfd72ae6c13e13f7cf8c7ebe96be16332057af69046081344265c897d8e73c.
Full OpenAPI command retained: 6 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/fast-openapi.sh; SHA256 bd8534450bb1124c050e1538c5ccdf27e6d0044e40aa6a63f405f6d6fa96085e.
Full BUILD sync command retained: 7 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/fast-buildsync.sh; SHA256 cdf1605d2809f41a7603fb0bb26343a0fb51512a4e6d8ecad424db3c251ee2b0.
Full fast output with fresh-view records retained: 9 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/fast-terminal-output.txt; SHA256 042fd87a2f11482b00528ec8621799a9f8f1fd9b340acb2d53ed8667702d1ad9.

## Full-suite evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-ga-29bi5s9a.VDkOqN --base 7b5614ac70d1031111f072370536bfd440199b34 --bead-json /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/bead-c3.json --owned /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/owned.tsv --run-dir /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence
test_cmd_scope: full-suite

Unit make -k check, acceptance, integration-packages, integration-full excluding the
latency list, then integration-latency containing only that list. Per-phase flags:

~~~text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
~~~

Every phase forces --nocache_test_results and --noremote_local_fallback.
Every BEP finished.overallSuccess is true, and all target summaries are PASSED.

| Phase | Result actions | Remote executions | Cached | Retries | Predicate |
|-------|----------------|-------------------|--------|---------|-----------|
| unit | 415 | 415 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

test_counts: 157131 PASS / 0 FAIL / 613 SKIP
Top-level result lines across 557 retained logs, including the floor; repetitions
across jobs count per job. These are not counts of distinct tests.
Independent gate-test-evidence.py re-read:

~~~text
diff_tests_executed:
  TestCoreCommandCityFlagStillSelectsCity PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-cd6930bb0d0da8646500.log)
  TestE1LazyExplicitScopeBeforeLeafWinsOutsideAmbientCity PASS (integration-packages-75f7330b9b673df65e4f.log, unit-10cc7795c0ec199a5530.log)
  TestE1LazyMissingTreeMatchesEagerFlagOwnership PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-912ab643330b35bd0b6b.log)
  TestE1ScopeLookingArgsAfterLeafOwnership PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-4d20b3d28497f844aa39.log)
  TestPackLeafScopeAfterLeafNeverSilentlyRetargets PASS (integration-packages-509ef0a6cd81866833cc.log, unit-088cc38d26d094729229.log)
GATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name
~~~

test_exec_mode: remote-exec (BEP-derived; local floor additive, no stitched replacement)
test_exec_bep: five remote/*.bep.json files; 541 remote results, neither cached flag, attempt 1, phase windows valid
rbe_state: config marker present; kill switch absent; certificate valid through 2027-01-09; worker/tree environment hashes match
fallback_reason: none
admission: remote; 25 GiB floor; 66.3503 GiB free; cap 2; waited 0 seconds
farm_test_seconds: 12204.616
owned_topup: none needed
floor_topup: complete; 16/16 steps rc0; 24 PASS / 0 FAIL / 14 SKIP included in combined counts
latency_leg: complete; probe clear; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS on attempt 1
heavy_mode: none (computed)
waiver_ref: none
test_log_dir: /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/logs

All 1082 saved raw artifacts were independently rehashed: zero digest mismatches.
Floor also passed Darwin compile and push-lock/local-concurrency self-tests
(67/25 shell assertions). Native compile checks are not claimed as runtime tests.

### Skip audit

skip_inventory: 599 farm entries (509 other, 90 environment-absence); every environment-
absence parent selected for the canonical local floor. Non-owned top-up skips remain
inventoried as the remote protocol permits. No owned parent or subtest skipped or failed.
failure_attribution: none

skip_justification: remaining skips concern platform-specific cases, opt-in live-provider/
process/timing/migration inputs, unavailable provider fixtures/capabilities, or explicitly
pending work. None removes evidence for the five pack-scope tests. The only skip in the
changed test file is TestE1PreLeafBooleanHelpNoScopeEager. Its unconditional ambient-CWD
test-binary guard predates the diff: --explain shows no hunk in its body and no shared/
helper hunk in that file; --site line 814 says UNCHANGED, line_in_hunk=no, func_touched=no,
shared_hunks_in_file=0. Scope checking cannot bypass this unconditional skip.
Printed ga-klo4gz reason retained in the inventory.

The 14 floor top-level skips: replay/simulation inputs unset; seven opt-in herdr journeys;
four bind-key tests outside tmux; br absent. These do not exercise post-leaf scope.
No default tmux server was altered. Podman ping passed before C3; pinned bd v1.3.4
and disk floors verified; ICU74 OK. Wrapper says PASS-THROUGH, NOT ISOLATED:
the committed fresh views and remote BEP are evidence, not a scrubbed ambient environment.

Floor load:
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 12.42
run_max_load: 25.09
run_mean_load: 14.74
run_readings: 45

Remote admission has no host-load wait. Latency probe clear:
load1=4.52 load5=1.52 load15=0.55, 32 CPUs.

Full skip inventory retained: 4794 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/skip-inventory.json; SHA256 596ac18f2acc5320dcbdfdad493802330b0de632cbc2756f1e490c693acc1281.
Full skip audit retained: 45 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/skip-audit.json; SHA256 72a103470745f87ed9e2745e4afe94dfadc86df842ca5048335f049a96b11649.
Full skip-site proof retained: 1 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/skip-site-proof.txt; SHA256 aa8e9e43f7ee5d2d6c9ae4883baeca7571c8b45ec5a569af8f150823aee1ecb3.
Full owned-test explain retained: 113 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/owned-explain.txt.stdout; SHA256 cbfb5d660080d2ed95183e959cb85673be927c8bc800b7b397616ccab8a4f90e.
Full artifact digest audit retained: 36 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/artifact-audit.json; SHA256 67303301366c911150fc77e34ea63975fb43b67b3fa1b524174ca4dc2e3ea8e0.
Full floor steps and commands retained: 1056 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/floor-topup.json; SHA256 29b484d56fbe47025c8ac595ca61fb2c5eac0571a5272dfc5ec64a67b315694c.
Full latency leg retained: 55 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/latency-leg.json; SHA256 154ee6b1ae476ec965966598832d24ed499a7ca5d7666b3feb93469bc95b42df.
Full execution record retained: 5999 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/execution.json; SHA256 ee372af179c8133407971c713af051735908347518628ba209abadbb9ba134fe.

Compact execution:

~~~json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":66.35030364990234,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12204.616,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":81.988,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=4.52 load5=1.52 load15=0.55 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 23.37 6.85 2.40 19/1631 57841"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39570}},"log_dir":"/var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":220368}}},"test_cmd":{"elided_array":{"items":5,"bytes":2104}},"test_counts":{"PASS":157131,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestCoreCommandCityFlagStillSelectsCity PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-cd6930bb0d0da8646500.log)\n  TestE1LazyExplicitScopeBeforeLeafWinsOutsideAmbientCity PASS (integration-packages-75f7330b9b673df65e4f.log, unit-10cc7795c0ec199a5530.log)\n  TestE1LazyMissingTreeMatchesEagerFlagOwnership PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-912ab643330b35bd0b6b.log)\n  TestE1ScopeLookingArgsAfterLeafOwnership PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-4d20b3d28497f844aa39.log)\n  TestPackLeafScopeAfterLeafNeverSilentlyRetargets PASS (integration-packages-509ef0a6cd81866833cc.log, unit-088cc38d26d094729229.log)\nGATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name\n"}
~~~

## Required CI job accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-ga-29bi5s9a.VDkOqN 7b5614ac70d1031111f072370536bfd440199b34 818e37e5ecf071ed48d7247f83e1213ea50ab062 origin; rc0; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status

| Job | Status | Evidence |
|-----|--------|----------|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc0; runner-policy.log. Outputs select Ubuntu Linux and blacksmith-4vcpu-windows-2025. |
| changes | LOCAL-PASS | Closure tool rc0; all three filters no match; suite_mode filtered; closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX, NOT-TRIGGERED, if needs.changes.outputs.credential_provider == 'true'. |
| pack-gate | NOT-TRIGGERED | Tool row: linux, NOT-TRIGGERED, if needs.changes.outputs.packs == 'true'. |
| windows-lane | LOCAL-PASS | Workflow's exact cross-compile commands in windows-lane.sh, detached fresh view -> load gate -> isolation wrapper; GATE_RUN_EXIT rc=0 state=complete; windows-lane-output.txt. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows go test -count=1 allowlist; own GOOS=windows test-binary closure from .github/windows-lane/test.txt computed, 10 module package directories, no changed inputs; windows-native-reach.json. |

ci_required_accounting: six LEAF jobs each carry one status; check and ci-required are aggregators
windows_reach: windows-lane-test none (own allowlist closure rc0; credential-provider helper not substituted)
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

Cross-compile commands use -exec /bin/true to compile tests: build proof only.
Job load summary: threshold=15, waited_seconds=0, wait_timed_out=0,
run_start_load=9.47, run_max_load=15.96, run_mean_load=13.79, run_readings=3.
Native Windows tests and CI / required must be green at the PR head before merge.
The merge authority watches PR CI; the deployer does not merge.

Full CI closure retained: 17 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/ci-required/closure.txt; SHA256 184e14617a532b35e250144eb44197db3013184baa8c24907659eab0afff101f.
Full native Windows reach retained: 35 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/ci-required/windows-native-reach.json; SHA256 300f0264ff98fb02d71275165b49a7dc47adca7b71122da602744f2e9f09d491.
Full cross-compile commands retained: 16 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/ci-required/windows-lane.sh; SHA256 4a479e943f0036ddff34efcfa4907ab6cefdc15372f3203d090a2f2582a136f9.
Full cross-compile output retained: 357 lines; /var/tmp/deploy-ga-29bi5s9a-20261119.dT4uB2Vi/ci-required/windows-lane-output.txt; SHA256 7da79a14d660679d316112775e4e9cea4a60e845c97aa6a6e8ad4131c007674c.

## Review context

Reviewer marked the raw os.Chdir ledger increase (64 to 68) nonblocking and filed
ga-2xfrqmxl to use t.Chdir in the two new tests. Resource-census targets passed;
this record grants no policy exception. Architect residuals are out of scope.
No CI config, dependency, API or dashboard change. PROJECT_MANIFEST.md is absent;
supplied release criteria and TESTING.md govern.

The PR must state: post-command --city/--rig selecting another city used to be
ignored and now exits 1 without executing the script. Put cross-city scope before
the pack binding, or change directory to the target city.

Publication preflight: origin push dry-run rc0, with all 298 pre-push targets passing.
Direct origin access supersedes the earlier fork assumption; origin and fork CI closure
computations are byte-identical. Active staged pre-commit docsync passed.
