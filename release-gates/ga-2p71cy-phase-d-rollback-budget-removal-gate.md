# Release gate: Phase D rollback budget removal

- Deploy bead: `ga-2p71cy`
- Review bead: `ga-cois5f` — PASS
- Build bead: `ga-yufa.4`
- Reviewed source: `875b527ddfb6c02dc541c380155a98b1f08b116d`
- Base: `origin/main@e589fdea3330ca33b7f6b53a1bc90699893ab4f3`
- Deploy mode: `remote`; push remote: `fork`
- Pre-flight: no pull request carries the reviewed source
- Overall verdict: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-cois5f` records PASS for exact reviewed source `875b527ddfb6c02dc541c380155a98b1f08b116d`, with no blocking style, security, specification, or coverage finding. |
| 2 | Acceptance criteria met | **PASS** | The prerequisite transactional lifecycle writes merged in PR #4232 on 2026-07-20. On 2026-09-14 the mayor explicitly reopened this bead because the requested 24-hour operational soak was long past. The remaining `maxRollbacksPerTick = 5` band-aid and its deferral path are removed; all six stale or mismatched creates in each regression fixture now roll back in one tick while an independent planned start still executes. The title's `staleCreatingStateTimeout=5m` wording is historical: main already used one minute before this candidate, and this diff does not change it. |
| 3 | Tests pass | **PASS with two attributed raw failures** | The documented isolated 40-job full suite completed **38 PASS / 2 raw FAIL / 0 omitted jobs**. Both failures occurred during fixture `gc init` because a concurrent initializer exposed a partially migrated shared `hq` database and `bd` refused the remaining migrations; tracker `ga-e2z1zb` predates this run and contains the reproduced root cause. Both diff-owned reconciler tests passed explicitly by name. The same-package failure passed alone and coverage proves the changed reconciler function was not executed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-cois5f` reports no blocker, major, unresolved HIGH, or security finding. Removing `TraceOutcomeRollbackDeferred` is dead-code cleanup because the deferral outcome is no longer reachable. |
| 5 | Final branch is clean | **PASS** | The exact reviewed source was evaluated in a clean detached worktree. `gofmt -l` on all three changed Go files and `git diff --check` produced no output before this checklist was added. |
| 6 | Branch diverges cleanly from main | **PASS** | `origin/main` remains the recorded base. `git merge-tree --write-tree origin/main 875b527ddfb6c02dc541c380155a98b1f08b116d` exited 0 and produced tree `851f51254074693961ddf0bc6fada0a21e671c4e`; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The red/green TDD pair changes only `session_reconciler.go`, its existing test file, and its trace outcome constants. The single theme is removing the five-rollbacks-per-tick budget after lifecycle writes became transactional. |

## Acceptance evidence

- The forward pass no longer returns early from `attemptRollbackPendingCreate` after five rollbacks.
- The phase trace retains `rollback_count` and removes only the obsolete `rollback_budget` field.
- The unreachable `rollback_deferred` trace outcome is removed.
- `TestReconcileSessionBeads_RollsBackAllMismatchesInOneTickAndStillStarts` verifies six runtime-identity mismatches close in one tick, no deferral is logged, and an unrelated planned start still reaches active state.
- `TestReconcileSessionBeads_RollsBackAllStaleNoRuntimeCreatesInOneTickAndStillStarts` verifies the same behavior for six stale never-started creates.
- The candidate changes no timeout constant. `staleCreatingStateTimeout` was already `time.Minute` on the base, so there is no missing five-minute revert in this diff.
- The two commits are `ab0e09f1c70894453c6ae97b640f35f9d288b098` (red) and `875b527ddfb6c02dc541c380155a98b1f08b116d` (green), both citing `ga-yufa.4`.

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope: full-suite`
- `test_counts: 38 PASS / 2 raw FAIL / 0 omitted jobs` out of 40 jobs
- `required_job_coverage`: all six `cmd-gc-process` shards ran; five passed and one has the attributed fixture-init failure below. The unit, package-integration, runtime/tmux, review-formula, bdstore, and REST groups all ran to completion; only one review-formula shard has the second attributed fixture-init failure.
- `diff_tests_executed:`
  - `TestReconcileSessionBeads_RollsBackAllMismatchesInOneTickAndStillStarts`: PASS in its full-suite shards and in a named `-count=1 -v` run.
  - `TestReconcileSessionBeads_RollsBackAllStaleNoRuntimeCreatesInOneTickAndStillStarts`: PASS in its full-suite shards and in a named `-count=1 -v` run.
- `skip_justification: none at job level`
- `waiver_ref: none`
- `ci_lane_run: n/a (no workflow, job, matrix, timeout, or required-check configuration changed)`
- Full-suite logs: `/var/tmp/gc-local-tests.rjj6nT`
- Same-package focused coverage: `/var/tmp/ga-2p71cy-fresh-managed.cover`

### Raw failure attribution

Both failures match the random-`vNN -> v66` concurrent-initializer condition reproduced and specified in open tracker `ga-e2z1zb`, created before this gate. This run's exact sightings were appended to that tracker and read back successfully.

| Raw failing test | Attribution |
|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `failure_attribution: ... -> ga-e2z1zb | clause 3(c): COVERAGE — PASS`. The full shard failed during `initAndHookDir` when `hq` was observed at v50 and `bd` refused 16 pending migrations. The identical reviewed SHA passed the test alone in 7.55s. Its focused coverage reports `reconcileSessionBeadsTracedWithNamedDemand` at 0.0%, proving the modified production function was not entered. `clause-4-guard: same_package=yes proof=c added_test_load=no` — the candidate adds no test file or top-level test; it rewrites two existing tests and reduces the package by 37 lines. |
| `TestPersonalWorkFormulaCompileAndRun` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture stopped during `gc init`, with `hq` at v41 and 25 migrations refused, before formula compilation or execution. The test is in `test/integration`; no failing path overlaps the candidate, and session reconciliation cannot execute before city initialization succeeds. |

The raw failures remain recorded as failures. Their attribution satisfies the predating-tracker, causal-disproof, and path/load guards without weakening the independent passing evidence for the changed behavior.

## Static and policy evidence

- `go build ./...` — PASS.
- `go vet ./...` — PASS.
- `make test-ci-policy` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `make check-docs` — PASS.
- `make check-hooks` — PASS; `.githooks` owns `core.hooksPath`.
- `gofmt -l` on all three changed Go files and `git diff --check` — PASS.

## Environment integrity

- The rootless Podman socket was active at `/run/user/1000/podman/podman.sock`, with `TESTCONTAINERS_RYUK_DISABLED=true` set before the full sweep.
- The repository's isolated test wrapper supplied the full local suite's test topology; no shared build cache was cleared or redirected.
