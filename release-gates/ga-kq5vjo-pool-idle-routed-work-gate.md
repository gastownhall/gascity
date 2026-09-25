# Release Gate: pool idle capacity with routed work

- Deploy bead: `ga-kq5vjo`
- Build bead: `ga-3ex7s2`
- Review bead: `ga-tiqffu`
- Reviewed source: `51808fa41f48fbf7d0ca10ea7b80858b888b3d22`
- Base evaluated: `origin/main@aafe756142e70a54995b412de4c0adfad984fe9a`
- Deploy mode: remote
- Overall verdict: **PASS WITH ATTRIBUTED FAILURES**

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first. The full local CI union reported red
jobs, but every failure is preserved below as **FAIL — WAIVED** with a specific
tracker and a mechanism showing that the doctor-only diff cannot cause it.
Mayor explicitly approved proceeding in the deploy bead after independently
checking the diff and the focused coverage evidence.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-tiqffu` is closed with a PASS verdict for exact source `51808fa41f48fbf7d0ca10ea7b80858b888b3d22`; unresolved HIGH findings: 0. |
| 2 | Acceptance criteria met | PASS | The new `pool-idle-routed-work` doctor check warns only when a pool has both a live idle instance and open unclaimed work routed to the pool. It scans city and eligible rig stores, ignores claimed work and asleep/drained capacity, is registered in the doctor golden, and is detection-only (`CanFix=false`, no-op `Fix`). The nine scenario tests and the golden registration test pass. |
| 3 | Tests pass | PASS WITH WAIVERS | `make test-local-full-parallel` ran the documented 40-job CI union with rootless Podman configured: **28 PASS jobs, 12 FAIL jobs, 0 SKIP jobs**. All 12 failures remain recorded as FAIL — WAIVED below. Focused diff-owned run: **9 PASS, 0 FAIL, 0 SKIP**; golden registration: PASS. `diff_tests_executed` is listed below. `waiver_ref`: mayor's explicit proceed ruling in `ga-kq5vjo`; standing beads#4566 authorization in `ga-lpfjhc`. Full log: `/var/tmp/ga-kq5vjo-test-local-full.log`; job logs: `/var/tmp/gc-local-tests.Wa1cfz`. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`: PASS (5 runner-policy tests, 15 suite-coverage tests, `scripts/cipolicy`, and the four static-scope tests). `go vet ./...`: PASS. `make lint-new`: PASS, 0 issues. |
| 4 | No high-severity review findings open | PASS | Review recorded no style, security, or specification blocker and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | Exact reviewed source was checked out detached; `git status --short` was empty before this checklist was written. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 51808fa41f48fbf7d0ca10ea7b80858b888b3d22` exited 0 and produced tree `68a4a63e5dfe1a359a3e6fc77932c1a20970dbac`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Five changed files, all in `cmd/gc`, implement and test one read-only doctor check for idle pool capacity beside unclaimed routed work. `assert_deploy_ancestry_scope` passed for `ga-kq5vjo` / `ga-3ex7s2`. |

## Diff-owned test execution

Command:

```text
GC_FAST_UNIT=0 go test ./cmd/gc/ -run '^(TestPoolIdleRoutedWorkCheck.*|TestBuildDoctorChecks_NameSetUnchanged)$' -v -count=1
```

Results:

- `TestPoolIdleRoutedWorkCheckWarnsOnIdleInstanceWithUnclaimedRoutedWork` — PASS
- `TestPoolIdleRoutedWorkCheckOKWhenNoUnclaimedRoutedWork` — PASS
- `TestPoolIdleRoutedWorkCheckOKWhenNoIdleInstance` — PASS
- `TestPoolIdleRoutedWorkCheckOKWhenOnlyInstanceIsAsleep` — PASS
- `TestPoolIdleRoutedWorkCheckIgnoresClaimedRoutedWork` — PASS
- `TestPoolIdleRoutedWorkCheckScansRigScopes` — PASS
- `TestPoolIdleRoutedWorkCheckWarnsOnSkippedStoreScopes` — PASS
- `TestPoolIdleRoutedWorkCheckCanFix` — PASS
- `TestPoolIdleRoutedWorkCheckFixIsNoop` — PASS
- `TestBuildDoctorChecks_NameSetUnchanged` — PASS (modified golden registration)

## Failure attribution

Each occurrence was logged on its tracker and verified before this gate was
signed off.

- **FAIL — WAIVED:** `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestCleanInstallTutorialPath`, `TestHumaBinary_SessionMessageAsync`, `TestGCLiveContract_BeadsAndEvents`, and `TestGraphWorkflowFailureRunsCleanup` → `ga-lpfjhc`. Each failed during fixture-store initialization with the exact `gastownhall/beads#4566` dirty-table migration signature, before doctor code was reachable. No changed path overlaps Dolt schema migration or store bootstrap.
- **FAIL — WAIVED:** `TestBdFlagManifestCurrent` → `ga-f0uceo`. The test compares `internal/bdflags` with the independently installed host `bd`; this `cmd/gc` doctor diff cannot alter either input and has no path overlap.
- **FAIL — WAIVED:** `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` → `ga-vbyn8v` (reviewed fix `ga-8c4rc5`, deploy `ga-63rfxj`). The failing `examples/gastown`/`internal/doltorphan` path is a separate test binary and subsystem from the doctor diff.
- **FAIL — WAIVED:** `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → `ga-afqddr`. Both are the tracked tmux 3.7b empty-default-binding signature in `internal/runtime/tmux`, a separate package and subsystem.
- **FAIL — WAIVED:** `TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning` → `ga-xbhkcx`. It timed out only in the 40-job run and passed focused. A focused coverage run reported **0.0%** for every changed doctor production function, proving the changed code does not execute in this stop path.
- **FAIL — WAIVED:** `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` → `ga-24e5ii`. Native schema open exceeded its deadline only in the 40-job run and passed focused. The same coverage run reported **0.0%** for every changed doctor production function, proving the changed code does not execute in this schema-open path.

Focused mechanism evidence:

```text
GC_FAST_UNIT=0 go test ./cmd/gc/ \
  -run '^(TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning|TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore)$' \
  -v -count=1 -coverprofile=/var/tmp/ga-kq5vjo-unrelated.cover
```

Both tests passed. Log: `/var/tmp/ga-kq5vjo-unrelated-focused.log`.
