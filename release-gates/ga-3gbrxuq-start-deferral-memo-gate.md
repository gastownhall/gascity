# Start-gate deferral memo release gate

2026-10-28 — Superseded evidence: evaluated at `c6d39b24`; head since advanced and merged with main@`2e934b1a`; current verification is the PR CI run on the merged head.

**Verdict:** **PASS**

Scope: applies to the legacy session reconciler; v2's never-started expiry (allocator_decide.go) does not consult the memo — tracked separately.

Bead: `ga-3gbrxuq`. Review: `ga-dls0s49`; original build: `ga-n2bzsx`.
Evaluated: 2026-10-25T16:14:41.408736+00:00. No waiver; no source rebase, history rewrite or live deployment.

## Immutable source and merge

- Original reviewed commit: `c6d39b2419eabc01cb7028b30e1e2a57b6bacce3`.
- Verified carryover deploy source: `85f19d283b960c60523f2efed71de117497a7652`.
- Carryover: mechanical-conflict, patch-id `30e21ee01523299fd6e1a71c4ca5fbf2794a5546`, only `cmd/gc/BUILD.bazel`. Recomputed this run with review-carryover.sh; proof `/var/tmp/deploy-ga-3gbrxuq-current.w8a8cplb/carryover.txt`.
- Frozen main / gate_base: `ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083`.
- Canonical synthetic merge: `39ea047fff55bd5310e7b564ea1c4393f7a0454e`.
- Tree: `06fc6485d5a4e0048390e32ebcb952cdf87ccbdf`; independently equals git merge-tree prediction.
- merge_state: clean (origin/main@ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083).
- gate_base_kind: current-tip.
- merge_validation: exact clean merge built and vetted; normal pre-commit hooks active.
- DEPLOY_MODE: remote; base_ref: origin/main.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Closed review ga-dls0s49 PASS on original reviewed SHA; independently verified allowed mechanical carryover to the pinned deploy source. |
| 2 | Acceptance | PASS | All24 changed tests and174 unchanged witnesses PASS by name, plus Tutorial01 process scenarios. Source/code audit and PR draft cover all deploy-time clauses; details below. Post-merge notification remains the merge authority's later event. |
| 3 | Full CI-equivalent tests | PASS | All five forced phases plus16 native-floor steps passed. Original criterion3_valid=true/exit0; all497 remote results freshly executed. |
| 4 | No unresolved HIGH finding | PASS | Latest ga-dls0s49 review PASS; earlier findings addressed by reviewed rework; no unresolved HIGH finding. |
| 5 | Clean deploy source | PASS | Exact source and canonical scratch clean; only this record is added on isolated publication branch, through normal pre-commit. |
| 6 | Merge, build and vet | PASS | Clean merge on frozen main; full Bazel build1385 targets, integration binary compilation, make vet/nogo and active hooks PASS. |
| 7 | One feature theme | PASS |12 source files under cmd/gc only: preserve never-started creates deferred by throughput gates, including memo plumbing, classification and tests. Confirmed ancestry ids below. |

## Criterion3 command and execution

test_cmd_scope: full-suite

test_cmd: canonical gate-criterion3.sh, heavy_mode=none, own-team provenance, remote selector. The phase commands are make check; acceptance suite; integration-packages suite; full integration minus the pack's gate-latency-tests.txt; then only that file's latency tests. Exact argv/phase commands and BEPs are retained in `/var/tmp/deploy-ga-3gbrxuq-current.w8a8cplb/c3/evidence/execution.json` and each phase stdout. All phases use `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1`, a unique phase BEP and --noremote_local_fallback. The prescribed latency split is computed by the canonical runner, not a narrowed substitute.

| Phase | BEP result records | Verdict |
|---|---|---|
| acceptance | 24 | PASS; remote, uncached, attempt 1, in window |
| integration-full | 14 | PASS; remote, uncached, attempt 1, in window |
| integration-latency | 1 | PASS; remote, uncached, attempt 1, in window |
| integration-packages | 85 | PASS; remote, uncached, attempt 1, in window |
| unit | 373 | PASS; remote, uncached, attempt 1, in window |

Unit287/287 targets; acceptance7/7; integration-packages36/36; full integration14/14 shards; latency1/1 unsharded target. TestTutorial01 PASS is separately verified in tutorial01-evidence.txt, including the unit skip and real integration-packages PASS.

test_exec_mode: remote-exec (BEP-derived).
test_exec_bep:497 remote records, all uncached, attempt1, starts within phase windows, terminal PASSED. No local suite result or mixed execution credited.
rbe_state: definitions present; kill switch absent; certificate valid until2027-01-09; source/main worker-env pins equal.
fallback_reason: none.
admission: remote class;25GiB floor;64.8187GiB free; cap2; waited0.001s.
farm_test_seconds:9035.586 (sum of result execution, not wall time).
latency_leg: measured; farm probe waited30s for load1 to fall71.61 to43.66 against threshold50; both TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS, attempt1.
owned_topup: none required.
floor_topup:16/16 steps PASS:13 package selections, Darwin compile, push-gate-lock selftest and local-concurrency selftest. The prescribed native floor is additive to the remote suite.
skip_inventory:356 farm skip events (71 environment-absence,285 other), with every environment-absence package/parent selected for the floor. native-skip-inventory.json retains39 native skip events including subtests,14 top-level lines; all non-owned. Optional replay/coverage files, live-herdr gating, unset PostgreSQL DSN, absent br/tmux session and intentional fake-provider capability refusals explain these. The new memo cannot alter these early environment/capability checks; no existing test file changed. None is a changed test or an unexplained missing result.

test_counts:126694 PASS /0 FAIL /370 SKIP top-level result lines across remote and native logs, summed per job rather than unique tests.
diff_tests_executed:24/24 PASS,0 FAIL,0 SKIP/MISSING;174 additional unchanged regression witnesses PASS.
waiver_ref: none; gascity has no waiver path.
test_log_dir: `/var/tmp/deploy-ga-3gbrxuq-current.w8a8cplb/c3/evidence/remote/logs`.

Independent terminal-audit.json verifies all497 BEP results and994 original log/XML artifacts; all1533 retained digest entries match, including normalized raw-log equality. Original execution.json remains criterion3_valid=true, exit0; no failed verdict was retried or overwritten. Raw evidence is retained outside disposable checkouts.

Remote-class host-load observation (no local load admission):

```text
LOAD_GATE_SUMMARYthreshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.60 run_max_load=14.41 run_mean_load=11.56 run_readings=40 wait_first_load=13.60 wait_max_load=13.60 wait_mean_load=13.60 wait_readings=1 read_errors=0
```

Required native-floor local soft load wait:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.41 run_max_load=12.96 run_mean_load=12.21 run_readings=7 wait_first_load=10.41 wait_max_load=10.41 wait_mean_load=10.41 wait_readings=1 read_errors=0
```

## Fast lanes and complexity

C6 passed before the three fresh fast views; all fast lanes passed before runtime setup and suite launch. policy_lane: pinned golangci-lint2.12.0 format, nogo/lint, shell guards and ten policy targets PASS. drift_lane: make bazel-sync plus git diff --exit-code and empty full untracked status PASS. generated_lane: all seven CI-generated-artifact targets PASS. ci_lane_run:n/a, no CI-config change.

```text
policy: FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-3gbrxuq.ul71khpa head=39ea047fff55 tree=06fc6485d5a4 dir=/var/tmp/gc-fresh-tree.0QmBbu modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-3gbrxuq.ul71khpa,bazel-out,bazel-testlogs
policy: GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.0QmBbu
drift: FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-3gbrxuq.ul71khpa head=39ea047fff55 tree=06fc6485d5a4 dir=/var/tmp/gc-fresh-tree.K2jyoC modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-3gbrxuq.ul71khpa,bazel-out,bazel-testlogs
generated: FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-3gbrxuq.ul71khpa head=39ea047fff55 tree=06fc6485d5a4 dir=/var/tmp/gc-fresh-tree.FsEnjb modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-3gbrxuq.ul71khpa,bazel-out,bazel-testlogs
```

AC12 complexity: literal make complexity-check exits2 on BOTH untouched frozen main and the candidate; complete output is byte-identical (5846B stderr,71 findings, sha256 bc843975eaeafbdb6ab6df5968f88b123d52601c383ea0340a4df242729a57a1). No literal command PASS claimed. make complexity-diff against the exact same base exits0 with no threshold changes; this satisfies the existing ga-3zaw9mx zero-growth ruling and mayor's tracker decision. Existing tracker ga-q7p6is was opened; this run's sighting was appended/read back. No baseline/checker edit, new condition or waiver.

## Acceptance and PR obligations

The12-file source diff adds four new test files and edits no existing test file; no internal/, API/config, allocator_decide.go, pool_desired_state.go, or wake-command change. The memo is nil-safe/mutex-guarded, records four call sites over three gate kinds, replaces only on uncanceled completion, holds in both desired reconciliation and the reaper, shares one lease-anchor helper, bounds the hold at60minutes, and writes no bead state. Trace/comment surfaces and the no-memo/control-dispatcher behavior are covered by the changed tests and198-root regression report. Both owning AGENTS.md files were read and need no boundary change.

A fresh focused historical RED reproduction at the original test commit failed all four selected roots. The multi-pass test stops at pass2-started0; a separate disposable diagnostic inserted ONE existing closed-row assertion before that early failure and demonstrated gc-2 closed/state failed-create. Both raw RED outputs, exact diagnostic patch and contexts are retained; neither changes the deploy source or counts as criterion3 evidence. Final GREEN output comes from this full suite.

PR draft `/var/tmp/deploy-ga-3gbrxuq-current.w8a8cplb/pr-body.md` (sha256 `eac8362dcdace9353b80b97565a380b06f4850e30d593a334054225668864036`) includes Refs #7170, real RED/GREEN quotes, the diagnostic disclosure, four-site/three-kind explanation, known scan holes, control-dispatcher deviation, wake divergence, no existing test edits, actual complexity result, required verification outputs and no upstream discussion/issue comment. Overlap check:644 open PR identities below1000; external content not read. #3905 and#6105 remain open/unmerged; #7431/#7432 merged. ga-8pzm5d and ga-r7s62t are accounted for; four lease-convergence beads remain open. One anchor helper is retained. Publication must verify the exact PR body/head before closing. No fleet binary installed or deployed; ordinary test/build artifacts were built. Merge/notification remains with mayor/MPR; this gate does not assert shipment.

Confirmed ancestry ids:ga-3gbrxuq,ga-b6pvoks,ga-vo2p9cm,ga-n2bzsx,ga-17zmyec,7170. The isolated deploy branch must start at the exact verified deploy source, never at the provenance builder tip.

## Changed tests

- TestCityRuntimeMainTickRecordsItsStartGateDeferralsInTheCityMemo — PASS
- TestCityRuntimeReapStaleSessionBeadsReadsTheCityMemo — PASS
- TestControlDispatcherTickLeavesTheMainTicksDeferralsAlone — PASS
- TestPendingCreateLeaseAnchor — PASS
- TestReapStaleSessionBeadsHonorsTheStartDeferralHold — PASS
- TestStartDeferralMemoBeginPassTakesTheObservation — PASS
- TestStartDeferralMemoCommitReplacesAndCanceledRunCommitsNothing — PASS
- TestStartDeferralMemoDoesNotHoldACreateTheLastPassSawButDidNotOffer — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartDeferralMemoHoldPendingCreate — PASS
- TestStartDeferralMemoHolds — PASS
- TestStartDeferralMemoHoldsACreateBornBetweenSnapshotAndExecutor — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartDeferralMemoHoldsACreateTheSnapshotMissed — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartDeferralMemoRecordsOnlyStartGateDeferrals — PASS
- TestStartDeferralMemoSeenIsTheNeverStartedCreatesOfTheSnapshot — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartDeferralMemoStartOptions — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartDeferralPassFinishReadsTheContext — PASS
- TestStartDeferralPassNoteStartOutcome — PASS
- TestStartGateDeferredFirstChanceHoldsARowBornAfterTheLastPass — PASS
- TestStartGateDeferredHoldHasACeiling — PASS
- TestStartGateDeferredHoldReleasesWhenTheSessionIsNoLongerDesired — PASS
- TestStartGateDeferredHoldTraceDecision — PASS
- TestStartGateDeferredPartialStorePassLeavesTheMemoAlone — added after the `c6d39b24` evaluation; verified by PR CI
- TestStartGateDeferredPendingCreateSurvivesTheLeaseAndDrainsOnePerPass — PASS
- TestStartGateDeferredPendingCreateWritesNothingWhileHeld — PASS
- TestStartGateDeferredTraceReasonIsDistinct — PASS
- TestStartOutcomeClassificationCoversEveryStartOpOutcome — PASS
- TestStartOutcomeClassificationDrivers — PASS
- TestStartOutcomeScanFailsClosed — PASS
- TestStartOutcomeScanResolvesTheExecutorShapes — PASS
- TestWithoutTheMemoAStartGateDeferredCreateIsRolledBack — PASS
