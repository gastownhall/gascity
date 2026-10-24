# Default-branch build probe release gate

**Verdict:** **PASS**

- Deploy bead: ga-xn6j7xr; build: ga-gqnrkh; review: ga-wsvapy9 (round 3 PASS).
- Reviewed source: `21bcbd27ad23fdd9f1c04162732c7339c5a47641`; implementation base: `4a1068a1da6d89890c7fa9ba130c73e0ca1eac42`.
- Deploy mode: remote; isolated branch: `deploy/ga-xn6j7xr-gate`; push remote: fork.
- Gate base: `origin/main@fec82025d24392176eef9153ebf7af8759c6ef0c`; gate merge: `23c81edc8e91f72ebb5bf0434b9c57ca92f25be4`.
- merge_state: clean (origin/main@fec82025d24392176eef9153ebf7af8759c6ef0c)
- gate_base_kind: current-tip
- merge_validation: clean scratch merge built and tested; MPR owns merge-time reconciliation and exact-head approval. No self-rebase or merge performed by deployer.
- Evidence retained: `/var/tmp/gate-ga-xn6j7xr-20261017` (marked against reaping).
- `docs/PROJECT_MANIFEST.md` is absent; commands come from TESTING.md, Makefile, CI and the current deploy protocol.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Merge state, build and vet | PASS | Clean normal-hook scratch merge above. `bazel build --config=remote-exec //cmd/gc:gc` and `make vet NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec"` returned 0; whole-tree nogo covered 1357 targets. Durable criterion6 run completed with 0 failed units. |
| 1 | Review PASS | PASS | Live ga-wsvapy9 records `verdict: pass` on the exact reviewed SHA. No review carryover or reviewer-added-test repin needed. |
| 2 | Acceptance criteria | PASS | Code and independently executed tests checked below; required PR prose prepared with RED/GREEN output, implementation base, Refs #7170 and no-deploy request. |
| 3 | Full CI-equivalent tests | PASS | Four uncached remote phases, complete native floor, by-name verification of all 21 owned tests; 0 FAIL. See counts and execution predicate below. |
| 4 | No unresolved HIGH findings | PASS | Round-3 review has no blocking style/security/spec findings; GL6 coverage gap is resolved by both added assertions. |
| 5 | Clean final branch | PASS | Reviewed checkout clean before adding this gate; this gate is the only deployer change and is committed by the normal pre-commit hook. |
| 7 | Single feature theme | PASS | Eight implementation paths, only cmd/gc and internal/git: bounded build-path default-branch probing and its tests/BUILD registration. No unrelated theme. Ancestry guard accepts only this lineage: ga-gqnrkh, ga-954f3xx and ga-dbql4ir. |

## Acceptance checks

| AC | Independent evidence |
|---|---|
| 1 | Original RED has 9 assertion failures and 6 passing characterization guards; GREEN patch preserved across rebases (review range-diff proof). Builder observed leaf `('main', nil)` and loop `main`, 120 seconds per probe. Latest leaf/helper/loop tests all PASS in this gate. Required before/after and GL6 control output is carried in the prepared PR body. |
| 2 | runCtx appends ctx.Err() on failure; deadline-only 2-second WaitDelay; existing error prefix retained. No blanket timeout, Setpgid or sentinel. The direct deadline and both orphan tests PASS. |
| 3 | DefaultBranchCtx documented; done ctx spawns nothing; cancellation returns empty/error; ordinary nonzero exit keeps master/main candidate chain. Wrapper still uses Background. Five DefaultBranch leaf tests PASS. |
| 4 | recordedDefaultBranch extracted with equivalent first-match behavior; defaultBranchForRig remains the CLI path. Existing whole cmd/gc and git suites PASS. |
| 5 | Read order is recorded rig, empty dir, latch, bounded probe; miss renders empty; first trip logs with nil-stderr guard. Owned helper and template tests PASS. |
| 6 | Pointer latch allocated in newAgentBuildParams, shared by copies; nil latch and fresh params covered. gitProbeTimeout nil-receiver and override tests PASS; whole-tree nogo/vet clean. |
| 7 | template_resolve.go changes only the defaultBranch assignment; no new resolveTemplate branch; PromptContext documents the empty value. |
| 8 | Literal make complexity-check FAIL is attributed to the existing stale baseline ga-q7p6is under criterion 3b's explicit baseline mirror rule. Pinned base and candidate normalized outputs match byte-for-byte; no baseline or threshold change. See attribution below. |
| 9 | New tests contain no time.Sleep/open-coded polling; blocking fake uses exec sleep; FIFO synchronization covers pipe-holder behavior. Existing tests pass in full suites. |
| 10 | New production functions have doc comments; exported DefaultBranchCtx documented. Test helper comment gaps are advisory in review. Build/vet, fmt, bazel-sync and check-hooks verified. |
| 11 | Refs #7170, exact implementation base, original and GL6 RED/GREEN evidence and no-deploy request in PR body. No closing keyword; no comment/issue on #7170 or PR #6164; no pack/city/hook/host config change or config key. Scope explicitly bounded controller build path, CLI and MCP probes unbounded by design. |

## Fast lane and baseline attribution

- format_lane: PASS — fresh-tree-run.sh, gate-base.sh run, run-pinned-lint.sh, make fmt-check; golangci-lint pin/resolved 2.12.0, mismatch=false.
- policy_lane: PASS — fresh-tree-run.sh and pinned gate-base.sh run; make lint (whole-tree remote nogo), check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary.
- drift_lane: PASS — separate fresh view; make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec then git diff --exit-code; no drift.
- policy_attribution: make complexity-check -> ga-q7p6is. Literal command fails on untouched pinned base and candidate with identical 254-line output; SHA256 `7aeebb322c93fe8ae34c9b9e977180956cca8afdddf318528e2f604b58a10d62`. Logs: complexity-base.log and complexity-candidate.normalized.log under the evidence directory.
- Attribution clauses: check/baseline untouched; tracker predates this run and was opened; same-run BASE_REF reproduction proves the condition; no baseline/script/census change, no new test target or test file in cmd/gc. make complexity-diff against pinned base PASS (no threshold changes). This is baseline-policy attribution, not a waiver or a literal complexity-check PASS.
- The fast driver records a nonzero exit for the attributed complexity command. All other fast lanes passed before criterion 3 launched; this retained driver status is not relabelled as a zero exit.
- ci_lane_run: n/a — no CI configuration changed.
- heavy_mode: none (`HEAVY_MODE mode=none shard_required=- watch=-`).

## Full-suite execution

- test_cmd: gate-criterion3.sh on the clean scratch merge, launched via gate-detached-run.sh systemd service; four phases below, with `--config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --keep_going` and verbose wrapped Go output.
- test_cmd_scope: full-suite
- test_exec_mode: remote-exec (BEP-derived; native floor is supplemental).
- test_log_dir: `/var/tmp/gate-ga-xn6j7xr-20261017/criterion3/evidence/remote/logs`
- test_exec_bep: `/var/tmp/gate-ga-xn6j7xr-20261017/criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json`
- Every BEP result is remote, first attempt, uncached locally/remotely, started inside its phase window and terminal; 490/490 remote results. No fallback or mixed execution.
- rbe_state: rc marker present, kill switch absent, certificate through Jan 9 2027; worker environment tree and main both `sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0`.
- fallback_reason: none
- admission: remote, floor 25 GiB, free 81.31 GiB, cap 2, wait 0.001 seconds.
- farm_test_seconds: 7838.11
- owned_topup: none — every owned test found in the original full-suite phases.
- floor_topup: complete, all 16 steps rc=0; every environment-absence SKIP selected by package (parent tests for subtests), pinned beadstest host-cache test, Darwin fsys compile and push-gate/local-concurrency self-tests. Inventory: `/var/tmp/gate-ga-xn6j7xr-20261017/criterion3/evidence/remote/floor-topup.json`.
- skip_inventory: `/var/tmp/gate-ga-xn6j7xr-20261017/criterion3/evidence/remote/skip-inventory.json`; consolidated inventory and exact commands/counts in criterion3/evidence/execution.json.
- waiver_ref: none (gascity has no waiver path).

| Phase | Command | PASS | FAIL | SKIP | Fresh BEP results |
|---|---|---:|---:|---:|---:|
| unit | `make check` | 69262 | 0 | 256 | 371 |
| acceptance | `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 410 | 0 | 13 | 24 |
| integration-packages | `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages` | 47020 | 0 | 73 | 81 |
| integration-full | `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v //test/integration:integration_test` | 189 | 0 | 14 | 14 |
| Native floor | computed environment top-ups + fixed floor | 21 | 0 | 14 | n/a |
| Total | top-level result events across 506 retained logs | 116902 | 0 | 370 | 490 |

Counts use gate-test-evidence.py's result parser and sum executions per job, not distinct tests; subtests are excluded. An interleaved partial result line in integration-packages is excluded by that parser. The by-name tool reports 440 logs with result lines; other logs include policy/build/self-test output. Unit process omissions were exercised by integration-packages; TestTutorial01 explicitly PASS there. Of the unit SKIP names, 122 have a PASS in integration-packages.

skip_justification: Remaining skips are unchanged live-service/optional-backend journeys, fixture inputs, platform/permission cases, harness helpers and placeholders. Remote environment omissions received all prescribed native top-ups. The 14 remaining native top-level SKIPs are replay/simulation inputs unset (2), optional br absent (1), live herdr journeys disabled (7), and tmux user-binding context unavailable (4); PostgreSQL subtests also need an optional DSN. They neither add nor modify these skip conditions, and no diff-owned test skips. The affected fake-git leaf/helper/template tests all execute. Complete named reasons are retained in the inventories and logs, rather than representing skips as passes.

Remote load observation (remote admission intentionally does not wait): load_threshold=15, load_waited_seconds=0, load_wait_timed_out=0, run_start_load=16.59, run_max_load=17.11, run_mean_load=12.73, run_readings=36. Native floor soft wait: threshold=15, waited_seconds=0, wait_timed_out=0, run_start_load=10.24, run_max_load=11.21, run_mean_load=10.38, run_readings=6.

The isolation wrapper was invoked and reported PASS-THROUGH -- NOT ISOLATED for the throwaway merge view (not on its isolate list); no clone/environment scrubbing is claimed. Tests ran on the remote executor, followed by the explicit native floor. No TRIPWIRE occurred.

## Changed tests, resolved by name

```text
diff_tests_executed:
  TestBuildParamsDefaultBranchCopySharesLatch PASS (integration-packages-12697e2ea0d32c278115.log, unit-23e7d9fc763496dd8095.log)
  TestBuildParamsDefaultBranchEmptyDirIsMainWithoutGit PASS (integration-packages-830685c60d0c2dd88567.log, unit-088cc38d26d094729229.log)
  TestBuildParamsDefaultBranchFreshParamsProbeAgain PASS (integration-packages-e5330b8c981927473156.log, unit-10cc7795c0ec199a5530.log)
  TestBuildParamsDefaultBranchMissedDeadlineLatchesBuild PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-5a9df316125cd0aeff2e.log)
  TestBuildParamsDefaultBranchMissedDeadlineWithNilStderrDoesNotPanic PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-493754ea2d5b4d8858c8.log)
  TestBuildParamsDefaultBranchNilLatchPaysEachDeadline PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-f679cd2551fcc64525aa.log)
  TestBuildParamsDefaultBranchProbesGitWhenNothingRecorded PASS (integration-packages-b72a67a2f9444af705e3.log, unit-cd6930bb0d0da8646500.log)
  TestBuildParamsDefaultBranchRecordedRigValueSkipsGit PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-912ab643330b35bd0b6b.log)
  TestBuildParamsDefaultBranchTrippedLatchKeepsRecordedAndEmptyDirAnswers PASS (integration-packages-821a75e4a503601af052.log, unit-64014bc0bb3833bc198c.log)
  TestBuildParamsGitProbeTimeout PASS (integration-packages-9326181a736eb30b71ca.log, unit-2bde55fab377f50b4739.log)
  TestGitProbeLatchTripsOnce PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-4d20b3d28497f844aa39.log)
  TestNewAgentBuildParamsWiresTheProbeLatch PASS (integration-packages-e4245ddf6a37030111bc.log, unit-2ef13e1b6898c5544764.log)
  TestResolveTemplateBlockedGitRendersEmptyDefaultBranch PASS (integration-packages-074fc6c34a1fb2577127.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestDefaultBranchCtx_DeadlineDuringShowRefReturnsErrorNotMain PASS (unit-38863865c30d74fbadb6.log)
  TestDefaultBranchCtx_DeadlineReturnsErrorNotMain PASS (unit-38863865c30d74fbadb6.log)
  TestDefaultBranchCtx_DoneContextSpawnsNothing PASS (unit-38863865c30d74fbadb6.log)
  TestDefaultBranchCtx_NonZeroExitKeepsCandidateChain PASS (unit-38863865c30d74fbadb6.log)
  TestDefaultBranch_NoDeadlineStillFallsBackToMain PASS (unit-38863865c30d74fbadb6.log)
  TestRunCtx_DeadlineBoundsWaitForPipeHoldingOrphanElapsed PASS (unit-38863865c30d74fbadb6.log)
  TestRunCtx_MissedDeadlineWrapsContextDeadlineExceeded PASS (unit-38863865c30d74fbadb6.log)
  TestRunCtx_WithoutDeadlineStillWaitsForPipeHolder PASS (unit-38863865c30d74fbadb6.log)
GATE_TEST_EVIDENCE_RESULT ok: 21 diff-owned tests verified by name
```
