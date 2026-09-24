# Release gate: ga-b8i2nl — cmd/gc shard flake fixes

**Verdict:** **FAIL — WAIVED BY MAYOR**

**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Deploy bead:** `ga-b8i2nl`

**Build bead:** `ga-agtlhi`

**Review bead:** `ga-g1h0pq`

**Reviewed commit:** `e4b2d4f1a9aeb3c959ac342de157667e506efac7`

**Base:** `origin/main@a565081fb87c13de8366594ad40ddfd731469539`

**Deploy mode:** remote (`origin` and `fork` are GitHub remotes)

**Deploy branch:** `deploy/ga-b8i2nl-gate`

The original technical gate remains a failure. Criterion 3 failed on a
same-package `cmd/gc` process test and two other failures that passed in the
exact-base comparison, so the release-gate attribution policy did not permit
them to be treated as pre-existing. The Mayor's bead-specific 2026-08-18
ruling, recorded verbatim in `ga-b8i2nl`, explicitly authorizes this deploy to
proceed without rewriting that result as green.

The inherited bead title describes the older deploy lineage. The reviewed
artifact evaluated here is test-only: order-gate concurrency coverage and the
stop-fallback timeout correction added while repairing that lineage's gate
failures.

`docs/PROJECT_MANIFEST.md` is absent from both this checkout and
`origin/main`; this record evaluates the active seven release criteria from
the deployer contract and the sharded test commands documented in
`TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-g1h0pq` records `REVIEW VERDICT: PASS` for the exact reviewed commit and carries the earlier PASS evidence from `ga-4knhwk`. |
| 2 | Acceptance criteria met | PASS | The reviewed diff is limited to two `cmd/gc` test files. It preserves the order-gate assertions while making overlap coverage tolerate a deliberately delayed check, and changes three stop-test call sites from a live `time.Second` cap to production's normal `0` timeout path. All 11 changed top-level tests pass. |
| 3 | Tests pass | **FAIL — WAIVED** | The changed tests and most required lanes pass, but the 40-job full sweep has six failed jobs. Only the two tmux failures and tutorial-path output failure meet all four attribution clauses under the original policy. The Mayor explicitly waived this failed criterion for `ga-b8i2nl`; the original counts and failures remain recorded below. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking or HIGH findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit immediately before this gate file was written. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main <reviewed-sha>` exited 0 and produced tree `df90120dbedfda362a3bc8824ea5b4f4b2190daa`. The lineage is three commits behind and four ahead of current main, but merges without conflict. No self-rebase was needed. |
| 7 | Single feature theme | PASS | All four lineage commits and both changed files stay within `cmd/gc` test reliability for the same deploy retry. No production behavior or second package is introduced. |

## Pre-flight

The recorded SHA resolves to the full commit above. The GitHub commit-to-PR
query returned no associated pull requests, so the target has not already
merged and is not a closed or superseded PR.

## Criterion 3 evidence

The gate used CI-pinned Dolt `2.1.7` and official `bd 1.1.0`, with temporary
files on `/var/tmp`. The rootless Podman socket and testcontainers environment
were configured before testing. The shared Go build cache was not cleaned or
relocated.

### Diff-owned tests

Command:

```text
go test -json -count=1 -timeout 15m ./cmd/gc -run '^(TestOrderGateReadsScaleWithStoresNotOrders|TestOrderGateIssuesZeroLedgerReadsOnASplitCity|TestOrderGateStillReadsTheLedgerOnASingleStoreCity|TestConditionChecksRunConcurrentlyWithinTheirCap|TestConditionCheckOverlapSurvivesAStragglerStart|TestConditionCheckRunsExactlyOncePerOrderPerTick|TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid|TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes|TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop|TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity|TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning)$'
```

Top-level counts: **11 PASS, 0 FAIL, 0 SKIP**. Five nested table/subtest cases
also passed.

`diff_tests_executed`:

- `TestOrderGateReadsScaleWithStoresNotOrders` — PASS
- `TestOrderGateIssuesZeroLedgerReadsOnASplitCity` — PASS
- `TestOrderGateStillReadsTheLedgerOnASingleStoreCity` — PASS
- `TestConditionChecksRunConcurrentlyWithinTheirCap` — PASS
- `TestConditionCheckOverlapSurvivesAStragglerStart` — PASS
- `TestConditionCheckRunsExactlyOncePerOrderPerTick` — PASS
- `TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid` — PASS
- `TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes` — PASS
- `TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop` — PASS
- `TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity` — PASS
- `TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning` — PASS

`waiver_ref`: Mayor's 2026-08-18 bead-specific ruling recorded verbatim in
`ga-b8i2nl`, section 1 ("ga-b8i2nl: WAIVED").

`skip_justification`: none required; zero changed tests skipped.

### Documented full sweep

Command: `LOCAL_TEST_JOBS=4 make test-local-full-parallel`, with the prepared
container and pinned-tool environment.

Log directory: `/var/tmp/gc-local-tests.fOxz0A`.

Job counts: **34 PASS, 6 FAIL, 0 SKIP** out of 40 jobs.

Five of six `cmd/gc` process shards passed; all six `cmd/gc` integration
package shards, all four integration-core shards, both PR REST-smoke shards,
all formula lanes, product-metrics, and static self-tests passed.

### Failure attribution

These failures satisfy all four attribution clauses:

- `TestGetKeyBinding_CapturesDefaultBinding` → `ga-afqddr`; exact base result
  in `/var/tmp/gc-local-tests.0azm2T`: FAIL with the same empty default binding.
- `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → `ga-afqddr`; exact base
  result in `/var/tmp/gc-local-tests.0azm2T`: FAIL with the same empty binding.
- `TestCleanInstallTutorialPath` → `ga-rsktma`; exact base direct result: FAIL
  with the same circuit-breaker cleanup text contaminating the expected `tra`
  prefix. The `test/integration` package does not overlap this diff.

These failures cannot be attributed:

- `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` → active
  fix-deploy tracker `ga-vb82ss`. Candidate result: FAIL because
  `OpenNativeStorage(rig)` exceeded its schema-initialization deadline. The
  failing test and this diff are both in package `cmd/gc`, so mandatory clause
  4 (no package/path overlap) fails regardless of base behavior.
- `TestSQLiteLegacySnapshotSIGKILLAtBoundaries/legacy-private-recovery-before`
  → `ga-5dsf6n`. Candidate result: FAIL waiting 10 seconds for the SQLite
  child-protocol line. Exact base's full unit-core job passed, so clause 3
  (proven pre-existing) fails; the different subcase that failed in an earlier
  candidate does not repair that proof.
- `TestHumaBinary_SessionMessageAsync` → `ga-lpfjhc`. Candidate result: FAIL
  during fixture-city initialization with the tracked beads#4566 dirty-table
  migration signature. The same exact-base full sweep did not fail this test,
  so clause 3 fails.

### Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS
- `GOLANGCI_LINT_CACHE=/var/tmp/gc-gate-ga-b8i2nl-lint-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues; conservatively expanded to the full repository because the reviewed lineage predates an upstream-deleted dashboard asset
- `make fmt-check-changed` — PASS (`no changed existing Go files`)
- `go build ./cmd/gc/...` — PASS
- `go vet ./cmd/gc/...` — PASS
- `git diff --check origin/main...HEAD` — PASS

## Waiver disposition

The technical gate result is still **FAIL**. The Mayor's 2026-08-18 ruling
explicitly names `ga-b8i2nl` as allowed to proceed because every failure is
mapped to a specific tracker: `ga-afqddr`, `ga-rsktma`, `ga-vb82ss`,
`ga-5dsf6n`, or `ga-lpfjhc`. This test-only diff changes order-gate coverage
and three stop-test timeout arguments; it has no plausible mechanism to reach
tmux bindings, tutorial-path installation, Dolt teardown/schema
initialization, SQLite SIGKILL boundaries, or async session messaging. No
failure is in a file or subsystem this diff changes.

The bead-specific waiver supersedes the earlier no-push disposition and
authorizes pushing this isolated deploy branch, opening its pull request, and
routing merge authority to mayor/mpr. No rig agent merges the pull request.
