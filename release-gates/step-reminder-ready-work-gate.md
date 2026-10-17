# Release gate: step reminders name ready work

**Verdict:** **PASS**

Deploy bead: ga-lu6csh7. Build: ga-qi3glw4. Review: ga-xz40nmk.
Reviewed source: `2990bd7c1426157db718c1387382a8f021ca9ab0`.
Tested merge: `66505748fdbe7610dd3738026e4b9c2196dd61f3`; tree `e8839a5b91f3a8dae20924a2a7da1c3b556239d0`.
Frozen base: `origin/main@a2c9a5a112dbf3933b972ec786f15e28094d32d5`. DEPLOY_MODE=remote; BASE_REF=origin/main.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-xz40nmk binds PASS to the exact source above. Independently ran reviewer-added-tests.sh check e495de6be5a02971100cfd0ab9b32d4fc042e7ba -> 2990bd7c1426157db718c1387382a8f021ca9ab0 --bead ga-qi3glw4; output reviewer-tests files=cmd/gc/wisp_step_inject_ready_test.go. |
| 2 | Acceptance criteria | PASS | Claimed work precedes routed work; only claim-ready routed beads bridge; the entry step is the first unblocked open step. Dependency read errors withhold the advisory reminder. Nine owned regressions pass in shared and split stores, in both unit and integration-package lanes. The existing pre-claim bridge for ready work remains supported. |
| 3 | Full tests | PASS | Fresh make check, acceptance including solo targets, integration-packages and FULL integration all pass. Mandatory native floor completes 16/16 steps. 116870 top-level PASS, 0 FAIL, 370 SKIP across 506 retained per-job logs; counts sum executions, not unique names. All nine owned tests PASS by name. No failed-verdict retry or waived criterion. |
| 4 | No open HIGH review findings | PASS | Reviewer verdict PASS; all recorded advisories are nonblocking. Zero unresolved HIGH findings. |
| 5 | Tested tree clean | PASS | Canonical scratch HEAD, tree and both parents independently checked; git status --porcelain empty. This record is added after testing and is the deploy branch's only added change. |
| 6 | Merge state and build/vet | PASS | Clean materialized merge on the frozen current tip; tree equals git merge-tree --write-tree. Full-tree Bazel build and make vet/nogo succeed. Real pre-commit hooks ran during materialization; make check-hooks confirms .githooks active. |
| 7 | Single feature theme | PASS | Three source files: step-reminder readiness logic, its regressions and BUILD wiring. Ancestry guard passes for ga-lu6csh7/ga-qi3glw4; no independent feature theme. |

```text
merge_state: clean (origin/main@a2c9a5a112dbf3933b972ec786f15e28094d32d5)
gate_base_kind: current-tip
merge_validation: materialized predicted merge tree, built and vetted; no self-rebase
reviewer_tests_verified: e495de6be5a02971100cfd0ab9b32d4fc042e7ba -> 2990bd7c1426157db718c1387382a8f021ca9ab0 files=cmd/gc/wisp_step_inject_ready_test.go
heavy_mode: none
fast_lane: PASS before criterion3
policy_lane: fresh-tree pinned format/lint-changed, shell guards, nine policy/format Bazel targets PASS
drift_lane: separate fresh-tree make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code; empty git status PASS
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
farm_test_seconds: 7816.728
criterion3_valid: true
owned_topup: none required (all nine already PASS)
floor_topup: complete, 16/16 steps PASS
skip_inventory: 356 remote events, including 71 environment-absence; 39 native skip events including subtests
skip_inventory_audit: 395 events, 266 unique source sites UNCHANGED, shared_hunks_in_file=0; no unresolved sites
retained_artifacts: 980/980 raw remote artifacts independently SHA256 verified
ci_lane_run: n/a (no CI configuration changed)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none (no failure)
load_threshold: 15
load_waited_seconds: 1802
load_wait_timed_out: 1
run_start_load: 86.35
run_max_load: 83.33
run_mean_load: 68.98
run_readings: 12
```

The load fields above describe the mandatory native floor. Its bounded wait expired and the standard soft-timeout policy then ran the checks; all completed with exit 0. Remote admission bypassed the host wait at its 25 GiB floor, cap 2, with 107.230 GiB free and 0.001 seconds waited. The wrapper recorded pass-through of the canonical scratch, not a new isolated-clone claim. RBE certificate is valid to Jan 9 2027; kill switch absent; worker-env pins match. No fallback occurred.

| Phase | Results | Fresh remote | Cached | Retries | Window and verdicts |
|---|---:|---:|---:|---:|---|
| unit | 371 | 371 | 0 | 0 | PASS |
| acceptance | 24 | 24 | 0 | 0 | PASS |
| integration-packages | 81 | 81 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |

Actual phase commands use flags `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1` and a separate `--build_event_json_file=/var/tmp/deploy-ga-lu6csh7.k3pi_p_l/c3-evidence/remote/<phase>.bep.json`:

- unit: `make check` (Bazel test //... plus documented shell guards).
- acceptance: `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`.
- integration-packages: `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`.
- integration-full: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v //test/integration:integration_test` (no smoke filter).

The full execution flags, argv, BEP, per-phase retention manifests, raw XML/logs and hashes are retained at `/var/tmp/deploy-ga-lu6csh7.k3pi_p_l/c3-evidence`. Nogo's flags include --keep_going --output_groups=nogo_fix --config=remote-exec. The final by-name union is `/var/tmp/deploy-ga-lu6csh7.k3pi_p_l/c3-evidence/remote/by-name.txt`; test_log_dir is `/var/tmp/deploy-ga-lu6csh7.k3pi_p_l/c3-evidence/remote/logs`.

Diff-owned tests (each PASS in both unit and integration-packages):

- TestStepReminderEntryStepIsTheFirstUnblockedStep — PASS.
- TestStepReminderNamesNoStepWhenItsDependenciesCannotBeRead — PASS.
- TestStepReminderNamesNoStepWhileEveryOpenStepIsBlocked — PASS.
- TestStepReminderPrefersClaimedWorkOverARoutedMolecule — PASS.
- TestStepReminderReadsEntryStepEdgesInOneBatch — PASS.
- TestStepReminderRereadsEdgesTheBatchDoesNotAnswer — PASS.
- TestStepReminderRoutedBridgeOffersOnlyWhatAClaimCouldTake — PASS.
- TestStepReminderSkipsARoutedBeadStillBlocked — PASS.
- TestStepReminderTreatsABlockerItCannotReadAsUnsatisfied — PASS.

The native floor runs every environment-absence selection by package (parent selected for subtests), plus internal/beads/beadstest, Darwin compile, push-gate-lock and local-concurrency self-tests. All step exits are 0; non-owned SKIPs are inventoried rather than silently counted as passes. The source diff owns no skip condition. All 266 remote/native source sites were checked with diff-owned-tests.py --site against the frozen base and tested merge: UNCHANGED, func_touched=no, shared_hunks_in_file=0. The only changed test file is new; its helpers, types and constants serve only the nine new tests, with no init, TestMain, environment mutation or SKIP. The production change has no test-environment or skip-guard effects. Remaining skips are lane/platform/tool/fixture availability guards or deliberate provider-conformance stand-ins, independent of step reminder readiness.

Native floor steps:

| Step | Result | Command |
|---|---|---|
| cmd/gc | PASS | `go test -tags integration -count=1 -v -run ^(TestReplay\|TestSessionEventPumpLiveHerdr\|TestSimCoverageGate\|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace\|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$ ./cmd/gc` |
| examples/gastown | PASS | `go test -tags integration -count=1 -v -run ^(TestAttachedRigScopeShellToken\|TestJsonlExportPushRetryDelayIsLocaleIndependent)$ ./examples/gastown` |
| internal/beads | PASS | `go test -tags integration -count=1 -v -run ^(TestMetadataEmptyStringClearContract\|TestProbeVersionsAgainstHostBinaries)$ ./internal/beads` |
| internal/beads/beadstest | PASS | `go test -tags integration -count=1 -v -run ^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$ ./internal/beads/beadstest` |
| internal/beads/contract | PASS | `go test -tags integration -count=1 -v -run ^(TestMigrateJournalFileMatchesPinnedBeads)$ ./internal/beads/contract` |
| internal/beads/exec | PASS | `go test -tags integration -count=1 -v -run ^(TestBrProviderConformance)$ ./internal/beads/exec` |
| internal/doctor | PASS | `go test -tags integration -count=1 -v -run ^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint\|TestCustomTypesCheck_TableDrift\|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$ ./internal/doctor` |
| internal/mail/exec | PASS | `go test -tags integration -count=1 -v -run ^(TestMCPMailBridgeSourceable\|TestMCPMailConformance\|TestMCPMailCrossPodNameResolution\|TestMCPMailProjectKeyIsolation\|TestMCPMailReplyMissingOriginalIsNotFound)$ ./internal/mail/exec` |
| internal/runtime/herdr | PASS | `go test -tags integration -count=1 -v -run ^(TestActivityLive\|TestHerdrConformance\|TestProviderLive\|TestProviderLiveClaudeKindPath\|TestProviderLiveOccupantSwapKeepsLiveness\|TestSessionEventsLive)$ ./internal/runtime/herdr` |
| internal/runtime/runtimetest | PASS | `go test -tags integration -count=1 -v -run ^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$ ./internal/runtime/runtimetest` |
| internal/runtime/tmux | PASS | `go test -tags integration -count=1 -v -run ^(TestGetKeyBinding_CapturesUserBinding\|TestGetKeyBinding_SkipsGasTownBindings\|TestIsGTBinding_DetectsGasTownBindings\|TestSetBindings_PreserveFallbackOnRepeatedCalls)$ ./internal/runtime/tmux` |
| test/acceptance/helpers | PASS | `go test -tags integration -count=1 -v -run ^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig\|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot\|TestRealBdProvisionLeavesTheHostHomeAlone)$ ./test/acceptance/helpers` |
| test/integration | PASS | `go test -tags integration -count=1 -v -run ^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$ ./test/integration` |
| fsys-darwin-compile | PASS | `make test-fsys-darwin-compile` |
| push-gate-lock-selftest | PASS | `bash scripts/test-push-gate-lock.sh` |
| local-concurrency-selftest | PASS | `bash scripts/test-local-concurrency.sh` |

Native skip inventory (including subtests; the top-level tally is 14):

- cmd/gc / TestReplay: reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set
- cmd/gc / TestSessionEventPumpLiveHerdr: session_event_pump_live_test.go:23: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- cmd/gc / TestSimCoverageGate: reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset
- internal/beads / TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother: metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance
- internal/beads / TestMetadataEmptyStringClearContract/Postgres/SetMetadata: metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance
- internal/beads / TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch: metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance
- internal/beads / TestMetadataEmptyStringClearContract/Postgres/Update: metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance
- internal/beads/exec / TestBrProviderConformance: br_test.go:18: br not available
- internal/runtime/herdr / TestActivityLive: activity_live_test.go:20: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/herdr / TestHerdrConformance: conformance_test.go:18: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/herdr / TestProviderLive: provider_live_test.go:15: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/herdr / TestProviderLiveClaudeKindPath: kindpath_live_test.go:105: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/herdr / TestProviderLiveOccupantSwapKeepsLiveness: panebinding_live_test.go:21: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/herdr / TestSessionEventsLive: events_live_test.go:40: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_AbsentSessionRefuses: conformance.go:551: provider cannot fence session input
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MatchingFenceDelivers: conformance.go:508: provider cannot fence session input
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MismatchedFenceRefusesEveryInput: conformance.go:525: provider cannot fence session input
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions: conformance.go:238: Start(skip-start-14): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions: conformance.go:271: Start(skip-start-18): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes: conformance.go:367: Start(skip-start-25): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix: conformance.go:349: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped: conformance.go:333: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions: conformance.go:294: Start skip-start-21: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering: conformance.go:313: Start skip-start-22: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions: conformance.go:421: Start(skip-start-30): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue: conformance.go:395: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop: conformance.go:404: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession: conformance.go:61: Start shared session: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions: conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession: conformance.go:84: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError: conformance.go:93: first Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions: conformance.go:209: Start(skip-start-11): provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped: conformance.go:193: Start: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning: conformance.go:170: Start sentinel: provider environment unavailable
- internal/runtime/runtimetest / TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning: conformance.go:134: Start: provider environment unavailable
- internal/runtime/tmux / TestGetKeyBinding_CapturesUserBinding: tmux_test.go:3663: not inside tmux — need server for bind-key
- internal/runtime/tmux / TestGetKeyBinding_SkipsGasTownBindings: tmux_test.go:3637: not inside tmux — need server for bind-key
- internal/runtime/tmux / TestIsGTBinding_DetectsGasTownBindings: tmux_test.go:3689: not inside tmux — need server for bind-key
- internal/runtime/tmux / TestSetBindings_PreserveFallbackOnRepeatedCalls: tmux_test.go:3719: not inside tmux — need server for bind-key

Evidence root: `/var/tmp/deploy-ga-lu6csh7.k3pi_p_l`. Audits: evidence-audit.json, skip-site-evidence.txt, native-skip-audit-input.json. Both detached prep and criterion3 runs completed with zero failed units; the continuation requires this final manual audit and did not publish anything. The gate record adds no production or test changes to the reviewed source.
