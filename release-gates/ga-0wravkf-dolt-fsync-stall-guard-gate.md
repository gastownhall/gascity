# Dolt fsync-stall recovery guard — release gate

**Verdict:** **PASS**

Deploy bead: `ga-0wravkf`; build: `ga-yrodto`; review: `ga-jbflyyl` PASS with independently recomputed mechanical-conflict carryover. Single-bead successor of the unmerged PR #7975; no merge or binary deployment is performed by this gate.

- Reviewed source: `eaca4ed1173eed0fc6536af0f93f1697fa84f496` (merged head, carrying `988eba796d0a0d93825c119cd4c2bb8fecf5e048`).
- Frozen main/base: `ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083`.
- Canonical merge head: `eaca4ed1173eed0fc6536af0f93f1697fa84f496`; tree: `60928ee85fe3bb2dbdfd556314f8e7f226abdae8`.
- Evidence: `/var/tmp/deploy-ga-0wravkf-regate.p4wf7hap` (original gate run); `/var/tmp/pr8290-evidence` (re-run on the merged head).
- merge_state: clean (origin/main@ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083; files: none).
- gate_base_kind: current-tip.
- gate_base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083.
- `988eba7` moved directory validation from the caller (`probe`) into the worker (`Measure`); the original gate evidence below was gathered before that change, and the owned tests were re-run on the merged head.
- merge_validation: canonical merge built/vetted here and tree identity independently checked against read-only merge-tree.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-jbflyyl full review passed; carryover from469d05da6847394063f71eec2b0d5d249a6b902b to reviewed source recomputed by review-carryover.sh: mechanical-conflict patch-id094de69dccaf1b6d1be1d276b0d0dd5f41721edf files=cmd/gc/BUILD.bazel,examples/bd/dolt/BUILD.bazel. Full SHAs resolved against git. |
| 2 | Acceptance | PASS | AC1–11 source audit,35 owned tests, historical RED proof, caller/stop map and current GREEN in the prepared PR body. Published-body equality is required after opening and before clearance. Details below; limitations remain explicit. |
| 3 | Tests | PASS | Five fresh uncached remote phases,498 strict first-attempt results,16 mandatory native/fixed steps exit0,35/35 owned names PASS. Zero failures, all skips inventoried and sites unchanged. |
| 3b | Policy/generated drift | PASS | Isolated fresh views: format/pinned lint/nogo, all shell/policy checks, BUILD regeneration plus empty tracked/untracked diff, seven generated-artifact targets. Static absolute complexity baseline failure is attributed below; no raw PASS is claimed. |
| 3c | CI-config lane | PASS | No workflow/job/matrix/timeout/required-check change. ci_lane_run: n/a (no CI-config change in this diff). |
| 4 | Review findings | PASS | No unresolved HIGH/blocker/major. Accepted minor boundary observations retained in acceptance audit/PR text. |
| 5 | Clean source | PASS | Canonical scratch tracked/untracked clean after normal commit hook. Final isolated branch must contain only reviewed source plus this gate record; clean check repeated before push. |
| 6 | Merge build/vet | PASS | Original C6_PASS, full all-target build, integration binary compilation, full vet/nogo, check-hooks and normal scratch commit hook; expected/actual tree identity independently verified. |
| 7 | One feature theme | PASS |18 source paths implement fsync measurement and managed-recovery/restart guards. Stack-aware denylist/citation guard must pass before cutting; accepted ids ga-0wravkf,ga-yrodto,ga-3ayztoc were opened and confirmed same lineage. No stack found at gate intake. |

## Acceptance and publication contract

- AC1: round1 RED was uncommitted compile/restart-test failures, accepted by review because the commit hook compiles staged code. Original round1 log expired; no committed round1 RED is claimed. Retained committed round2 RED was independently replayed; four actual FAIL lines and their current candidate unit PASS lines are quoted in pr-body.md. Historical RED is supplementary focused evidence and excluded from current suite counts.
- AC2–3: hidden fsync-probe command, TSV outcomes/exit codes and timeout validation; unique O_EXCL0600 regular root dotfile,4KiB write/fsync/cleanup; detached worker/null stdio/private result fd and parent deadline. Error status includes error text; fsync_ms is defined for finished/unfinished. The never-answering in-tree bound test uses the start seam; real command success is covered. Process creation itself can block. No real storage stall was induced.
- AC4–7: ENOSPC precedence and unchanged ENOSPC tests; TCP-listening automatic-recovery guard before recover-managed or shell fallback; finished>=WARN/unfinished refuses; fast result proceeds; missing/failed/malformed helper emits visible fail-open warning. WARN1000/ERROR5000 validation tested. ERROR is parsed for the sibling doctor work and has no consumer here. Restart checks even a non-listening server and --force explicitly bypasses refusal after measuring.
- AC8: pr-body.md enumerates health/managed-bd-retry/explicit recover, recover-managed/Go stop, op_recover shell fallback, restart, explicit stop/shutdown, gc stop/supervisor/migration, suspension, startup ownership repair and provider-owned exempt paths. It names internal/fsyncprobe and states plainly that the guard has not been observed firing against a real stalled city filesystem. Final published-body verification is required before clearance.
- AC9–10: dotfile invisibility to journal/data-dir listings, embedded asset declaration and materialized helper all pass by name. BUILD generation and artifact drift are clean.
- AC11: build/full vet/nogo/full suites, sh -n and policy checks pass. Complexity is scored with the documented static baseline mirror/zero-growth proof below, not relabeled as an absolute raw success.

## Complete test evidence

`test_cmd`: gate-criterion3.sh against the canonical merge, detached systemd service, canonical admission/load/isolation wrappers. The five phases use --nocache_test_results and --noremote_local_fallback with remote execution and verbose test output; exact command strings retained in c3/evidence/execution.json and each phase stdout. An explicit complete local fallback was supplied and not used.

`test_cmd_scope: full-suite`; `test_exec_mode: remote-exec`; `heavy_mode: none`; `waiver_ref: none`; runtime failure_attribution: none.

| Phase | Actual target summary | Fresh remote first-attempt results |
|---|---|---:|
| unit | Executed 288 out of 288 tests: 288 tests pass. | 374 |
| acceptance | Executed 7 out of 7 tests: 7 tests pass. | 24 |
| integration-packages | Executed 36 out of 36 tests: 36 tests pass. | 85 |
| integration-full | Executed 1 out of 1 test: 1 test passes. | 14 |
| integration-latency | Executed 1 out of 1 test: 1 test passes. | 1 |

All498 results have remote strategy, both cache flags absent/false, attempt1, start inside their phase window and PASSED terminal status; independently re-read from BEP. Latency is excluded from phase4 and measured in phase5: both canonical names PASS, first attempt, probe clear after10s at load1=46.44/32CPUs (threshold50); no deferral/retry. `farm_test_seconds: 9416.173`.

Mandatory floor:16/16 steps exit0, including all selected environment-absence packages, beadstest pinned-module check, Darwin fsys compile and both runner self-tests. Native commands/retained logs/remaining non-owned SKIPs are in floor-topup.json. `owned_topup: []` (no missing owned name); `floor_topup: complete`; `fallback_reason: null`.

```text
test_counts: PASS=126575 FAIL=0 SKIP=370
test_log_dir: /var/tmp/deploy-ga-0wravkf-regate.p4wf7hap/c3/evidence/remote/logs
diff_tests_executed:
  TestDoltStateFsyncProbeErrorExitsNonZeroWithMessage PASS (integration-packages-830685c60d0c2dd88567.log, unit-27b23e708b477fce4dad.log)
  TestDoltStateFsyncProbeFinishedPrintsStatusAndMeasuredMS PASS (integration-packages-18745ddf7d62465654f5.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestDoltStateFsyncProbeIsHidden PASS (integration-packages-12697e2ea0d32c278115.log, unit-fd986cf5bab8709b8d45.log)
  TestDoltStateFsyncProbeRealChain PASS (integration-packages-e5330b8c981927473156.log, unit-1ab526405ace21879f85.log)
  TestDoltStateFsyncProbeRejectsNonPositiveTimeout PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-d9e667428f972d067bd3.log)
  TestDoltStateFsyncProbeRequiresDirAndTimeout PASS (integration-packages-b72a67a2f9444af705e3.log, unit-637c74c578a2b17fc654.log)
  TestDoltStateFsyncProbeUnfinishedExitsZeroWithBound PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-3d6d3036a2f5902472b5.log)
  TestDoltStateFsyncProbeUsesTheRealProbe PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-423c6a0da1986b90e9a7.log)
  TestFsyncStallErrorThresholdFromEnv PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-5a9df316125cd0aeff2e.log)
  TestGcBeadsBdRecoverENOSPCWinsOverFsyncStall PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-10cc7795c0ec199a5530.log)
  TestGcBeadsBdRecoverFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-912ab643330b35bd0b6b.log)
  TestGcBeadsBdRecoverFsyncGuardFailsOpenWithOneStderrLine PASS (integration-packages-821a75e4a503601af052.log, unit-f93a0ca96ccc61d0cae9.log)
  TestGcBeadsBdRecoverFsyncWarnThresholdFromEnv PASS (integration-packages-75af79934639abaae69c.log, unit-cd6930bb0d0da8646500.log)
  TestGcBeadsBdRecoverNeedsNoProbeWhenDoltNotListening PASS (integration-packages-074fc6c34a1fb2577127.log, unit-088cc38d26d094729229.log)
  TestGcBeadsBdRecoverProceedsWhenFsyncFast PASS (integration-packages-e4245ddf6a37030111bc.log, unit-abd97d2cb9b450225d57.log)
  TestGcBeadsBdRecoverSkipsWhenFsyncFinishedAtOrAboveWarn PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestGcBeadsBdRecoverSkipsWhenFsyncProbeUnfinished PASS (integration-packages-9326181a736eb30b71ca.log, unit-752a0bf25b15b465517a.log)
  TestMaterializedBdPackCarriesFsyncStallHelper PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f679cd2551fcc64525aa.log)
  TestDotfileProbeFileIsInvisibleToDataDirListings PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-6b47d0615f42f4ae2b8d.log)
  TestRestartENOSPCRefusalStillWinsOverFsyncStall PASS (integration-packages-a9e12e81511731ef69dd.log, unit-da81bca7c80446c5af28.log)
  TestRestartFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-ff6413fa61a58d5e02ec.log)
  TestRestartFsyncGuardFailsOpenWithNote PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-89d44f035a0dc6a9b6c9.log)
  TestRestartProceedsWhenFsyncFast PASS (integration-packages-2c974992a3e988636c94.log, unit-9d67ff2f074bc8f49779.log)
  TestRestartRefusesFsyncStallUnlessForced PASS (integration-packages-130aa78402132a9cb052.log, unit-98fce3c243b1e6971368.log)
  TestSumJournalFilesIgnoresFsyncProbeDotfile PASS (unit-7bce503b7c011c7475f3.log)
  TestMeasureErrorCases PASS (unit-d50e950eb1e22cc863ea.log)
  TestMeasureFinishedReportsMeasuredTimeAndRemovesFile PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeBoundHoldsForCaller PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeFinished PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeDoesNotTouchTargetDir PASS (/var/tmp/pr8290-evidence/owned-fsyncprobe-dolt-doctor.log)
  TestProbeRejectsUnknownWorkerStatus PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeSpawnFailureIsError PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeWorkerThatExitsWithoutAnswerIsError PASS (unit-d50e950eb1e22cc863ea.log)
  TestRunWorkerWritesJSONResult PASS (unit-d50e950eb1e22cc863ea.log)
  TestWorkerCommandIsDetached PASS (unit-d50e950eb1e22cc863ea.log)
```

Counts are top-level result lines per job, not distinct tests.395 remote/native skip events including subtests were checked at268 distinct source sites; every site UNCHANGED, no skip site shares a changed hunk/helper file. The five SHARED-HUNKS files in the candidate inventory contain this feature's helpers/imports/tests and none contains a skipped site; their scope does not change the existing provider/platform/opt-in skip decisions. No owned FAIL/SKIP/MISSING. Reasons and site mappings retained in skip-inventory.json, floor-skip-inventory.json, skip-sites-evidence.txt and skip-sites-summary.json; environment-absence packages received the mandatory native top-ups. Unsupported provider capabilities, platform-specific cases, opt-in replay/live tests and existing unconditional conformance deferrals remain non-owned and explicitly inventoried.

All996 raw log/XML artifacts and514 normalized logs, plus historical RED artifacts, SHA256-verified in retained-artifact-digests.json. Final-audit.json ties them to source/base/tree/phase windows. All logs/run directories are retained.

## Static complexity baseline attribution

policy_attribution: make complexity-check -> ga-q7p6is, criterion3b mirror rule, same-run pristine pinned base reproduction. Tracker opened, predates run; sighting1aebbb59-b70b-5684-87f7-001820a2b46c read-verified. The check script and baseline are unchanged; this static check excludes added test load.

Base command exits2; canonical merge command exits2. Both contain71 byte-identical rows (report SHA256bc843975eaeafbdb6ab6df5968f88b123d52601c383ea0340a4df242729a57a1). Pinned complexity-diff exits0 and prints `complexity: no threshold changes`, with no new/regressed line and no worse row. Raw outputs retained in complexity.json and complexity-*-stdout/stderr. No baseline edit, waiver or literal raw complexity-check PASS.

## Runtime, admission and load

Recorded ICU74 verdictOK, rootless podman socket active, pinned Dolt image checked. RBE worker pin/certificate/kill-switch state retained in rbe-state.json. Admission remote,25GiB floor,cap2,free71.84411239624023GiB,wait0.001s. Remote phase does not impose the host-heavy wait. The host observations below are separate from the remote worker's latency probe:

```text
remote host wrapper: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=99.34 run_max_load=109.72 run_mean_load=42.17 run_readings=103 wait_first_load=99.34 wait_max_load=99.34 wait_mean_load=99.34 wait_readings=1 read_errors=0
native floor host wrapper: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=25.79 run_max_load=32.68 run_mean_load=30.56 run_readings=9 wait_first_load=33.98 wait_max_load=44.30 wait_mean_load=31.98 wait_readings=61 read_errors=0
```

Native floor required fields: load_threshold15, load_waited_seconds1800, load_wait_timed_out1, run_start_load25.79, run_max_load32.68, run_mean_load30.56, run_readings9. The bounded ordinary wait timed out and proceeded by policy; all floor steps passed.

## Publication and retention

Prepared PR body carries exact RED/current GREEN, caller/stop inventory, force/env behavior and accepted limitations. Before publication: repeat ancestry guard, cut deploy/ga-0wravkf-gate at exact reviewed source, commit only this record with active hooks, verify clean tree and reviewed source. Push uses guarded fork ref, never builder provenance. Verify the opened PR body/head; post exact-head release-gate/deploy-clearance success before routing the peek-verified merge-request. Deployer does not merge.

Canonical scratch/Bazel cleanup independently verified before publication: exact temporary tree absent/unregistered, private output base removed, original services inactive/MainPID0 and all1528 retained artifact/log digests unchanged (cleanup-verified.json). This gate record and raw evidence remain retained; the old unmerged PR7975 is superseded by the new isolated publication. Merging the PR does not replace the running city binary.
