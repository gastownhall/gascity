# Recovery-hook read diagnostics and shell stderr release gate

**Verdict:** **PASS**

evaluated_at: 2026-11-09T21:13:07.910672+00:00
deploy_bead: ga-2ukghpb
build_bead: ga-lu59gf.1
review_bead: ga-qxnw6o7
reviewed_commit: 1652930d188a8acd711d875021de5704838d493e
deploy_mode: remote
push_remote: fork
merge_state: clean (origin/main@862ee37e4570338d5135f3fbc761bd6221de38d4)
gate_base_sha: 862ee37e4570338d5135f3fbc761bd6221de38d4
gate_base_kind: current-tip
merge_head: 846d658c3ec3e26bf720c730b946a3cfe6dba6de
tested_tree: d6c0c273aeb3f1954be5de56f455909a2a27f3fe
publication_base: b687bf421edecdc905f3043c921c775b76de4f5c
publication_merge_state: clean (read-only merge-tree; files: none)
The tested base above remains the evidence boundary; the newer publication base has merge-state proof only. Current merged-result CI is required before landing.
evidence_root: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld
city_evidence_commit: 3b3afd2760a1d826b7e7f8903f06a903a8a7a4fc

Criterion 6 ran first, then review/acceptance and all fast lanes, then the detached full suite. The reviewed commit remains the deployment source. The clean trial merge is test evidence only. No rebase was performed. `docs/PROJECT_MANIFEST.md` is absent; release criteria come from the deployed release-gate-criteria protocol and repository TESTING.md/Makefile.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Live review ga-qxnw6o7 is closed PASS, metadata.commit equals the resolved reviewed source. No carryover or reviewer-added commit. |
| 2 | PASS | R1/T1–T3 and R2 acceptance mapping below; all three diff-owned tests PASS by name. Required live-store probe PASS. |
| 3 | PASS | Five fresh remote phases, 16 local-floor steps, all three diff-owned tests, 498 remote and 39 native SKIP justifications, and all six required-CI leaf accounts verified. Two Windows runtime jobs are deferred to PR CI. |
| 4 | PASS | Review has no unresolved HIGH findings; minor stderr/fd inheritance risks are accepted in the scope ruling. |
| 5 | PASS | Reviewed source and test scratch are clean. The isolated deploy branch adds only this gate record; publication checks require empty post-commit porcelain and no other delta from the reviewed commit. Final check is recorded on the bead. |
| 6 | PASS | Clean merge onto the pinned base; complete bazel build //... and make vet/nogo both terminal SUCCESS. |
| 7 | PASS | One failure-observability theme, explicitly bundled by architect ga-4yries8. 17 files: two production files, three test files and 12 recovery-hook goldens. |

## Acceptance evidence

| Contract | Verification |
|---|---|
| R1.1–R1.2 | TestDefaultRecoveryHooksReadFailures checks one hook/read/exit marker for each nonzero read. No stderr-signature matching. |
| R1.3–R1.8; T1 a–e | Eight generated-hook subtests cover continued independent reads, all reads failing, discarded partial rows, marker bypass of release IDs, healthy stderr warnings, zero hook exit and one call per read. |
| R1.9–R1.11; T2–T3 | No new execution site, retries, temp files or env knobs; comment updated; shape assertion, parity oracle and 12 goldens match. Existing internal/config/config_test.go lifecycle tests and resource census baselines are untouched. |
| R2.1–R2.6 | TestShellCommandSurfacesStderrOnFailure has five subtests: stderr/exit 3, trim, empty stderr, whitespace-only stderr and success. errors.As preserves ExitError and ExitCode; source inspection confirms process cleanup/timeouts unchanged. |
| Live-store probe | Exact tested generator, non-owning identity gateprobe-f0fb2bcf4d674ecba67fdd40cbd13af9; both hooks rc0, empty stdout/stderr, no marker, five real bd reads, zero mutation attempts. |
live_acceptance_evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/live-hook-result.json

## Full-suite test evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh start --run-dir /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run --key d6c0c273aeb3f1954be5de56f455909a2a27f3fe -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2ukghpb DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/merge --base 862ee37e4570338d5135f3fbc761bd6221de38d4 --bead-json /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ga-2ukghpb.json --owned /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/owned.tsv --run-dir /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence
test_cmd_scope: full-suite
test_counts: {"FAIL": 0, "PASS": 149751, "SKIP": 512}
Counts are top-level result events summed over 550 retained logs, including repeated jobs; subtests are not counted. The SKIP inventories below include subtests and use a different population.
test_log_dir: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 408, "results": 408, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 71.06284713745117, "waited_seconds": 0.001}
farm_test_seconds: 13914.867
owned_topup: []
floor_topup: 16/16 checks rc0; details in c3-run/evidence/remote/floor-topup.json
latency_leg: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
policy_attribution: none

```text
diff_tests_executed:
  TestShellCommandSurfacesStderrOnFailure PASS (integration-packages-568812e4d525bdd1f974.log, unit-493754ea2d5b4d8858c8.log)
  TestDefaultRecoveryHooksReadFailures PASS (unit-4bbf1267e0c4ccc54042.log)
  TestDefaultRecoveryHooksSurfaceFailures PASS (unit-4bbf1267e0c4ccc54042.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
```

All 534 phase results were fresh remote execution, attempt 1, uncached, within phase windows and with terminal PASSED verdicts. Independent audit re-hashed 1068 CAS artifacts and verified 534 normalized log copies. Three local self-checks are compile/runner checks, not runtime test credit.

`test_cmd` phase details:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

runtime-preflight.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=28.12 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=28.12 wait_max_load=28.12 wait_mean_load=28.12 wait_readings=1 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=26.30 run_max_load=36.13 run_mean_load=19.17 run_readings=80 wait_first_load=26.30 wait_max_load=26.30 wait_mean_load=26.30 wait_readings=1 read_errors=0
ci-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=38.70 run_max_load=39.95 run_mean_load=37.41 run_readings=7 wait_first_load=15.01 wait_max_load=86.92 wait_mean_load=60.87 wait_readings=61 read_errors=0

Local floor native-test load: threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.75 run_max_load=14.72 run_mean_load=14.10 run_readings=8. Remote admission has no host-heavy load wait.

Local floor commands (environment-absence top-ups supplement the complete remote suite):

- cmd/gc: `go test -tags integration -count=1 -v -run '^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-0.log`.
- examples/gastown: `go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-1.log`.
- internal/beads: `go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-2.log`.
- internal/beads/beadstest: `go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-3.log`.
- internal/beads/contract: `go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-4.log`.
- internal/beads/exec: `go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-5.log`.
- internal/doctor: `go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-6.log`.
- internal/mail/exec: `go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-7.log`.
- internal/runtime/herdr: `go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-8.log`.
- internal/runtime/runtimetest: `go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-9.log`.
- internal/runtime/tmux: `go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-10.log`.
- test/acceptance/helpers: `go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-11.log`.
- test/integration: `go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-12.log`.
- fsys-darwin-compile: `make test-fsys-darwin-compile`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-13.log`.
- push-gate-lock-selftest: `bash scripts/test-push-gate-lock.sh`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-14.log`.
- local-concurrency-selftest: `bash scripts/test-local-concurrency.sh`; rc=0; log `/var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c3-run/evidence/remote/floor-logs/floor-15.log`.

## Fast lanes and build evidence

policy_lane: PASS: fast-lane.sh policy — fmt-check-changed, make lint/nogo, release/routing/split/residency/hooks guards and 13 policy targets
drift_lane: PASS: fresh-tree make bazel-sync then git diff --exit-code and empty complete porcelain status; seven generated-artifact targets; separate pinned-base OpenAPI check
merge_validation: Gate tests the clean trial merge; merge authority rechecks current merge state and exact-head CI/clearance before landing. Any changed head requires re-gating. Merge-time policy: packs/actual/deployer/docs/merge-time-guarantee.md.
evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/c6-verified.json sha256=900693b10d1ba79bd4d8a08a7115b7bcf064a690d19c39954b7e04a309af45cd
evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/fast-verified.json sha256=6184baf3c9b00afcce58571f2b5fccc455a707b290d6f8cd8cfa8d0ab98245a1
evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/resume-finalprep-audit.json sha256=9c2796901850bcc6eefbfbd61bb6ecaab8f1022ee7b9dd11956a53d0ea47bc4b
evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/resume-c3-audit.json sha256=abf402c1bfe8e9ae34a425db69800b796e88557870797055d33c8181aeb5e124
evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/remote-skip-final-verified.json sha256=eb2009501a8853ca0da464f8ee3d33ff4f5820b545e16a8d63a270d3788a0592

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/merge 862ee37e4570338d5135f3fbc761bd6221de38d4 1652930d188a8acd711d875021de5704838d493e fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status
```text
diff 862ee37e4570338d5135f3fbc761bd6221de38d4...1652930d188a8acd711d875021de5704838d493e: 17 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               MATCH internal/config/testdata/workquery/legacy_OnBoot_bd104.golden, internal/config/testdata/workquery/legacy_OnBoot_bd105.golden, internal/config/testdata/workquery/legacy_OnDeath_bd104.golden
suite_mode: full  (shared filter matched)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting: changes | LOCAL-PASS | /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/closure-verified.json
ci_required_accounting: runner-policy | LOCAL-PASS | /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/runner-policy-result.json
ci_required_accounting: windows-lane | LOCAL-PASS | /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/windows-lane-verified.json
ci_required_accounting: credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/windows-reach.json
ci_required_accounting: windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/windows-native-reach.json
ci_required_accounting: pack-gate | LOCAL-PASS | exact YAML command groups and step environment: scripts/update-bundled-gastown-pack --check; GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live; both rc0. Commands, log hashes and fresh-tree identity: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/ci-required/pack-final-verified.json
windows_reach: credential-provider-windows=none; windows-lane-test=none, independently computed from its actual Windows test allowlist
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
Windows-only runtime jobs stay deferred to PR CI. The windows-lane LOCAL-PASS covers its exact Windows/Darwin compile commands and nogo working directory, not runtime execution. The PR may merge only once CI / required is green at the gated head and each deferred job is success or legitimately path-skipped.


## SKIP justification

skip_inventory: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/skip-audit-inventory.json (498 remote events); /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/floor-skip-inventory.json (39 native events including subtests)
skip_justification: REMOTE COMPLETE: 396 same-name/same-reason matches at pinned base, 102 events with no changed shared/helper input in their actual tagged import closures; all 290 reported sites unchanged. NATIVE COMPLETE: all 39 events match the same name and reason at the pinned base using the identical local-floor commands in 6 packages. No diff-owned SKIP or FAIL. Full comparison evidence: /var/tmp/deploy-ga-2ukghpb.dm0iu_ld/skip-audit-final.json.
None is a diff-owned test. Reason/location plus actual tag-specific import closures were examined; unchanged lines alone were never used to excuse shared/helper reach. Source ownership and locations: owned-explain.txt, skip-sites.txt and skip-audit-prepared.json. Native skips repeat already-audited unchanged source sites and have explicit base comparisons.

| Package | Changed shared/helper packages reached |
|---|---|
| cmd/gc | cmd/gc, internal/config |
| examples/gastown | internal/config |
| internal/api | internal/config |
| internal/beads | internal/config |
| internal/beads/beadstest | none |
| internal/beads/contract | none |
| internal/beads/exec | none |
| internal/beads/splittest | internal/config |
| internal/config | internal/config |
| internal/connect/tokenrender | internal/config |
| internal/convergence | none |
| internal/dispatch | internal/config |
| internal/doctor | internal/config |
| internal/formula | internal/config |
| internal/fsys | none |
| internal/gitcred | none |
| internal/mail/exec | none |
| internal/molecule | none |
| internal/orders | none |
| internal/pidutil | none |
| internal/processgroup | none |
| internal/runproj | none |
| internal/runtime | none |
| internal/runtime/exec | none |
| internal/runtime/herdr | none |
| internal/runtime/runtimetest | none |
| internal/runtime/ssh | none |
| internal/runtime/t3bridge | none |
| internal/runtime/tmux | internal/config |
| internal/sessionlog | none |
| internal/worker/transcript | none |
| internal/worker/workertest | internal/config |
| internal/workspacesvc | internal/config |
| test/acceptance | internal/config |
| test/acceptance/helpers | internal/config |
| test/containerhost | none |
| test/conversationlane | internal/config |
| test/integration | internal/config |
| test/tmuxtest | none |

## Scope ruling ga-4yries8

Architect ruling 2026-10-27: replace #3901 with R1 recovery-hook read-failure markers and R2 shell-command stderr. The work-query guard is superseded by #7944. The startup doctor probe adds database-opening overhead without blocking an unprimed session; the string-based doctor check duplicates typed verdicts and bypasses pinned-binary/store boundaries. None of those changes or their census/bootstrap exemptions were ported. R1 and R2 are one failure-observability theme and have separate implementation commits.

Accepted residuals: non-proxied schema skew and exit-zero warnings remain outside this change; SessionStart skew reporting is unchanged. Markers carry exit codes without read stderr; jq parse failures get no marker; no retry occurs until a later hook invocation. Custom hook overrides pass through unchanged. Partial failed-read rows are dropped, so work can remain in progress until a later successful sweep. fd3 inheritance can retain the existing stdout pipe until WaitDelay; failed-command stderr may reach logs.

The PR body says Supersedes #3901 and quotes RED/GREEN assertion evidence. Mayor mail gcg-4694017 confirms #3901 was closed as superseded by quad341 on 2026-10-28T15:17:04Z; its replacement cross-link remains required. No GitHub issue is linked for closure.


## Additional load and CI evidence

pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=45.57 run_max_load=43.07 run_mean_load=34.29 run_readings=10 wait_first_load=84.36 wait_max_load=90.05 wait_mean_load=75.11 wait_readings=61 read_errors=0

native skip-base comparison: LOAD_GATE_SUMMARY threshold=15 waited_seconds=270 wait_timed_out=0 run_start_load=14.14 run_max_load=16.17 run_mean_load=15.14 run_readings=3 wait_first_load=24.59 wait_max_load=24.59 wait_mean_load=19.05 wait_readings=10 read_errors=0

host_gaps: none

CI jobs with native Go commands ran exactly as the checked-in workflow specifies, under the gascity criterion-3e ruling. They are separate job evidence and do not replace the Bazel suite.

The isolation wrapper reports PASS-THROUGH for gascity scratch views; nothing is cloned or stripped by that wrapper. The retained remote test actions use their declared Bazel environment; local job/floor drivers scrub fleet variables and retain the pinned test bd. The fresh-tree records identify each clean command view.

The 512 top-level SKIP events in the suite tally and the detailed inventories have different populations: the latter contain 498 remote events plus 39 native events, including subtests. Every inventory event is justified; no SKIP is silently counted as a PASS.

Initial/r2 baseline queues failed setup before any test or CI job ran (directory permissions, then a scratch driver status-contract check). Their records are retained. Corrected r3 ran only the unexecuted baselines/jobs; no full suite was relaunched and no product failure was excused.

## Retained artifact manifest

Full inventories and per-name comparison details remain outside the PR. The protected evidence root retains all suite/CAS/native logs; it must not be reaped as gate cleanup.

| Artifact (under evidence_root) | Bytes | Lines | SHA256 |
|---|---:|---:|---|
| `context.json` | 932 | 22 | `79054f21d4700be82afbb727572be1dccf4f4f76a0ba37db502f4e18db6c1931` |
| `current-retained-audit.json` | 979 | 21 | `4e876e69def3427ad274cac703f20e9277a278c3ded79fde3850912d4a639760` |
| `publication-merge-state.json` | 368 | 9 | `cd0156ce498abeb45d8a12e08aef0ce93069fba92e75b932f228c8e25bd5226e` |
| `acceptance-static.json` | 1749 | 30 | `bcf5e0faea82c42e70148adb8341f7b3ad9fbdd680c5988c29a818422ff6fe48` |
| `resume-c3-audit.json` | 4612 | 122 | `abf402c1bfe8e9ae34a425db69800b796e88557870797055d33c8181aeb5e124` |
| `raw-bep-final-audit.json` | 3444 | 88 | `0701189ad68d27ecafe4e123d14edb2855636705ed492606ede1068cdb403409` |
| `retained-evidence-manifest.json` | 478870 | 12142 | `4a3f74eab3541633b5095163ad8afe43d4a6ada8cd4385092b417a422f2b21fa` |
| `c3-by-name-verified.txt` | 1014 | 9 | `09ca9da8f0da5d8e3cffcf951997484a3f624e56664406261aa5f6933a9da687` |
| `owned.tsv` | 156 | 3 | `8fed5ceca8f3e49ed096aa76ebb9bfdf02b677c00a366b2454a2c467978a39ba` |
| `owned-explain.txt` | 12339 | 113 | `9aa2fe5b5c793d741b3866225dbce972ab94e491e16712e2348490497778c306` |
| `skip-audit-inventory.json` | 197928 | 3986 | `a976f366d76ce11e80cd1036b4e57b233ca88287a867f3b283971fb840aab18d` |
| `floor-skip-inventory.json` | 14902 | 314 | `0422a47d76b20f90b4743baada3348e70372952c19d07d9ac85bb6d989fa8bac` |
| `skip-sites.txt` | 56986 | 290 | `a109059bff7c95fd4ba6a951d3b9288c55cd7d4effa33964bad9792acf558158` |
| `skip-audit-prepared.json` | 164796 | 5562 | `132d876c219a717c3fce1ce253f1521e5692525c0a3db0c6c6164d84aea9c29a` |
| `remote-skip-final-verified.json` | 1284 | 40 | `eb2009501a8853ca0da464f8ee3d33ff4f5820b545e16a8d63a270d3788a0592` |
| `skip-audit-final.json` | 6446 | 208 | `82539f5e3b49dda93790c5139ad9fc41edd2cde9ae1fc43167896feca118d5a0` |
| `native-skip-base-evidence-r3/results.json` | 3217 | 104 | `12ed28887a331b92cbbd4a4e7fb4db643e916221da6d1aeb0bda745440d400ae` |
| `native-skip-base-evidence-r3/result.json` | 426 | 16 | `ecc1f45c956f1772dbb46528850bce6f83c2ac88b329d1df16aea120233f84ed` |
| `live-hook-result.json` | 1400 | 63 | `695aee9ec0e2ae356cd7184c7697a900543b2426f7e4854d15e16142202a9ebd` |
| `live-bd-calls.jsonl` | 567 | 5 | `b23a5a16b038dd96711e76af87342acd6792646741f52fce501ba45c9c96dd9a` |
| `ci-required/accounting-final.json` | 1555 | 48 | `3faacc9605a699d7030f403aa754ee4741127e4f4a337c4e823595d18faab1f5` |
| `ci-required/pack-final-verified.json` | 2094 | 47 | `a975ae082d52a2260ac62d8df7dfc8cc6df082ad2e0787bb840e9ec4f629284e` |
| `ci-required/windows-lane-verified.json` | 1035 | 17 | `97ecbad914f1306e58e255ee9726492db2c59b47c4eda5670f4148fb998d7ce2` |
| `ci-required/windows-reach.json` | 433 | 11 | `ab8b7203744662a0ade74a7c7442134099340b2f8cf3bdbd6e7d4f59655c279c` |
| `ci-required/windows-native-reach.json` | 1654 | 38 | `ea94a5177d80cf9b47faefc015dc409c3008d0dbfa9b4375813d3ec5bce4158f` |
| `ci-required/runner-policy-result.json` | 325 | 15 | `ba69b4dc16d92750f29acee50746106ffe1cfc7071de2265141942fc8acf8afc` |
| `ci-closure-final.txt` | 1448 | 17 | `59899488434ad34fc9cd963043e0eb16a5218a89c10a8dc2a907f26b327e63a8` |
| `finalize-gate-record.py` | 8874 | 101 | `a29f70081ad34d28e4d98dd1410660ca2ea8c392c6b4fc989de3e5c98e37a4ed` |
