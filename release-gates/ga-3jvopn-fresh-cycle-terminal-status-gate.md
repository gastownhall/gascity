**Verdict:** **PASS**

# Release gate: fresh-cycle terminal-status guard

- Deploy bead: `ga-3jvopn`
- Reviewed source: `30236c7797ac8a60a7e6331b539302966361b9e9`
- Base: `origin/main@849cb1a3978baf3eac0cfce5258c5598b2d0eecd`
- Isolated branch: `deploy/ga-3jvopn-gate`
- Gate date: 2026-09-22

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-issuie` records an exact-SHA PASS for `30236c7797ac8a60a7e6331b539302966361b9e9`, with no findings. |
| 2 | Acceptance criteria met | PASS | Code inspection confirmed that the guard uses the current self-claim, defers while the previous bead is open, defers when the current incarnation began after the prior close, and otherwise preserves the existing fresh-cycle path. All seven acceptance tests A-G passed independently; details follow. |
| 3 | Tests pass | PASS WITH ATTRIBUTED RAW FAILURES | The documented full local CI union ran through the isolation wrapper with rootless Podman and scheduled all 40 jobs: **34 PASS jobs, 6 raw FAIL jobs, 0 skipped/omitted jobs**. Every `cmd/gc` process shard, core integration shard, `cmd/gc` integration shard, tmux shard, and REST smoke shard passed. The six failures are non-diff-owned and satisfy criterion 3a's four proofs below. The seven diff-owned tests passed in a fresh exact-name run: **7 PASS, 0 FAIL, 0 SKIP**. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Non-diff-owned failures attributed | PASS | All six raw failures are recorded on open tracker `ga-vkhfnj`, which predates this run. None is modified by or overlaps this diff. The same tests and signatures occurred on two unrelated candidates, providing conclusive cross-PR proof; details follow. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make lint-new`, `make vet`, `make check-docs`, `make check-hooks`, `git diff --check origin/main...HEAD`, and changed-file `gofmt -d` all passed. |
| 3c | CI-config diff | PASS | Not applicable: the diff changes reconciler Go code and its unit tests, not CI jobs, matrices, timeouts, or required-check configuration. |
| 4 | No high-severity review findings open | PASS | Reviewer reported no style, security, or specification findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | The isolated branch was clean at the reviewed SHA before adding this checklist and is clean after the checklist commit. |
| 6 | Branch diverges cleanly from main | PASS | After a fresh fetch, `merge-base(origin/main, reviewed SHA)` is `849cb1a3978baf3eac0cfce5258c5598b2d0eecd`; `git merge-tree --write-tree origin/main HEAD` succeeded as tree `349ccf472817b595baa20d1fc5c6c9bac459c41e`. |
| 7 | Single feature theme | PASS | Both commits affect one concern only: deciding whether the reconciler should fresh-cycle a session when replacing its current work bead. |

## Acceptance evidence

The fresh exact-name command completed with 7 PASS, 0 FAIL, and 0 SKIP:

```text
go test -v ./cmd/gc/... -run '^(TestReconcileSessionBeads_FreshCycleDefers_WhenPreviousBeadStillOpen|TestReconcileSessionBeads_FreshCycleFires_WhenPreviousBeadClosedWithNoTimingSignal|TestReconcileSessionBeads_FreshCycleDefers_WhenIncarnationStartedAfterPreviousClose|TestReconcileSessionBeads_FreshCycleFires_WhenIncarnationPredatesPreviousClose|TestReconcileSessionBeads_FreshCycleDefers_WhenSessionAlreadySelfClaimedAnchor|TestReconcileSessionBeads_FreshCycleFires_WhenSelfClaimMismatchesAnchor|TestReconcileSessionBeads_FreshCycleGuard_UsesCurrentClaimNotCurrentlyProcessing)$' -count=1
```

- A — open previous bead defers the cycle: PASS.
- B — closed previous bead with no timing signal cycles: PASS.
- C — incarnation started after previous close defers: PASS.
- D — incarnation predates previous close and cycles: PASS.
- E — current self-claim already equals the new anchor and defers: PASS.
- F — mismatched self-claim still permits the cycle: PASS.
- G — the guard reads the current claim rather than stale `currently_processing` state: PASS.

The implementation leaves `ComputeAwakeSet` and the existing `last_woke_at`
restart-state behavior unchanged.

## Criterion 3: full-suite evidence

- `test_cmd`: `LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope`: `full-suite`
- `container_runtime`: rootless Podman 5.8.4 at `/run/user/1000/podman/podman.sock`, Ryuk disabled as required, cached pinned Dolt 2.1.7 image available
- `job_counts`: 34 PASS, 6 raw FAIL, 0 skipped/omitted
- `diff_tests_executed`: 7 PASS, 0 FAIL, 0 SKIP
- `skip_justification`: none; no full-suite job or diff-owned test was skipped
- `waiver_ref`: none
- `full_logs`: `/var/tmp/gc-local-tests.FvhA3X`
- `focused_log`: `/var/tmp/ga-3jvopn-focused-v.log`

Raw failures attributed to `ga-vkhfnj`:

- `TestAdoptPRFormulaCompileAndRun`
- `TestPersonalWorkFormulaCompileAndRun`
- `TestAdoptPRFormulaRetriesTransientReviewerStep`
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`
- `TestCleanInstallTutorialPath`

Attribution satisfies all four required proofs:

1. **Not diff-owned:** this candidate changes `cmd/gc/session_bead_cycle.go`,
   `cmd/gc/session_reconciler.go`, and
   `cmd/gc/session_reconciler_bead_reassign_test.go`; it changes none of the
   six failing tests.
2. **Tracked before the run:** open consolidated host-contention tracker
   `ga-vkhfnj` was created 2026-08-29 and was opened before this gate. This
   run's exact sightings and logs were appended to it.
3. **Not caused by the diff:** conclusive cross-PR proof. Unrelated candidates
   `ga-bequ8d@c15cfdd19228c3ff7cc2e29857ab4d95ffeca060` and
   `ga-yshs1v@da38d7dc04363aff37c9b0f140ceee0f6f78f7cc` reproduced the same
   six tests and signatures: five fixed 20-second managed-Dolt readiness
   timeouts and one unreachable ephemeral provider-owned Dolt server.
4. **No path overlap:** none of the failing test files overlaps the three
   changed paths. The two prior candidates also changed unrelated packages
   and behavior.

`failure_attribution: TestAdoptPRFormulaCompileAndRun -> ga-vkhfnj (cross-PR)`

`failure_attribution: TestPersonalWorkFormulaCompileAndRun -> ga-vkhfnj (cross-PR)`

`failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-vkhfnj (cross-PR)`

`failure_attribution: TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-vkhfnj (cross-PR)`

`failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-vkhfnj (cross-PR)`

`failure_attribution: TestCleanInstallTutorialPath -> ga-vkhfnj (cross-PR)`

The inconclusive guard does not apply because cross-PR reproduction is a
conclusive criterion-3a proof. The candidate adds no test target or resource
census load.

## Policy and hygiene evidence

- `policy_lane: make test-ci-policy — PASS`
- `make lint-new` — PASS, 0 issues
- `make vet` — PASS
- `make check-docs` — PASS
- `make check-hooks` — PASS; `.githooks` owns `core.hooksPath`
- `git diff --check origin/main...HEAD` — PASS
- changed-file `gofmt -d` — PASS, no output

No CI configuration changed, so `ci_lane_run` is not applicable.
