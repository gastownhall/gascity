# Release gate: T3 bridge runtime-provider contract

**Verdict:** **PASS**

Evaluated 2026-11-23T23:46:56.452396+00:00. Re-gate `ga-0xbn6b14` for PR #8652; source deploy `ga-kklbiu2`, review `ga-3b6674s`, feature `ga-uz5t3a.1`.

- deploy_mode: remote; base_ref: origin/main; repository: gastownhall/gascity; existing PR head repository: quad341/gascity.
- accepted_source: `e92b0592798803987687888ea8d8e0857af10b24`; original reviewed source: `fb80773de38b2d8ad575185caa104c0845be90c1` (resolved commit and ancestor).
- source_acceptance: mayor mail gcg-15599617 accepts the exact builder conflict-resolution merge for a fresh gate under gm-6ieewc. It preserves the earlier maintainer merge; rebase is forbidden. Publish only as a plain fast-forward to deploy/ga-kklbiu2-gate.
- gate_base: `b551c44aa01fe011ebfb3d0ee2f6876e2f09a1d3`; gate_base_kind: current-tip.
- gate_head: `e92b0592798803987687888ea8d8e0857af10b24`; gated_merge_head: `08bb37ad15d68a76a825d922316ec08d3538a19f`; gated_tree: `c6f028e5261579cd2c250d1e9f73a887bf4b1390`.
- merge_state: clean (origin/main@b551c44aa01fe011ebfb3d0ee2f6876e2f09a1d3); merge_validation: the pinned merge result passed full build/nogo and the complete test chain. MPR must validate current-head CI and merge compatibility.
- Prepublication compatibility observation: accepted source also merges cleanly with origin/main `a8aa15f076848914b7a613bbd9b8ae4b7ec48294` at 2026-11-23T23:46:55.833050+00:00. This is a compatibility check; the tested base remains the frozen gate_base above.
- Evidence root: `/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu`. Original BEPs, normalized logs, native-floor logs, source/site/closure evidence and base comparisons are retained. Earlier gate attempts are historical and supply no test credit here.
- waiver_ref: none; failure_attribution: none; uncovered_criteria: none; descoped_criteria: none.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS: ga-3b6674s passes the original reviewed source; its history and maintainer repairs remain present. The mayor explicitly accepted the builder merge-forward source in gcg-15599617 for this fresh gate. |
| 2 | Acceptance | PASS: six checks below verified against code and fresh retained results, including all six diff-owned tests. |
| 3 | Tests, policy, drift, skip integrity, CI accounting | PASS: 553 fresh local Bazel results, 16/16 native-floor commands, both latency tests, five fast lanes, 20/20 required base SKIP comparison groups, and six-leaf CI accounting below. |
| 4 | No unresolved HIGH findings | PASS: zero. Original review and current own-PR comments/reviews checked; accepted snapshot-isolation and request-write/startup-cancellation repairs are retained. |
| 5 | Clean gated tree | PASS: pinned source and canonical merge clean; only this gate record is added to the publication commit. Final clean status is required again before push and clearance. |
| 6 | Merge state and build/vet | PASS: clean merge of the frozen base and accepted source; c6-rbe terminal complete, units 1/1, failed 0. Full bazel build --config=remote-exec --noremote_local_fallback //... and nogo succeeded. |
| 7 | Single feature theme | PASS: T3 shared runtime conformance, its hermetic fixture, environment-map isolation, and exact ledger proof/options bookkeeping; 12 changed paths including this record. |

## Acceptance evidence

1. TestT3Bridge_RunProviderConformance calls RunProviderTestsWithOptions directly, with a direct NewSeamBacked factory and the explicit generic DuplicateStartReconnects option.
2. The stateful in-repository HTTP/WebSocket fixture uses unique names, temporary directories, overridden discovery and cleanup. TestT3BridgeStatefulSnapshotIsolated verifies independent snapshots.
3. The complete shared lifecycle/discovery/metadata/observation/input/interrupt contract passed: 43 result lines including the parent, zero FAIL/SKIP.
4. Both exact t3bridge and exec-prefix registrations name that same exact-constructor proof. The ledger rejects missing or mismatched literal options and skip callbacks; fresh catalog and validator tests passed.
5. TestCatalogMatchesProductionWiringAndDocumentation passed by name in unit-4f0468303977036e4cd4.log; the full T3 bridge and providerledger targets passed.
6. DuplicateStartReconnects defaults false; existing strict duplicate-start behavior and generic provider neutrality remain. The private cfg.Env clone preserves #9454 startup cancellation and write bounds. TestRunLifecycleTestsDoesNotStopUnownedConcurrentStart passed; its intentional concurrent-start SKIP is separately measured at the pinned base.

## Test execution

- fast_lane: PASS; format, shell guards, BUILD regeneration drift, generated artifacts and strict storeclass baseline/growth returned rc0 in separate fresh views. fast-results.json and fast/ retain commands and raw output.
- policy_lane: full nogo build and policy guards PASS; c6-rbe, fast-results.json and the canonical make -k check log.
- drift_lane: make bazel-sync plus git diff --exit-code PASS; generated-artifact and storeclass checks also PASS.
- known_red_precheck: proceed, nothing-known-red; check-routed-test-rows green at the gated merge.
- heavy_mode: none; test_cmd_scope: full-suite; test_exec_mode: local; fallback_reason: not-own-author (legacy commit authors are not mapped by authenticated GitHub provenance). No provenance override or mixed-suite stitching.
- test_cmd: gate-criterion3.sh selected `/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-local-bazel-run.py` with --leg-dir `/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu/c3/evidence/local` --owned `/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu/owned.tsv`; the canonical recorder reports criterion3_valid=true.
- test_counts: 159116 PASS, 0 FAIL, 614 SKIP (top-level result occurrences across 570 retained logs; repeated names count per job).
- test_log_dir: `/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu/c3/evidence/local/logs`; execution.json: `/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu/c3/evidence/execution.json`.
- test_exec_bep: unit.bep.json, unit.bep.leg2.json, acceptance.bep.json, integration-packages.bep.json and integration-full.bep.json under c3/evidence/local. Local audit rejects cache credit, remote strategies, retries, missing terminals and out-of-window results.
- admission: {"cap": 3, "class": "local-heavy", "floor_gib": 40.0, "free_gib": 51.435237884521484, "prewait_s": 870.207, "waited_seconds": 0.001}.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}; farm_test_seconds: 0.
- ICU74: verdict OK, recorded host libraries; c3/evidence/local/icu74.txt. Rootless Podman API probe passed before suite setup.
- owned_topup: []; floor_topup: complete, all 16 commands rc0, including environment-absence top-ups, beadstest, Darwin compile and runner self-tests. Native commands run under mayor ruling gcg-15584257 until gm-uqk5td lands.
- latency_leg: measured, complete, first attempt, clear probe; TestGcBdLatencyBudget PASS and TestGcSeatHookLatencyBudget PASS. Neither ran in integration-full. Raw probe and results: c3/evidence/local/latency-leg.json.
- ci_lane_run: n/a (no CI-configuration change).

| Phase | Fresh results | Failed | Execution |
|---|---:|---:|---|
| unit | 425 | 0 | {"linux-sandbox": 425} |
| unit-leg2 | 1 | 0 | {"processwrapper-sandbox": 1} |
| acceptance | 25 | 0 | {"linux-sandbox": 25} |
| integration-packages | 87 | 0 | {"linux-sandbox": 87} |
| integration-full | 15 | 0 | {"linux-sandbox": 15} |

The unit phase ran make -k check; the other phases ran the canonical acceptance targets, //test:integration_packages and //test/integration:integration_test through gascity-bazel-legs.sh. All carried --config=fork-cache --config=ci --rewind_lost_inputs --remote_download_outputs=toplevel --keep_going --nocache_test_results --modify_execution_info=GoTestRun=+no-cache --test_env=GO_TEST_WRAP_TESTV=1. Integration phases added --config=integration; acceptance added --config=acceptance. Integration-full excluded only the registry latency tests for their own lane. Actual argv and phase windows are in bazel-chain.json.

```text
diff_tests_executed:
  TestT3BridgeStatefulSnapshotIsolated PASS (unit-0e7d33178260e20a6bca.log)
  TestT3Bridge_RunProviderConformance PASS (unit-0e7d33178260e20a6bca.log)
  TestCatalogBindsExecCompositionToSeamBackedContract PASS (unit-4f0468303977036e4cd4.log)
  TestCatalogReturnsIndependentEntries PASS (unit-4f0468303977036e4cd4.log)
  TestValidateProofRefsChecksContractRunnerOptions PASS (unit-4f0468303977036e4cd4.log)
  TestValidateRequiresOptionsProofToUseOptionsRunner PASS (unit-4f0468303977036e4cd4.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name
```

## Skip integrity and load

- skip_inventory: 598 original Bazel SKIP events, plus native-floor SKIPs; source audit covers 672 events including subtests at 316 sites. current-skip-audit.json records every printed reason, source site, test-binary import closure and named shared helper in reach.
- Every printed source site has line_in_hunk=no. The only touched skipped helper is RunLifecycleTestsWithOptions; its new duplicate-reconnect branch does not change any skip condition. All other test bodies are unchanged.
- SHARED-HUNKS: T3 conformance fixture helpers, stateful provider test helpers and providerledger waiver-test bookkeeping. They are test-only functions, so downstream production imports do not compile them. HELPER-HUNKS: runtimetest.RunLifecycleTestsWithOptions. Its false default preserves existing suites; every skipped test binary reaching these packages was measured conservatively at the pinned base.
- 53 import-closure queries succeeded. All 20 same-command base groups (537 SKIP events) returned compare rc0 and ALL PRE-EXISTING, including the four native-floor groups. base-skip-plan.json and base-skip-evidence/report.json bind each group to its exact source logs, command, BEP and comparison output. Other SKIPs lie outside the changed packages and helper reach; source/site evidence and printed platform, optional-fixture, fast-unit or subprocess-helper reasons are retained. No diff-owned SKIP was accepted.
- skip_justification: platform-specific paths, explicit optional/live-environment conditions, subprocess helpers and existing contract capability exclusions; unchanged and measured as required above. Environment-absence cases also ran through the mandatory native floor and its by-name accounting.
- Comparator recovery: the first base group passed, then the collector hit an artifact-retention AttributeError. The corrected collector reused that result and both Windows vet results, without retrying tests. Original error evidence is retained; final report.complete=true covers all 20 groups.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=870 wait_timed_out=0 run_start_load=14.50 run_max_load=142.35 run_mean_load=60.56 run_readings=111 wait_first_load=42.46 wait_max_load=56.12 wait_mean_load=30.86 wait_readings=31 read_errors=0 prewait_s=870
LOAD_GATE_TELEMETRY v1 state=degraded reason=governor mode=on mode_source=default mode_word=- dir=/var/tmp/gc-regate-ga-0xbn6b14-accepted.tf1dz2fu/c3/telemetry/gate.w1-1620163 sweeps=3349 gaps=1 source_errors=0 unsupported=6 bytes=30434130 overhead_cpu_ms=23846
```

- load_threshold: 15; load_waited_seconds: 870; load_wait_timed_out: 0; run_start_load: 14.50; run_max_load: 142.35; run_mean_load: 60.56; run_readings: 111.

Load threshold 15; admission prewait 870.207 seconds; wait did not time out. The canonical run began at load 14.50, max 142.35, mean 60.56 across 111 readings. Native-floor and each named base measurement retain their separate bounded load waits. No load result is a test verdict. Telemetry reported degraded governor data as shown; all actual test results passed.

The comparator reported these exact normalization notes; original logs were retained unchanged:

```text
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestEventsMirrorInheritsSetgidGroup (base gct37 and 3827560750 and 4060130409, head gct36 and 2960525393 and 1427961951)
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestEventsMirrorInheritsSetgidGroup (base gct34 and 3493246713 and 2668810466, head gct38 and 541126868 and 1920116854)
```

## Required CI jobs

- ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-ga-0xbn6b14.coc65yc8 b551c44aa01fe011ebfb3d0ee2f6876e2f09a1d3 e92b0592798803987687888ea8d8e0857af10b24 fork; rc0; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status. Raw closure: required-ci/closure.stdout.
- ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Exact closure command rc0; required-ci/closure.stdout. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=... python3 .github/workflows/scripts/runner_policy.py, rc0, fresh merged-tree view; required-ci/runner-policy.stdout and runner-policy-steps.json. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | TRIGGERED. runner_windows resolves to blacksmith-4vcpu-windows-2025. Windows reach listed providerledger inputs; GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider and its -tags=integration form both rc0; base-skip-evidence/windows-vet-results.json and windows-vet.log. Native tests remain for PR CI. |
| pack-gate | NOT-TRIGGERED | Exact tool row: packs filter did not match; required-ci/closure.stdout. |
| windows-lane | LOCAL-PASS | All four exact run steps rc0: Windows allowlist build, test compilation, Windows nogo analyzers/test compilation and full Darwin build/test compilation. Job/step environments preserved; sequential detached fresh view behind load/isolation wrappers. required-ci/windows-lane.stdout and windows-lane-steps.json. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows allowlist. Its separate GOOS=windows test-binary closure succeeded and reaches no changed inputs; windows-accounting/reach.json. Tests compiled in windows-lane; execution awaits Windows CI. |

- windows_reach: internal/testutil/providerledger/BUILD.bazel, ledger.go, ledger_test.go, proof.go and waiver_stagger_test.go; tool rc0. The separate Windows allowlist test reach is none (rc0).
- deferred_to_pr_ci: credential-provider-windows (non-linux-runner); windows-lane-test (non-linux-runner).
- not_covered: none; host_gaps: none.
- Merge condition: CI / required green at the exact current PR head, and each deferred native Windows job successful or legitimately skipped by its own false path filter. MPR holds merge authority; this gate does not merge the PR.
