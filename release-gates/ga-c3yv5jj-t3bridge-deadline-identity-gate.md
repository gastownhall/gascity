# T3 multi-candidate deadline identity release gate

**Verdict:** **PASS**

Bead: ga-c3yv5jj. Build: ga-tmz92nn. Review: ga-ptmtpq1.
Evaluated on 2026-10-23; retained evidence audited at 2026-10-23T16:43:14.329767+00:00.

- reviewed_commit: `3fc6702df47a403b140c6e6a00704b13a303ba20`
- gate_merge_commit: `b92127dbfe9c4adb4883b104e9bc2cd90688e8e7`
- gate_tree: `f342f485328290f439da850caf438d6b551d5a25`
- gate_base_sha: `87a47634d0cebe611874df321c723bba95dcffea`
- deploy_mode: remote
- merge_state: clean (origin/main@87a47634d0cebe611874df321c723bba95dcffea)
- gate_base_kind: current-tip (frozen at gate preparation)
- merge_validation: gate tests the clean synthetic merge; merge authority must check the current PR head and merge state before merging. No rebase or merge performed by deployer.
- test_cmd_scope: full-suite
- waiver_ref: none (Gas City has no waiver path)
- heavy_mode: none

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-ptmtpq1 records verdict pass and deploy_commit matching reviewed_commit above. Style, security and specification findings contain no blocker. No review carryover or reviewer-added-test exception used. |
| 2 | Acceptance criteria met | PASS | Deterministic late context timer regression preserves errors.Is(context.DeadlineExceeded), ErrRuntimeUnavailable and joined candidate details. ctxEndErr checks cancellation first, then the elapsed monotonic deadline. Existing stalled-server test and both bearer cases PASS by name. Request-write fence is unchanged. |
| 3 | Full test scope and changed tests | PASS | Five fresh remote phases plus all required native floor steps completed. Three unrelated conditions attributed under 3a below; raw failures remain retained. Changed regression PASS by name, no changed test skipped or failed. |
| 4 | No unresolved high severity finding | PASS | Reviewer style/security/specification review reports none; unresolved HIGH count 0. HTTP snapshot sibling error handling is a disclosed non-blocking residual outside this bead (ga-yd8r0p). |
| 5 | Feature source clean | PASS | Canonical scratch HEAD/tree match the frozen context; tracked and untracked status empty before and after execution. Only this gate record is added for publication; older untracked role-worktree gate records are retained separately. |
| 6 | Merge state recorded; build and vet | PASS | Clean synthetic merge built by bazel build --config=remote-exec --noremote_local_fallback --keep_going //...; nogo includes vet. CRITERION6 PASS retained before the full suite. |
| 7 | One feature theme | PASS | Exactly provider.go and turn_fence_test.go under internal/runtime/t3bridge, +70/-0, preserving deadline error identity. No dependency, import, build, CI-config or unrelated feature change. |

The rig has no docs/PROJECT_MANIFEST.md or matching work-packages file. Acceptance comes from the concrete build bead description and reviewed patch; required commands come from TESTING.md, Makefile and .github/workflows/bazel.yml.

Acceptance evidence:

1. Builder/reviewer TDD evidence records a test-only RED commit and production GREEN commit. The new lateTimerContext deterministically separates socket deadline expiry from context timer delivery; the deployer's independent full-suite run supplies GREEN evidence.
2. The 17 production lines wrap the context error with the joined candidate errors using %w. ctxEndErr returns ctx.Err() when set, otherwise DeadlineExceeded after Deadline(), otherwise nil. Existing replay/write-fence behavior remains in place.
3. TestObserveTurnKeepsTheDeadlineWhenTheContextTimerRunsLate PASS; TestObserveTurnStalledServerReturnsByTheDeadline PASS, bearer=test-bearer PASS, bearer= PASS. Results appear in the retained unit t3bridge log.

## Test execution and integrity

The fast lane completed first, before criterion 3. Each format, policy and drift lane used a separate fresh tree; none mutated the canonical scratch.

- format_lane: run-pinned-lint.sh -- make fmt-check; PASS, pinned version 2.12.0 matched.
- policy_lane: gate-base.sh run -- make lint with remote nogo; make check-hooks check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; workflow Python policy tests (5 + 15); PASS.
- drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec, git diff --exit-code and empty git status --porcelain --untracked-files=all; PASS.
- ci_lane_run: n/a (no CI-config change in this diff).
- test_cmd: gate-criterion3.sh through gate-detached-run.sh, remote admission and isolated-test-run.sh --rbe. unit=make check; acceptance=bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages=bazel test --config=integration //test:integration_packages; integration-full=bazel test --config=integration //test/integration:integration_test excluding the canonical latency list; integration-latency=the canonical list only, worker-probed and unsharded. Every phase adds --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback, verbose result collection and BEP capture.
- test_exec_mode: remote-exec (derived from the full-suite BEP; the mandatory native floor is recorded separately).
- test_exec_bep: unit 373/373, acceptance 24/24, integration-packages 80/80, integration-full 14/14, integration-latency 1/1 fresh remote result attempts. Total 492/492; cached remotely 0, cached locally 0, other strategies 0, retries 0; every result has attempt 1, in-window start and terminal verdict. Canonical BEP files are retained under /var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote.
- rbe_state: worker-env tree pin equals origin/main pin; rc marker present; kill switch absent; certificate valid until 2027-01-09. See c3/evidence/rbe-state.json.
- fallback_reason: none; no local fallback or suite stitching.
- admission: remote cap 2, provisional disk floor 25 GiB; native-floor wrapper admitted at 56.959 GiB after 5.002 seconds. Remote phases have no local-heavy wait. Local floor uses its own bounded soft load wait.
- farm_test_seconds: 9090.97
- skip_inventory: /var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote/skip-inventory.json (356 farm skip records: 71 environment-absence, 285 other).
- owned_topup: [] (new regression already PASS in the full remote unit output).
- floor_topup: /var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote/floor-topup.json; complete=true, all 16 steps exit 0. Every environment-absence SKIP was selected by package, with parent tests selected for subtests; pinned beadstest, Darwin compile and both runner self-tests included. Remaining non-owned SKIPs are inventoried, never counted as PASS.
- latency_leg: /var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote/latency-leg.json retains verdict failed, clear probe, no retry. TestGcBdLatencyBudget PASS; TestGcSeatHookLatencyBudget FAIL, attributed below. The raw failure was neither replaced nor rerun into PASS.
- evidence_integrity: 19 preserved raw-summary digests and 984 artifact digests verified; final audit recomputed the execution predicates from BEP. Raw execution.json and all partial failure summaries remain unchanged. Completed native-floor service and watcher both inactive/dead, Result=success, MainPID=0.

Counts below are copied directly from gate-test-evidence.py. A test run in several jobs counts once per job; the failure reader also names a parent/subtest pair for one condition.

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote/logs logs=508 logs_with_results=442
test_counts: PASS=125493 FAIL=4 SKIP=370
  (top-level result lines, column 0, summed over the 508 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
failed: TestGCLiveContract_BeadsAndEvents (integration-full-65412ec6e874f54aa912.log)
failed: TestGcSeatHookLatencyBudget (integration-latency-f97796e57f2c5c3606da.log)
failed: TestPrePushSuiteWorkerEnvGuard/gh_failure_is_best_effort (unit-db0feb4ad70da6248282.log)
failed: TestPrePushSuiteWorkerEnvGuard (unit-db0feb4ad70da6248282.log)
diff_tests_executed:
  TestObserveTurnKeepsTheDeadlineWhenTheContextTimerRunsLate PASS (unit-a23a357eedd1739a53e0.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name
test_log_dir: /var/tmp/gc-gate.ga-c3yv5jj.MaGJex/c3/evidence/remote/logs
```

skip_justification: No changed test skips. Unchanged platform/backend/lane filters and absent optional fixture inputs account for farm SKIPs. All environment-absence records were retried through the required native floor. Remaining native SKIPs are optional replay/simulation input manifests, opt-in live herdr journeys, absent Postgres/BR fixtures, intentional classified-start-error cases and pre-existing tmux binding conditions. They do not exercise the modified T3 RPC error join. The new shared lateTimerContext helpers are referenced only by the new regression; they cannot reach these skip sites. The existing T3 start-grace backend filter is unchanged. Individual names, reasons and retained logs remain in skip-inventory.json and floor-logs; no non-owned SKIP is promoted to PASS.

## Attributed failures (criterion 3a)

No failure is owned by this diff. Failure sites are UNCHANGED, share zero hunks, and their packages do not overlap internal/runtime/t3bridge. The declared resource census, suite targets and test-file set are unchanged. Complete computed test-binary import closures, not a one-hop search, support the structural proofs.

- failure_attribution: TestPrePushSuiteWorkerEnvGuard/gh_failure_is_best_effort (and parent) -> ga-witwk55; clause 3(a) mechanism. The scripts test-binary import closure excludes the sole changed production package. Same-run tracker is permitted by the protocol's clause-2 first-sighting exception because clauses 1/4 are clear and the mechanism proof landed. No earlier matching tracker was found; no base reproduction or load causation is claimed. Proof: failure-proof.json.
- failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy; clause 3(a) mechanism. Integration-tagged import closure (65 module packages) excludes t3bridge. The failure is the unchanged missing-assignee probe. Repeat qualifies for fix-carrying: current build ga-tmz92nn is stamped gc.fixes_tracker=ga-xf3ijk3; designated fix ga-ze0aym0 is stamped gc.fixes_tracker=ga-od4rcxy and its resolved fix commit remains unlanded on freshly fetched origin/main. Proof: integration-failure-proof.json.
- failure_attribution: TestGcSeatHookLatencyBudget -> ga-bqsrp4o; clause 3(a) mechanism INCLUDING external gc binary. The fixture selects subprocess sessions and a sleep command; the registry's constructor closures never execute the changed T3 RPC failure path. Import closure alone is not the proof. Repeat qualifies for fix-carrying via ga-tmz92nn and designated, stamped, unlanded fix ga-yu01v6x. Clear-probe lane failure is retained with budget/statistic unchanged; no retry. Proof: latency-failure-proof.json.

All three condition trackers were reopened live and cover the named tests. Both designated repeat-fix commits resolve and are not ancestors of current origin/main. No waiver was requested or used. All proof files are under /var/tmp/gc-gate.ga-c3yv5jj.MaGJex; no raw failed result is rewritten as green.

## Load and environment

The isolation wrapper was invoked; for this unlisted scratch path it explicitly reports PASS-THROUGH, not a cloned private environment. The remote phase helper and native top-up runner perform their documented environment scrubbing; the pinned bd is v1.3.1. This limitation is retained, not described as isolation. ICU74 check: verdict=OK, provenance=recorded (three library hashes in fast/units/gate.w1.stdout.log).

Native floor's own load-gate-run.sh record (soft timeout permits an ordinary gate to proceed; this is not a heavy-package composite):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=23.08 run_max_load=56.34 run_mean_load=38.77 run_readings=11 wait_first_load=72.79 wait_max_load=72.79 wait_mean_load=38.57 wait_readings=61 read_errors=0
```

load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 23.08
run_max_load: 56.34
run_mean_load: 38.77
run_readings: 11

Remote driver-host observations, never asserted as failure causes: original unit start/max/mean=123.45/139.59/126.92 (11 readings); continuation=105.79/111.15/73.18 (75); latency wrapper=77.13/85.32/83.63 (6); native wrapper=72.79/68.96/38.41 (72). The floor's separate wait above is the local wait evidence. The latency worker probe was clear at load1=16.33, threshold=50.

Evidence root: /var/tmp/gc-gate.ga-c3yv5jj.MaGJex. Keep all raw BEP, per-job logs, manifests, proof files and summaries for subsequent review; no gate run directory is deleted.
