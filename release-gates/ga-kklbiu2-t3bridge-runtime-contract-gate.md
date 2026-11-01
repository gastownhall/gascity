# Release gate: T3 bridge runtime-provider contract

**Verdict:** **PASS**

Evaluated 2026-11-01T06:56:37.564498+00:00. Deploy bead `ga-kklbiu2`; build/rework `ga-yr75x5`; feature `ga-uz5t3a.1`; review `ga-3b6674s`.

- deploy_mode: remote; base_ref: origin/main; push_remote: fork; repository: gastownhall/gascity.
- reviewed_commit: `fb80773de38b2d8ad575185caa104c0845be90c1` (resolved as a commit, never a branch tip).
- gate_base: `0f4e1b316662bc3c0c0c423cbe99c66df1538c0d`; gate_base_kind: current-tip.
- gated_merge_head: `23ba97189080d66662290dd7256810a00d5e63f1`; gated_tree: `e5d48763ecdf6346c31d1c5638ab277754f59ed8`.
- merge_state: clean (origin/main@0f4e1b316662bc3c0c0c423cbe99c66df1538c0d); merge_validation: merge result built and vetted here. Merge authority remains MPR; this PASS authorizes PR-open.
- Evidence root: `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p` (protected from reaping); all original BEPs, CAS-digest-verified artifacts, per-job logs and comparison inputs retained.
- reviewer_tests_verified: `eb07ae2f706f46a283171d5e97fa823b03265f40` -> `fb80773de38b2d8ad575185caa104c0845be90c1` files=internal/testutil/providerledger/ledger_test.go (reviewer-added-tests.sh independently rerun during recovery).
- uncovered_criteria: none; descoped_criteria: none; deploy_stage_criteria: none.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-3b6674s passes the exact reviewed commit. Independently checked reviewer-added coverage from the builder green commit with reviewer-added-tests.sh: only internal/testutil/providerledger/ledger_test.go was added/extended. Receipt: reviewer-tests.txt. |
| 2 | Acceptance | PASS — six checks below exercised independently; all five owned tests passed. |
| 3 | Tests, policy, drift and CI accounting | PASS — full five-phase uncached remote suite, mandatory native floor, skip integrity and four-leaf CI accounting below. No waiver or failure attribution. |
| 4 | No unresolved HIGH findings | PASS — zero; review read in full. Nonblocking follow-ups ga-ik3vsbb (stateful fake snapshot copying) and ga-e44qywg (optional default/reconnect self-test guard) remain separate work. |
| 5 | Clean gated tree | PASS — canonical source tree clean before and after the suite; exact parents and tree equality verified in merge-verified.json. The release record is the only prepared deploy addition. |
| 6 | Merge state and build/vet | PASS — clean canonical merge of frozen base plus reviewed source; bazel build --config=remote-exec --noremote_local_fallback //... succeeded, 1,448 targets and 22,426 actions, 193.064 s. Normal commit hook lint and docs check completed. c6.log / c6-output.log. |
| 7 | Single feature theme | PASS — seven source commits, eleven changed files, one T3 provider proof: hermetic fixture, generic reconnect option, strict ledger validation, regenerated ledger table, and the environment-map copy needed for repeated starts. |

## Acceptance evidence

1. `TestT3Bridge_RunProviderConformance` calls `RunProviderTestsWithOptions` directly with a factory returning `NewSeamBacked`; the approved generic `DuplicateStartReconnects` option is explicit. The architecture decision in ga-uz5t3a.1 amends the original plain-runner requirement.
2. The in-repository stateful WebSocket/HTTP server uses unique session names, temporary state/work directories, cleanup and overridden discovery; no live T3 process is needed.
3. All 40 shared conformance subtests passed (41 result lines including the parent), with zero FAIL/SKIP: lifecycle, discovery, metadata, observation, nudge, fenced input, interrupt and shared sessions. `acceptance-unit-evidence.json` and the retained t3bridge logs.
4. Both `runtime.builtin.t3bridge` and the T3 constructor under `runtime.builtin.exec` point to that same checked proof. Literal option validation rejects mismatches, missing required true and skip callbacks; both reviewer-added validation tests passed.
5. T3 bridge and providerledger targets passed; `TestCatalogMatchesProductionWiringAndDocumentation` passed by name. The current authoritative Bazel gate supplies the older acceptance text's package-test coverage plus nogo and policy checks.
6. Shared runtime code contains no T3-specific branch or identifier. The option defaults to false and keeps the strict duplicate-start path. The reuse switch, AllowThreadReuse and DecideThreadReuse are unchanged. The narrow `Start` environment-map clone is the disclosed race fix needed when the new test repeats a start with the same configuration; it isolates the watcher read without changing reuse decisions. The review explicitly assessed this scope detail.

## Test execution

- fast_lane: PASS 2026-11-01T05:17:03Z, before runtime setup or any suite launch. Separate fresh views ran make lint (full nogo), //:go_format_test and make bazel-sync plus git diff --exit-code. policy.log, format.log and drift.log retain commands and empty-diff evidence. No lane wrote the canonical scratch.
- policy_lane: make lint, PASS, rc=0; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/policy.log`.
- drift_lane: make bazel-sync followed by git diff --exit-code, PASS, rc=0; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/drift.log`.
- skip_justification: all nonzero skips audited by source site and import closure, with same-command BASE comparisons wherever required; see Skip integrity below. No diff-owned skip.
- test_cmd_scope: full-suite; heavy_mode: none (classifier receipt heavy.txt); waiver_ref: none; failure_attribution: none.
- test_exec_mode: remote-exec, derived from BEP; fallback_reason: none; owned_topup: [] (not needed).
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 40.68007278442383, "waited_seconds": 0.001}.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- farm_test_seconds: 11847.937.
- test_counts: **138573 PASS, 0 FAIL, 486 SKIP**, top-level Go result lines summed across 530 retained logs; repeated names in different jobs are counted per job. These counts differ deliberately from the 514 Bazel shard results.
- test_log_dir: `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/logs`; execution.json: `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/execution.json` (full structured evidence; not pasted here).
- test_exec_bep: five verified phases, detailed below; structured predicates in execution.json.
- skip_inventory: execution.json plus native-skip-inventory.json; combined source audit in skip-site-rows.json.
- floor_topup: complete, 16/16 steps rc=0; commands and load evidence below.

```text
diff_tests_executed:
  TestT3Bridge_RunProviderConformance PASS (unit-a23a357eedd1739a53e0.log)
  TestCatalogBindsExecCompositionToSeamBackedContract PASS (unit-207dd82f52a3d3f9c9c3.log)
  TestCatalogReturnsIndependentEntries PASS (unit-207dd82f52a3d3f9c9c3.log)
  TestValidateProofRefsChecksContractRunnerOptions PASS (unit-207dd82f52a3d3f9c9c3.log)
  TestValidateRequiresOptionsProofToUseOptionsRunner PASS (unit-207dd82f52a3d3f9c9c3.log)
GATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name
```

| Phase | Results | Remote | Cached local / remote / retries | Verdict |
|---|---:|---:|---|---|
| unit | 389 | 389 | 0 / 0 / 0 | PASS |
| acceptance | 24 | 24 | 0 / 0 / 0 | PASS |
| integration-packages | 86 | 86 | 0 / 0 / 0 | PASS |
| integration-full | 14 | 14 | 0 / 0 / 0 | PASS |
| integration-latency | 1 | 1 | 0 / 0 / 0 | PASS |

Every result has remote strategy, both cached flags false, attempt 1, a start inside its phase window and a terminal verdict. Each phase's .evidence.json records window_ok=true and no errors. Build/nogo and regeneration may use cache; no test-result cache supplied a PASS.

Actual runner: gate-detached-run.sh starts gate-criterion3.sh with --repo `/var/tmp/gc-merge-validate.ga-kklbiu2.zlmbcb7r` --base `0f4e1b316662bc3c0c0c423cbe99c66df1538c0d` --bead-json `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/bead.json` --owned `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/owned.txt` --run-dir `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence`; GC_GATE_REMOTE_ENABLED=1 was already mayor-enabled. The phase flags below come from this run's receipts:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

The unit phase runs `make check`. Acceptance runs `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`; integration packages run `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`. Full integration runs that config on `//test/integration:integration_test` with `--test_arg=-test.v` and `--test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$`. The latency phase selects only those names with `--test_filter`, disabled sharding and gate-farm-probe.sh as --run_under (probe hash and command parameters retained in latency_leg).

latency_leg: complete, first attempt, both budget tests PASS, none executed in the main leg. Worker probe cleared after 30 s at load1=49.74 (threshold 50); observed test load1=37.06. A busy/deferred lane was never credited as PASS.

ICU74 check: OK, recorded host libraries (icu74.txt). Runtime setup ran through remote admission and isolation wrapper before the suite: rootless socket /_ping OK, native Dolt 2.6.0 and cached dolt-sql-server:2.6.0 matched deps.env; Ryuk disabled with the paired unreaped-container opt-out. The wrapper is PASS-THROUGH for this rig; actual remote execution and fresh evidence are proven by BEP, and native selectors are scrubbed by the floor runner.

### Owned tests

Source and canonical-merge ownership agree exactly (owned.txt, owned-merged.txt, owned-explain.txt):

- `TestT3Bridge_RunProviderConformance`: PASS
- `TestCatalogBindsExecCompositionToSeamBackedContract`: PASS
- `TestCatalogReturnsIndependentEntries`: PASS
- `TestValidateProofRefsChecksContractRunnerOptions`: PASS
- `TestValidateRequiresOptionsProofToUseOptionsRunner`: PASS

### Native floor

All 16 required steps completed rc=0. Local floor execution is accounted separately from the BEP-derived remote mode; it is not an execution fallback or a stitched local replacement suite.

load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0; run_start_load: 14.65; run_max_load: 26.63; run_mean_load: 20.89; run_readings: 8.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.65 run_max_load=26.63 run_mean_load=20.89 run_readings=8 wait_first_load=14.65 wait_max_load=14.65 wait_mean_load=14.65 wait_readings=1 read_errors=0
```

- `go test -tags integration -count=1 -v -run ^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$ ./cmd/gc`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-0.log`.
- `go test -tags integration -count=1 -v -run ^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$ ./examples/gastown`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-1.log`.
- `go test -tags integration -count=1 -v -run ^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$ ./internal/beads`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-2.log`.
- `go test -tags integration -count=1 -v -run ^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$ ./internal/beads/beadstest`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-3.log`.
- `go test -tags integration -count=1 -v -run ^(TestMigrateJournalFileMatchesPinnedBeads)$ ./internal/beads/contract`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-4.log`.
- `go test -tags integration -count=1 -v -run ^(TestBrProviderConformance)$ ./internal/beads/exec`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-5.log`.
- `go test -tags integration -count=1 -v -run ^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$ ./internal/doctor`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-6.log`.
- `go test -tags integration -count=1 -v -run ^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$ ./internal/mail/exec`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-7.log`.
- `go test -tags integration -count=1 -v -run ^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$ ./internal/runtime/herdr`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-8.log`.
- `go test -tags integration -count=1 -v -run ^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$ ./internal/runtime/runtimetest`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-9.log`.
- `go test -tags integration -count=1 -v -run ^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$ ./internal/runtime/tmux`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-10.log`.
- `go test -tags integration -count=1 -v -run ^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$ ./test/acceptance/helpers`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-11.log`.
- `go test -tags integration -count=1 -v -run ^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$ ./test/integration`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-12.log`.
- `make test-fsys-darwin-compile`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-13.log`.
- `bash scripts/test-push-gate-lock.sh`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-14.log`.
- `bash scripts/test-local-concurrency.sh`: rc=0; log `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/c3/evidence/remote/floor-logs/floor-15.log`.

## Skip integrity

The remote inventory records 472 SKIP events and the native inventory 39; these include subtests and repeated configurations. All 511 events were mapped to 281 source locations, with zero edited lines and zero unresolved locations; 264 sites are UNCHANGED and 17 are TOUCHED only at function scope. Unit, integration and acceptance_a import closures were computed for every affected test binary. `skip-sites.txt`, `skip-site-rows.json`, `final-skip-reach.json` and raw import graphs retain the per-event proof.

The full `--explain` blocks name all shared reach: one new-file shared hunk in t3bridge/conformance_test.go, five stateful-fake hunks in t3bridge/provider_test.go, one comment-only file-level hunk in providerledger/waiver_stagger_test.go, and RunLifecycleTestsWithOptions in runtimetest/conformance.go. The first two support only the new conformance fixture; the existing delivery start-grace SKIP occurs before provider construction. The shared runner's first-Start classifiers and fake-provider negative fixtures skip before the new reconnect branch; default false is unchanged. No skipped test imports providerledger, whose targets have no skips.

Independent BASE comparisons at `0f4e1b316662bc3c0c0c423cbe99c66df1538c0d` prove all 353 remote skip events in the reached packages pre-existing, with unchanged reasons, no new/changed-reason/owned/unexplained skip: seven unit targets and six integration targets. Comparisons are package-separated to avoid same-name matches across packages; all control targets executed uncached remotely and passed. `base-skip-proof/plan.json`, phase BEPs/predicates, raw CAS logs and *-compare.txt retain every selected name. The original seven-target unit run was reused after a text-reader refusal: raw result lines carried byte 0x16, so comparison copies remove only that byte, with normalization receipts. No test failure was retried into PASS.

Native reached-package skips were separately reproduced with the exact native-floor commands and scrubbed environment on BASE:

- `cmd/gc`: exact native-floor command, rc=0, all selected skips matched; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/native-base-proof/cmd_gc-compare.txt`.
- `internal/runtime/herdr`: exact native-floor command, rc=0, all selected skips matched; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/native-base-proof/internal_runtime_herdr-compare.txt`.
- `internal/runtime/runtimetest`: exact native-floor command, rc=0, all selected skips matched; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/native-base-proof/internal_runtime_runtimetest-compare.txt`.
- `internal/runtime/tmux`: exact native-floor command, rc=0, all selected skips matched; `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p/native-base-proof/internal_runtime_tmux-compare.txt`.

Other skips are unchanged platform/tool/infrastructure guards outside the changed helper closures: managed-process tests deferred by unit configuration, absent optional live providers or tmux context, unavailable PostgreSQL DSN, unavailable br, legacy pre-journal binary, and deliberate empty-fixture/negative-harness cases. Their printed reasons, unchanged source sites and computed lack of changed-hunk reach are retained in the inventories. No owned test or owned subtest skipped; no waiver was used.

Native BASE load evidence:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=62.39 run_max_load=74.83 run_mean_load=68.55 run_readings=4 wait_first_load=30.12 wait_max_load=79.52 wait_mean_load=50.00 wait_readings=61 read_errors=0
```

## Required CI job accounting

ci_lane_run: n/a (no CI-config change).
ci_required_closure: ci_required_triggers.py `/var/tmp/gc-merge-validate.ga-kklbiu2.zlmbcb7r` `0f4e1b316662bc3c0c0c423cbe99c66df1538c0d` `fb80773de38b2d8ad575185caa104c0845be90c1` fork; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; four LEAF jobs.

ci_required_accounting:

| Leaf job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Exact closure command rc=0, ci/closure.txt; eleven changed files. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=ci/runner-policy.out python3 .github/workflows/scripts/runner_policy.py, rc=0; ci/runner-policy.log. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | TRIGGERED by providerledger paths. windows_reach.sh rc=0 lists the five files below. Both required GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider and go vet -tags=integration ./internal/credentialprovider passed, rc=0, in a fresh isolated merged-tree view; ci/windows-vet-plain.log and ci/windows-vet-integration.log. These prove build/vet, not Windows test execution. |
| pack-gate | NOT-TRIGGERED | Exact closure-tool row: packs filter did not match. |

The check aggregator receives no independent status. The read-only accounting commands ran sequentially in one fresh merged-tree view through the isolation wrapper. Formula 3e explicitly exempts the read-only closure and runner-policy scripts from the general load-wait recipe. Windows reach computes an import closure and executes no tests; its five-file output required the two separate Windows vet commands, run sequentially after a bounded load wait in another fresh merged-tree view. No local test job was deferred for load.

- windows_reach: the following files, exactly as ci/windows-reach.txt reports:

```text
internal/testutil/providerledger/BUILD.bazel
internal/testutil/providerledger/ledger.go
internal/testutil/providerledger/ledger_test.go
internal/testutil/providerledger/proof.go
internal/testutil/providerledger/waiver_stagger_test.go
```

Windows vet load evidence (windows-vet/units/gate.w1.stderr.log; command results in gate.w1.stdout.log):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=810 wait_timed_out=0 run_start_load=14.65 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=35.71 wait_max_load=40.92 wait_mean_load=29.00 wait_readings=28 read_errors=0
```

- deferred_to_pr_ci: credential-provider-windows (non-linux-runner).
- not_covered: none; host_gaps: none.
- Merge condition: CI / required must be green on the exact PR head, and the triggered deferred Windows job must succeed. This gate PASS does not replace PR CI or authorize a merge.

```text
n/a: read-only closure, runner-policy and Windows import-closure accounting; formula 3e explicitly runs read-only scripts as shown.
```

The first CI-accounting attempt was refused by the isolation wrapper's disk floor at 2026-11-01T06:14:20Z (39.4 GiB free, 40 GiB required); no CI command ran. Its captured bead note records the bounded load wait: threshold=15, waited_seconds=1801, wait_timed_out=1, run_start_load=67.22. After a successful disk precheck, the read-only accounting was restarted. Its unnecessary load wait was stopped before any accounting command ran, following formula 3e's explicit read-only exception; the final commands ran directly in a fresh isolated view. The completed full-suite evidence was retained unchanged.

Full command/status/log evidence remains under `/var/tmp/gc-deploy-ga-kklbiu2.8vjtiw6p`. The reviewed source is unchanged; only this release record is added on the isolated deploy branch.
