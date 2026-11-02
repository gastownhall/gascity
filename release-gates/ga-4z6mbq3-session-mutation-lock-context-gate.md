# Context-aware session mutation lock release gate

**Verdict:** **PASS**

- Deploy bead: ga-4z6mbq3; build: ga-m8cgi8; review: ga-1dz4y76.
- Reviewed source: `fb3d51f7d66bec115ed5df187a935710bd2cadb0`; original build base: `afff5f04f6867f939f1d3df9c8b7f2b98e674d40`.
- gate_base: `e2bf5075c4e184126bf9c181b8ab1b8fac6c85d0` (`origin/main`, remote mode).
- Materialized merge: `4625fbaac5208a223c582bc29b58cc46b5680d4b`; predicted and materialized tree: `b95bf2cf49b98cc1cb56c159c4171d6cf33e769c`.
- Isolated publication branch: `deploy/ga-4z6mbq3-gate`, cut at the reviewed source; the gate record is its only additional commit. No source-branch push, rebase, merge or binary installation.
- Evidence: `/var/tmp/deploy-ga-4z6mbq3.gz4dpv2m`; exact detached run: `/var/tmp/gc-heavy-gate/runs/ga-4z6mbq3.c3.gz4dpv2m`. Completed service status: units 1/1, failed 0. Existing run resumed, never relaunched.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — live review ga-1dz4y76 final verdict pass on the exact resolved source; no review carryover used. |
| 2 | Acceptance criteria | PASS — code, 22 owned test results, documentation and PR text checked; accepted process deviations are explicit below. |
| 3 | Full CI-equivalent suite | PASS — all four forced remote phases and required native floor completed; 112703 PASS, 0 FAIL, 340 SKIP top-level result lines across 500 retained logs. Every owned test PASS by name, no owned SKIP/FAIL/MISSING. |
| 4 | No open high-severity finding | PASS — final reviewer pass, zero unresolved HIGH findings; earlier requested changes addressed at reviewed tip. |
| 5 | Clean final branch | PASS — source and canonical merge scratch clean; this gate record is the only added publication file. Final commit/index/worktree cleanliness checked before push. |
| 6 | Clean divergence and valid merge | PASS — current main equals the pinned base, merge-tree exits 0 and matches the materialized tree; normal pre-commit hooks, go build ./... and full-tree make vet/nogo passed on that merge. No self-rebase. |
| 7 | One feature theme | PASS — session mutation lock acquire and mechanically threaded Store contexts; tests and owning requirements/invariants only. Ancestry guard accepts deploy id plus build id ga-m8cgi8; no .claude contamination or unrelated commits. |

The current checkout has no `docs/PROJECT_MANIFEST.md`; the documented Makefile, TESTING.md and deployed release-gate criteria govern the commands. Preflight commit-pulls lookup returned no PR; repeated before publication.

## Acceptance evidence and disclosed limitations

1. Primitive compile-RED independently reproduced at committed test-first `f840d6b6f0236b859bf5b8c24531afc4cb4c08d6`, on original base `afff5f04f6867f939f1d3df9c8b7f2b98e674d40`: missing `WithSessionMutationLockContext` and `ErrSessionMutationBusy`, exit 1. GREEN is the full-suite per-name evidence below. **Accepted review deviation:** the Store busy test was not demonstrated as the requested assertion-first RED. It now passes, and reviewer context-ignoring/lock-dropping mutants fail. This process gap is disclosed in the PR, never claimed as completed RED.
2. Cap-1 semaphore, free-lock try first (even with done ctx), then send vs ctx.Done; busy errors wrap both sentinel and ctx error. Busy drops refs without receiving from sem. Callback runs only after acquire and completes regardless of later cancellation. Legacy entry point delegates with context.Background().
3. All requested acquire, deadline, abandoned waiter, done-context, FIFO, stress and Store no-write cases execute PASS. Supplemental `go test -race -count=1 -v -run '^TestWithSessionMutationLockContextStress$' ./internal/session` passes (1 PASS, 0 FAIL, 0 SKIP).
4. Three Store operations and the lock-owning `CommitStartedIfCurrentUnder` / `RollbackPendingCreateAtomicallyUnder` take ctx first, document wait-only bounds; all FIVE production callers pass context.Background(), including the fenced-store wrapper. No parallel Ctx variants or other Manager signatures.
5. Existing session and cmd/gc suite passes. **Accepted mechanical deviation:** store_start_fence_test.go and acp_permission_test.go use the semaphore inspection helper because the lock representation changed; existing fence assertions remain.
6. PR answers design item 4 against the actual pinned base: `observeSessionBounded` at cmd/gc/session_lifecycle_post_start_bound.go:54 calls worker.ObserveBounded; :55 calls only observe(). cmd/gc/worker_handle.go:573 calls worker.ObserveHandle. internal/worker/observe.go:46 and :51 call Manager.Get and ObserveRuntimeForInfo. Manager.Get (:2065), ObserveRuntimeForInfo (:2075), Peek (:2122), EnrichInfo (:2153), loadSessionBead, provider metadata/liveness reads and RuntimeBindingPatch do not acquire the session mutation lock. The acquire census contains mutation entry points only. An abandoned observation cannot hold this lock; underlying store/provider locks are outside this claim.
7. SESSION-START-017 names the lock deadline and test evidence. internal/session/AGENTS.md explains timed acquire, callback exclusion on busy, durable channel blocking under synctest and the exact Under variants.
8. Every changed/new exported identifier has a current-behavior doc comment.
9. Full tests, full-tree nogo/vet, hooks and BUILD drift pass. Complexity baseline is attributed below, not misreported as green.
10. PR body says Refs #7170, names original and gate bases, records actual RED/GREEN, the item-4 answer and accepted deviations. No upstream comment/issue/ping and no deploy requested; only this PR is published. Merge request goes to mayor for MPR, not a deployment request.

## Criterion 3: execution evidence

- test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.pwx9hda1 --base e2bf5075c4e184126bf9c181b8ab1b8fac6c85d0 --bead-json /var/tmp/deploy-ga-4z6mbq3.gz4dpv2m/bead.json --owned /var/tmp/deploy-ga-4z6mbq3.gz4dpv2m/owned.tsv --run-dir /var/tmp/gc-heavy-gate/runs/ga-4z6mbq3.c3.gz4dpv2m/evidence; env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-4z6mbq3.
- test_cmd_scope: full-suite; heavy_mode: none (mandatory classifier); waiver_ref: none (gascity has no waiver path).
- Remote phases: make test (bazel test //...); acceptance (bazel --config=acceptance //test/acceptance:acceptance_test); integration-packages (bazel --config=integration //test:integration_packages); full integration (bazel --config=integration //test/integration:integration_test, no smoke filter).
- Every phase forces --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback. Tests and BEP output are retained per phase. Aggregate counts include repeated jobs/phases and are not a count of unique tests.
- test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-4z6mbq3.c3.gz4dpv2m/evidence/remote/logs; gate-test-evidence.py independently re-run on that directory, verifies all22 names. 968 raw test-log/XML artifacts independently SHA256/length verified against captured CAS URI metadata.
- Native floor: 16 steps completed rc0, including all environment-absence SKIP parents grouped by package, pinned-beads module resolution, Darwin fsys compile and both runner self-tests. Native results remain local supplemental floor evidence, never counted as remote executions. floor_topup.complete=true; owned_topup=[] (none needed); fallback_reason=null.
- Native load evidence: threshold15, waited0s, no timeout, run_start_load7.62, run_max10.12, run_mean9.23, 6 readings. Remote admission itself has no local load wait/heavy slot.
- Podman: rootless socket configured before launch; TESTCONTAINERS_RYUK_DISABLED=true paired with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Pinned bd v1.3.1; default fleet bd unchanged.
- Wrapper disclosure: PASS-THROUGH, NOT ISOLATED for disposable checkouts; no claim of wrapper environment isolation. Farm execution is proven by BEP; local floor and diagnostic runs execute in those scratch views. Source worktree never used as a test mutation sandbox.

```json
{
  "test_exec_mode": "remote-exec",
  "test_exec_bep": {
    "unit": {
      "results": 368,
      "remote_executed": 368,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 24,
      "remote_executed": 24,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 78,
      "remote_executed": 78,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 14,
      "remote_executed": 14,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    }
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0",
    "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 103.16489028930664,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "farm_test_seconds": 8140.207
}
```

```text
diff_tests_executed:
  TestCommitStartedIfCurrentBoundsALosingFence PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentFencesAWriteAfterTheReread PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRaceAgainstAKill PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRedecidesPastAnUnrelatedWrite PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRefusesOverAnOutOfBandKill PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRequireModeFailsClosed PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedRefusesAnOperatorSuspendDuringTheStart PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLeasedClosesRefuseALostLease PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLeasedWritesRefuseALostLease PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyLeavesAReopenedRowItsName PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyReportsUnsupportedWithoutWriting PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyRetriesALostPostCloseFence PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartAndRollbackHoldMutationLockAcrossReadAndWrite PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStoreLockedMethodsReturnBusyWithoutWriting PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextAbandonedWaiterNeverRuns PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextBusyAtDeadline PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextDoneContextBusyWhenHeld PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextDoneContextStillAcquiresFreeLock PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextFIFO PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextReturnsFnError PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextStress PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextUncontended PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 22 diff-owned tests verified by name
```

## Skips and attribution

skip_inventory: `/var/tmp/gc-heavy-gate/runs/ga-4z6mbq3.c3.gz4dpv2m/evidence/remote/skip-inventory.json`, raw printed reasons and per-name logs retained. 326 remote skip rows (71 environment-absence, 255 other); 340 top-level SKIP result lines including required floor/repeated jobs. Independently checked 227 distinct `_test.go` skip sites: all UNCHANGED. The 39 remaining rows resolve to five unchanged harness/fixture files (close-notification capabilities, ready-parity fixture, runtime conformance classified-start skips, legacy topology fixture, and the intentionally skipped crash-injection fail subtest). Supplemental proof: `/var/tmp/deploy-ga-4z6mbq3.gz4dpv2m/skip-supplemental-proof.json`. No changed skip helper, shared test hunk or owned test is excused.

skip_justification: unchanged platform-specific, optional external-tool/service, explicit scenario/fixture gates and intentional harness skip tests do not test the context-aware session mutation acquire. The required native floor attempted every environment-absence parent; non-owned remaining SKIPs include replay/simulation fixtures, live herdr/br, Postgres DSN, binding-environment checks and deliberately classified fake-provider start failures. No session lock test skipped. Raw reasons inventory is the authority for each name; the count is not treated as PASS evidence.

failure_attribution: none for candidate tests (0 FAIL). Review's known tmux 1ms-deadline flake ga-vkhfnj did not fire in this independent run; it is neither fixed nor waived here.

policy_attribution: make complexity-check -> ga-q7p6is, explicit criterion3b baseline-policy mirror rule. Untouched pinned main and canonical merge produce identical normalized 240-line reports, SHA256 cbd540538967cabb007584a19957332b45bc5f20f06a6a0241c34bac4f14f8cf. Diff-scoped complexity check exits0, no threshold changes. Tracker read, same-run sighting 378c8d7f-45fe-5ec9-b00d-652c692cd440 verified; no new tracker or waiver. The unchanged baseline offenders are not this change's lock code.

## Fast lane and criterion 6

Normal pre-commit hook diagnostic rc0; fresh canonical materialization then ran the normal hook again. Initial setup rc10 produced no verdict and no suite; it was diagnosed and retried once, not bypassed. Retained logs: materialize-hook.log, materialize-retry.log, build.log, vet.log.

policy_lane: PASS — fresh-tree views with gate-base.sh run; fmt-check-changed (pinned lint), changed-package nogo, check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary; all eight static policy/format Bazel targets forced uncached remotely. Static target list retained in static-policy.sh; raw logs format.log, policy.log, static-policy.log.

drift_lane: PASS — separate fresh view, make bazel-sync followed by git diff --exit-code (no changes). This is CI's generated BUILD drift check. make check-hooks confirms .githooks active. Together with make test these cover make check's required commands. No fast-lane command wrote the suite's canonical merge scratch.

ci_lane_run: n/a (no CI workflow/job/matrix/timeout/required-check changes). Dashboard/API/OpenAPI untouched; dashboard test/build targets included in full //... suite, manual dashboard preview not applicable.

RED diagnostic load: threshold15, waited60s, start14.36. Supplemental stress load: threshold15, waited0s, start11.39/max12.20/mean11.91. Full-suite floor load recorded above. Execution/admission records and per-job logs remain retained, never deleted.
