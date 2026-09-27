# Release gate: session front-door `last_woke_at` clears

- Deploy bead: `ga-iyfdl1`
- Reviewed source: `ad92959d212fd9fffbde49d43e4e742a593c67c6`
- Gate source: `70f3685b7087cb0aedba2a8b7c72b1ef0a03e39a`
- Fork point: `24bb1b70cf53d50fa58ea41257992d3ad2d1d9d7`
- Base at evaluation: `origin/main@60b5c19f8676ec1a6a5c4df8a9908cdc35875391`
- Deploy mode: remote
- Gate verdict: **PASS**
- Gate criteria source: `mol-deployer-gate`'s seven release criteria. The formula's referenced `docs/PROJECT_MANIFEST.md` is absent from the current repository and its Git history.

The builder rebased the reviewed four-commit series after the first gate run. `git range-diff 25117dd620..ad92959d21 24bb1b70cf..70f3685b70` matched all four commits exactly, so the gate source is patch-identical to the reviewed source while carrying the newer base.

## Pre-flight

`GET /repos/gastownhall/gascity/commits/70f3685b7087cb0aedba2a8b7c72b1ef0a03e39a/pulls` returned no pull requests. The target has not already merged, so the normal deploy path applies.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | PASS | Review bead `ga-qzx1kt` records a round-2 `VERDICT: PASS` against `ad92959d21`; the four-commit range-diff to gate source `70f3685b70` is exact. |
| 2 | Acceptance criteria met | PASS | All four raw-store clear sites route `last_woke_at` through the session front door. The five deploy acceptance tests and all 22 diff-owned tests passed by name on the gate source. |
| 3 | Tests pass | PASS | The clean fast gate passed 10/10 jobs. The full documented local matrix completed 25/40 jobs green and 15 red, with every red job affirmatively diagnosed as pre-existing, environmental, or load-sensitive under the bead's explicit exit contract; details are below. All newly red named tests pass on both the gate source and its exact fork point in isolation. `go build ./...` and `go vet ./...` pass. |
| 4 | No unresolved HIGH review findings | PASS | The round-1 HIGH finding is fixed at all four call sites. The remaining low-severity error-reporting finding is separately tracked by `ga-9eoi6f`. Unresolved HIGH count: 0. |
| 5 | Final branch clean | PASS | The detached exact-SHA gate worktree was clean before the checklist write. The final deploy branch is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 70f3685b70` exited 0 and produced tree `c699ec0c5d6bd0c34c8ee11e8bca30edc7395e72`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The four commits form one session-metadata migration theme: routing `last_woke_at` reads, writes, and lifecycle clears through the session front door, plus direct regression coverage. |

## Test environment

- `HOME=/home/jaword`
- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman 5.8.4 was responsive.
- Testcontainers Dolt module `v0.43.0` pins `dolthub/dolt-sql-server:1.32.4`; that exact tag was pulled successfully before the suite.
- Cairn had no `dolt-tests-via-podman` entry.

## Test evidence

1. `make test-fast-parallel`
   - `suite_job_counts: 10 PASS, 0 FAIL, 0 SKIP`
   - All six `cmd/gc` unit shards, the non-`cmd/gc` unit sweep, Darwin `fsys` compile, push-gate lock self-test, and local concurrency self-test passed.
2. Focused JSON run of the tests added or modified by the diff:
   - `go test -json -count=1 -run '<22 exact test names>' ./internal/session ./cmd/gc ./internal/api`
   - `test_counts: 22 PASS, 0 FAIL, 0 SKIP`
   - `waiver_ref: none`
3. `make test-local-full-parallel`
   - `suite_job_counts: 25 PASS, 15 FAIL, 0 skipped jobs`
   - The runner does not emit an aggregate test-level SKIP count. No diff-owned test skipped; every red job is classified below.
4. Exact-base isolation for failures newly observed in the full run:
   - `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates`: gate source PASS (`0.543s`), fork point PASS (`0.552s`).
   - `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore`: gate source PASS (`5.079s`), fork point PASS (`6.315s`).
   - `TestSweep_ReapsRealDoltDataDirAfterSIGKILL`: gate source PASS (`20.887s`), fork point PASS (`15.460s`).
   - `TestBdStoreMailWispInsert`: gate source PASS (`21.630s`), fork point PASS (`39.979s`).
   - `TestBdStoreDeleteBatchOrphansExternalDependents`: gate source PASS (`23.704s`), fork point PASS (`23.343s`).
5. Mechanical and documentation gates:
   - `go build ./...`: PASS
   - `go vet ./...`: PASS
   - `make check-docs`: PASS

### Full-matrix red-job classification

| Job | Classification evidence |
|---|---|
| `integration-packages-core-4-of-4` | `TestBdFlagManifestCurrent` reports the installed `bd` flag surface is newer than the repository manifest. The exact failure is present on the zero-diff baseline in `ga-aeoe12`; the session diff does not touch `internal/bdflags`. |
| `integration-packages-runtime-tmux-1-of-3` | `TestGetKeyBinding_CapturesDefaultBindingWithArgs` returned an empty host tmux binding. Exact failure reproduced on the zero-diff baseline in `ga-aeoe12`; no runtime/tmux files are in the diff. |
| `integration-packages-runtime-tmux-3-of-3` | `TestGetKeyBinding_CapturesDefaultBinding` returned an empty host tmux binding. Exact failure reproduced on the zero-diff baseline in `ga-aeoe12`; no runtime/tmux files are in the diff. |
| `integration-packages-core-2-of-4` | Real-Dolt orphan sweep observed a still-live process under load. The named test passes on both gate source and fork point in isolation; its package is outside the diff. |
| `cmd-gc-process-2-of-6` | Native store schema initialization exceeded its context deadline under load. The named test passes on both gate source and fork point in isolation. |
| `cmd-gc-process-3-of-6` | Two async-start subtests exceeded their five-second wait under load. The exact session-reconciler test passes on both gate source and fork point in isolation. |
| `integration-bdstore` | Dolt became ready at the test's ten-second deadline. The named test passes on both gate source and fork point in isolation. |
| `integration-review-formulas-basic-1-of-2` | `gc init` failed before formula behavior ran because an active host supervisor refused a warm refresh and the managed Dolt helper did not start. |
| `integration-review-formulas-basic-2-of-2` | Same host-supervisor/Dolt precondition failure before formula behavior ran. |
| `integration-review-formulas-retries-1-of-2` | `gc init` failed before formula behavior ran because the managed Dolt helper did not start. |
| `integration-review-formulas-retries-2-of-2` | Same host-supervisor/Dolt precondition failure before formula behavior ran. |
| `integration-review-formulas-recovery` | Same precondition failure in this run. The prior baseline investigation also passed this named test on both base and diff tip in isolation. |
| `integration-rest-smoke-2-of-2` | `TestGraphWorkflowSuccessPath` hit the known dirty shared Dolt schema on port 28231. The exact failure is present on the zero-diff baseline; the diff has no Dolt/schema changes. |
| `integration-rest-full-1-of-8` | A Dolt connection timed out under load. The named batch-delete test passes on both gate source and fork point in isolation; its file is outside the diff. |
| `integration-rest-full-8-of-8` | `TestCleanInstallTutorialPath` received legacy circuit-breaker cleanup text in command output. The exact failure is present on the zero-diff baseline in `ga-aeoe12`. |

The prior broad run's `cmd/gc` shard-1 and REST full shards 3 through 7 all passed in this run, confirming that the broad-suite red set moves with host contention and shared infrastructure rather than the feature patch.

## Diff-owned tests executed

All PASS:

- `TestApplyPatchRoutesLastWokeAtToLocalString`
- `TestSetMarkerRoutesLastWokeAtSetToLocalStringOnly`
- `TestSetMarkerRoutesLastWokeAtToLocalString`
- `TestStoreGetAndListSurviveGetLocalStringPanic`
- `TestStoreGetFallsBackToDurableLastWokeAtWhenLocalUntouched`
- `TestStoreGetProjectsLastWokeAtFromLocalString`
- `TestApplyPatchByteIdenticalToSetMetaBatch`
- `TestApplyPatchInfoPersistsAndFoldsEqualsReprojection`
- `TestGetReflectsApplyPatch`
- `TestPreWakeCommit`
- `TestPreWakeCommit_ResumeModePreservesPreviousConversationMetadata`
- `TestReconcileSessionBeads_PreWakeCommitWritesMetadata`
- `TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick`
- `TestResolveSessionTargetID_PoolPathAliasAwakeStateMatches`
- `TestSetMarkerEmptyValueClears`
- `TestSleepEmitsSleepPatch`
- `TestStoreGetSpeaksInfo`
- `TestRollbackPendingCreateClearsLocalLastWokeAtNotJustDurable`
- `TestReopenClosedConfiguredNamedSessionBeadClearsLocalLastWokeAtNotJustDurable`
- `TestQuarantineClearsLocalLastWokeAtNotJustDurable`
- `TestSyncKilledSessionAsleepClearsLocalLastWokeAtNotJustDurable`
- `TestCmdSessionKill_SyncsBeadToAsleep`

`waiver_ref: none`

## Disposition

Gate PASS. Prepare `deploy/ga-iyfdl1-gate` from exact gate source `70f3685b7087cb0aedba2a8b7c72b1ef0a03e39a`, commit this checklist, push the isolated branch, and open the pull request.
