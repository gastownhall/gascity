# Release gate: isolate Dolt test identity from `t.TempDir` cleanup

- Deploy bead: `ga-pfdabs`
- Build bead: `ga-7dgcg6`
- Review bead: `ga-0gqma7`
- Reviewed commit: `25148bc121317fb357d84f43fbd53eabdca64f6e`
- Gate base: `origin/main` at `29b36facde4ffe557b6fb5b99c7375468600b606`
- Evaluated: 2026-07-31
- Result: **PASS**

Criterion 6 was evaluated first, as required. The remaining criteria were then
evaluated in numeric order. `docs/PROJECT_MANIFEST.md` is absent from both the
reviewed commit and current `origin/main`; this checklist therefore applies the
deployer gate criteria and
`engdocs/contributors/release-gate-criteria-conventions.md` directly.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-0gqma7` is closed with reason `pass`; its notes record `verdict: pass` and pin deploy commit `25148bc121317fb357d84f43fbd53eabdca64f6e`. |
| 2 | Acceptance criteria met | **PASS** | The reviewed diff adds `doltIdentityHomeDir`, places Dolt/Git identity files outside every `t.TempDir` tree, redirects `configureTestDoltIdentityEnv` to it, and widens the leak guard to `cityPath`, `feRepoDir`, and the identity home. The regression test fails on RED commit `296cc5920` and passes on the reviewed commit. `GC_FAST_UNIT=0 go test ./cmd/gc/ -run '^TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore$' -count=3` passed all 3 repetitions. `gofmt -l` returned no files. The reviewer also verified the remaining shared-helper call sites and `go vet ./...`. |
| 3 | Tests pass | **PASS** | Required target `make test-cmd-gc-process-parallel` was run in detached worktrees at merge-base `4a636f6ad88002556c6c0891b7b9e07f9502c81c` and reviewed commit `25148bc121317fb357d84f43fbd53eabdca64f6e`. Both sides produced **4 PASS jobs, 3 FAIL jobs, 0 SKIP jobs** and the identical failing set: `TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork`, `TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork`, and `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession`. Shards 4-6 and `productmetrics-testhook` passed on both sides. The pre-push `make test-fast-parallel` run likewise produced **9 PASS jobs, 1 FAIL job, 0 SKIP jobs**; its sole failure, `TestCustomTypesCheck_TableDrift`, was reproduced at both the merge-base and reviewed SHA with the identical missing-`tst` error. These failures are the known ambient-HOME Dolt leak (`ga-zxpfic`): real `bd` is redirected to fleet server `127.0.0.1:3308`, where temporary databases are absent. Both differentials therefore show **0 change-introduced regressions**; the environment fix is tracked by `ga-8pkpor`. The shard wrappers do not emit exact per-test PASS/SKIP counts for red shards, so no unsupported aggregate is claimed. Process-suite logs: `/var/tmp/gc-local-tests.h62qwY` (merge-base) and `/var/tmp/gc-local-tests.lCATVj` (reviewed); pre-push log: `/var/tmp/gc-local-tests.ZRvOxj`; focused doctor logs: `/var/tmp/gc-ga-pfdabs-diff.e7RVAA/{base,reviewed}.doctor.log`. |
| 4 | No high-severity review findings open | **PASS** | Review notes record no style, security, or specification findings and no unresolved HIGH findings. |
| 5 | Final branch is clean | **PASS** | The isolated gate worktree was clean at gate commit parent `25148bc121317fb357d84f43fbd53eabdca64f6e` before this checklist was amended; the checklist is the only gate-commit delta. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main`, `git merge-tree --write-tree origin/main 25148bc121317fb357d84f43fbd53eabdca64f6e` exited 0 and produced tree `96f5fcbae551d89a868720d2f18e93de9ef47078`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | The two-commit change touches only `cmd/gc/testenv_test.go` and `cmd/gc/cmd_bd_test.go`, both within the Dolt-backed `cmd/gc` test-environment cleanup theme. |

## Gate decision

The reviewed change introduces no process-suite regression relative to its
merge-base, satisfies its focused RED/GREEN acceptance evidence, and remains
conflict-free with current `origin/main`. It is eligible for an isolated deploy
branch and pull request.
