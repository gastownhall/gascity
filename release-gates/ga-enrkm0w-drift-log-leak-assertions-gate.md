# Release gate: deterministic drift-log leak assertions

**Verdict:** **PASS**

Evaluated 2026-11-01T08:11:45.547280+00:00. Deploy `ga-enrkm0w`; build `ga-jmqlssi`; review `ga-4u3x7hi`; fixes condition `ga-o4hcan`.

- deploy_mode: remote; base_ref: origin/main; push_remote: fork.
- reviewed_commit: `1f97c493f90f83529b181fff8b251b23e78ffbbe` (resolved to a commit).
- gate_base: `aff9be93b11cdc4fee7cd1c7e31108863840b6b9`; gate_base_kind: current-tip.
- gated_merge_head: `ba60f5d78f41cd8eeaa21a9a38f2721e8580d89d`; gated_tree: `06a4c34de73658e50cf84d2861e54a0c67e3cba9`.
- merge_state: clean (origin/main@aff9be93b11cdc4fee7cd1c7e31108863840b6b9); merge_validation: canonical merge result built and vetted. MPR owns merge-time validation.
- Evidence root: `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf`, protected from reaping. Original BEPs, per-job logs, source-site audits and intentional mutation receipts retained.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-4u3x7hi passes the exact source; review notes read in full. No carryover or reviewer-added commit. |
| 2 | Acceptance | PASS — forced scope/port overlap passes, sibling no-controller test passes, and three independent deliberate leak mutants are killed as specified below. |
| 3 | Tests and CI accounting | PASS — full five-phase forced-uncached remote gate, required native floor, by-name execution, policy, drift and four-leaf CI accounting below. |
| 4 | No unresolved HIGH findings | PASS — no blocker, major, minor or should-fix finding in review. |
| 5 | Clean tree | PASS — canonical merge clean before/after tests; exact parents and tree checked in merge-verified.json. Only this release record is added on deploy. |
| 6 | Merge state and build/vet | PASS — clean canonical merge of frozen main with exact source; whole-repository bazel build --config=remote-exec --noremote_local_fallback //... rc=0 (c6.log). Normal scratch commit hooks ran. |
| 7 | Single theme | PASS — two source commits, one test file, +17/-7; deterministic leak assertions. Production code, helper bodies, imports and BUILD files unchanged. |

## Acceptance

The changed `TestWithholdDriftLogOmitsEnvValues` forces `city-44829` into the scope path and uses the identical env-port value. It masks only the exact scope path before checking the port digits. The credential check still scans raw output. The sibling `TestPreflightSaysSoWhenNoControllerIsLive` already forces a `city-4242` path and checks the controller-PID phrase; its source is unchanged.

Both named tests were independently found PASS, with no FAIL/SKIP, in the full canonical suite (`acceptance-by-name.txt`). This is in addition to the owned-test verifier, never a narrowed substitute for criterion 3.

Three expected-red probes ran in separate fresh disposable views of the same canonical merge, with remote Bazel, forced test execution, disabled sharding and the two-test filter. They are acceptance evidence only; their deliberate failures do not enter the canonical suite's counts. Each probe returned Bazel rc=3, the intended named test FAILed with the specified leak diagnosis, the other test PASSed, and neither SKIPped:

| Mutation | Expected assertion killed | Retained evidence |
|---|---|---|
| Emit env-derived port in drift log | TestWithholdDriftLogOmitsEnvValues: env-derived port | acceptance-mutants/port.log and port.json |
| Emit env-derived credential in drift log | TestWithholdDriftLogOmitsEnvValues: env-derived credential | acceptance-mutants/credential.log and credential.json |
| Print controller PID line with no live controller | TestPreflightSaysSoWhenNoControllerIsLive: fixture leaked a PID | acceptance-mutants/pid.log and pid.json |

Probe command: `bazel test --config=remote-exec --noremote_local_fallback --nocache_test_results --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all --test_sharding_strategy=disabled --test_filter=^(TestWithholdDriftLogOmitsEnvValues|TestPreflightSaysSoWhenNoControllerIsLive)$ //cmd/gc:gc_test`. Each probe used the root's mutate-one.sh through fresh-tree-run.sh and isolated-test-run.sh under remote admission; exact commands, assertions and raw logs retained. No mutant touched the canonical tree.

- uncovered_criteria: none; descoped_criteria: none; deploy_stage_criteria: none.
- reviewer_tests_verified: n/a; waiver_ref: none; failure_attribution: none.

## Test evidence

- test_cmd_scope: full-suite; heavy_mode: none (heavy.txt).
- policy_lane: make lint (whole-repository nogo), PASS, rc=0; policy.log.
- format_lane: bazel test --config=remote-exec --noremote_local_fallback //:go_format_test, PASS; format.log.
- drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code, PASS; drift.log.
- fast_lane: all three lanes passed in separate fresh views before runtime setup or the full suite. Canonical merge never written by a lane.
- test_exec_mode: remote-exec, derived from BEP; fallback_reason: none.
- test_exec_bep: 514 fresh first-attempt remote results in five phases; each terminal, inside its own phase window, with both cached flags false and no retries. Predicate/error records are each phase's .evidence.json.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 44.05379867553711, "waited_seconds": 0.001}.
- farm_test_seconds: 9968.014.
- test_counts: 138915 PASS / 0 FAIL / 481 SKIP, top-level result lines summed across the retained per-job logs. Repeated names in different jobs count per job.
- test_log_dir: `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/logs`.
- execution_record: `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/execution.json`.
- owned_topup: [].
- floor_topup: complete; 16/16 rc=0, detailed below.
- latency_leg: complete, independently judged only under the clear-probe latency lane; execution.json retains the probe, commands and measurements. The main integration leg excludes those named budgets.

```text
diff_tests_executed:
  TestWithholdDriftLogOmitsEnvValues PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-4d20b3d28497f844aa39.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name
```

| Phase | Results | Remote | Cached local / remote / retries | Verdict |
|---|---:|---:|---|---|
| unit | 389 | 389 | 0 / 0 / 0 | PASS |
| acceptance | 24 | 24 | 0 / 0 / 0 | PASS |
| integration-packages | 86 | 86 | 0 / 0 / 0 | PASS |
| integration-full | 14 | 14 | 0 / 0 / 0 | PASS |
| integration-latency | 1 | 1 | 0 / 0 / 0 | PASS |

Actual phase flags from this run:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

The unit phase runs make check; acceptance runs both acceptance_test and acceptance_solo_tests with --config=acceptance; integration-packages runs //test:integration_packages with --config=integration; full integration runs //test/integration:integration_test with --config=integration, excluding only the named latency budgets; those run in the fifth, clear-probe lane. gate-criterion3.sh supplies the complete command chain; no hand-selected suite subset is credited.

ICU74 check OK (icu74.txt); runtime setup completed through the wrapper before the suite: rootless Podman socket responds and the native Dolt/cached image match deps.env, with Ryuk disabled and its paired unreaped-container opt-out. Wrapper PASS-THROUGH is disclosed in logs; remote execution is proven independently by BEP and native selectors are scrubbed by the standard floor runner.

### Native floor

load_threshold: 15; load_waited_seconds: 1561; load_wait_timed_out: 0; run_start_load: 14.30; run_max_load: 15.19; run_mean_load: 14.67; run_readings: 7.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1561 wait_timed_out=0 run_start_load=14.30 run_max_load=15.19 run_mean_load=14.67 run_readings=7 wait_first_load=63.08 wait_max_load=85.70 wait_mean_load=55.43 wait_readings=53 read_errors=0
```

- `go test -tags integration -count=1 -v -run '^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-0.log`.
- `go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-1.log`.
- `go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-2.log`.
- `go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-3.log`.
- `go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-4.log`.
- `go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-5.log`.
- `go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-6.log`.
- `go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-7.log`.
- `go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-8.log`.
- `go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-9.log`.
- `go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-10.log`.
- `go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-11.log`.
- `go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-12.log`.
- `make test-fsys-darwin-compile`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-13.log`.
- `bash scripts/test-push-gate-lock.sh`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-14.log`.
- `bash scripts/test-local-concurrency.sh`: rc=0; `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf/c3/evidence/remote/floor-logs/floor-15.log`.

## Skip integrity

skip_inventory: 467 remote events plus 39 native events, subtests and repeated configurations included. SKIP_SITE_AUDIT rows 506 sites 281 unresolved 0 rc 0. All source locations resolved at the canonical merge, all are UNCHANGED with line_in_hunk=no; the source diff owns exactly TestWithholdDriftLogOmitsEnvValues and has no SHARED-HUNKS or HELPER-HUNKS. Unit, integration and acceptance_a test-binary import closures are retained in final-skip-reach.json and final-skip-import-*.txt.

skip_justification: the unchanged skips name lane, platform, missing optional infrastructure/tool, or intentional fixture guards; their exact printed reasons and source sites are in skip-site-rows.json, skip-sites.txt and the inventories. The only edited function is one test body; no skipped test can call it, and no shared helper or production input changed. No TOUCHED skip site or changed-hunk reach exists, so the same-command BASE-reproduction trigger is absent. No owned test or owned subtest skipped; no waiver or failure attribution was used.

## Required CI job accounting

ci_lane_run: n/a (no CI-config diff).
ci_required_closure: ci_required_triggers.py `/var/tmp/gc-merge-validate.ga-enrkm0w.8hr5x19j` `aff9be93b11cdc4fee7cd1c7e31108863840b6b9` `1f97c493f90f83529b181fff8b251b23e78ffbbe` fork; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; four LEAF jobs. Exact output: ci/closure.txt.

ci_required_accounting:

| LEAF job | Status | Command/result/log |
|---|---|---|
| changes | LOCAL-PASS | Closure command above rc=0; ci/closure.txt. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=ci/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc=0; ci/runner-policy.log. |
| credential-provider-windows | NOT-TRIGGERED | Exact closure-tool row: credential_provider filter does not match. |
| pack-gate | NOT-TRIGGERED | Exact closure-tool row: packs filter does not match. |

The check aggregator gets no independent status. These read-only accounting commands ran sequentially in a fresh canonical view through the isolation wrapper; formula 3e explicitly runs closure and runner-policy as shown without a test load wait.

- windows_reach: none (windows_reach.sh rc=0, empty output; ci/windows-reach.txt).
- deferred_to_pr_ci: none; not_covered: none; host_gaps: none.
- This PASS authorizes opening the PR. MPR must confirm current required PR CI on the exact head before merging.

## Setup attempt integrity

The first helper invocation refused an already-existing, empty reserved temporary directory before any source checkout, build or test ran. Its c6/ and fast/ logs remain unchanged. Only that verified-empty directory was removed; corrected c6-v2 and fast-v2 runs use the same source and frozen base and supply the actual build/fast evidence. A preparation guard initially still referenced the retained first fast/ run; it stopped before any suite execution. launch3-v2 points to the verified fast-v2 result. Both preparation attempts remain retained; no candidate build or test failure was retried into a pass.

All original evidence remains under `/var/tmp/gc-deploy-ga-enrkm0w.n_iho6zf`. Only this gate record is added to the reviewed source on the isolated deploy branch.
