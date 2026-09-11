# Release gate: symlink-safe city-root containment

- Deploy bead: `ga-bp4zyv`
- Source review: `ga-bnd1fs`
- Reviewed commit: `026f11a4131964d24c33c6cb5c65d5f785441bf1`
- Reviewed base: `4a636f6ad88002556c6c0891b7b9e07f9502c81c`
- Main evaluated: `origin/main@9a88d149cd5c3fb1054f75f8d540fd2aefa465e1`
- Deploy branch: `deploy/ga-bp4zyv-gate`
- Evaluated: `2026-07-30T04:36:26Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this checklist applies the deployer role's release-gate criteria.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first and rechecked after tests. `git merge-tree --write-tree origin/main 026f11a4131964d24c33c6cb5c65d5f785441bf1` exited 0 against `origin/main@9a88d149cd5c3fb1054f75f8d540fd2aefa465e1` and produced tree `1999b4e91bb28274c16f8a7fd8082aa38a6f6220`. No self-rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | The deploy bead records a reviewed-and-passed verdict for exact commit `026f11a4131964d24c33c6cb5c65d5f785441bf1`. The source review reports no style, security, or correctness findings. |
| 2 | Acceptance criteria met | **PASS** | The focused containment suite passed 12 tests, 0 failed, 0 skipped. It covers both valid symlinked-city forms, six `controllerState.CreateRig` behaviors, and four git-provision rejection paths, including relative escapes, absolute client paths, and symlinked-parent escapes. The implementation normalizes both lexical operands while leaving the independent symlink-aware containment pass unchanged. |
| 3 | Tests pass | **PASS** | On the exact reviewed SHA: `go build ./...`, `go vet ./...`, and `gofmt -l` passed; `make test-fast-parallel` passed 10/10 jobs (0 fail, 0 skip); the documented non-short `cmd/gc` coverage inside `make test-local-full-parallel` passed all six process shards plus the product-metrics testhook (7/7 jobs, 0 fail, 0 skip); `make test-acceptance` passed the Tier A package (0 fail; five tag-empty packages reported no tests to run); and `make test-worker-core-phase2-all` passed 3/3 package invocations (0 fail, 0 skip). The broad 40-job local sweep also exposed host-only failures outside the changed files: the host `bd` binary differed from the verified CI archive despite sharing its version string, tmux 3.7b returned no builtin key bindings, and Dolt 2.2.1 rejected dirty migration fixtures that CI runs under Dolt 2.1.7. The CI-archive rerun cleared the `bdflags` and formula-retry failures; serial reruns cleared the readiness, live-contract, and cleanup races (4 top-level PASS, 0 FAIL, 5 fixture-required subtest SKIPs). The two remaining host-tool failures are in unchanged tmux/recovery code, and the exact merge-base CI run `30496938823` passed every corresponding required lane. |
| 4 | No high-severity review findings open | **PASS** | The reviewer reports no security findings and no blocking findings. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Detached reviewed commit `026f11a41` had an empty `git status --porcelain=v1` before this checklist was added; `git diff --check` against the reviewed base passed. The configured hook path is `.githooks`; this checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | The two-commit TDD diff changes only `cmd/gc/api_state.go` and its focused test (+90/-2). Both commits address one behavior: API rig creation when the city root is reached through a symlink. |

## Review notes

- `assertRigPathWithinCity` reuses `pathutil.NormalizePathForCompare` on the
  city root and target before the lexical containment check.
- The second, symlink-aware `EvalSymlinks`/`realPathForContainment` pass is
  unchanged, so escaping paths still have to pass both containment checks.
- Local `gc rig add` behavior, API wire shapes, configuration, and storage
  migrations are unchanged.

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 026f11a4131964d24c33c6cb5c65d5f785441bf1
git diff --check 4a636f6ad88002556c6c0891b7b9e07f9502c81c..026f11a4131964d24c33c6cb5c65d5f785441bf1
gofmt -l cmd/gc/api_state.go cmd/gc/api_state_rig_path_symlink_test.go
go test -count=1 -v ./cmd/gc -run '^(TestAssertRigPathWithinCityRejectsResolvedTargetUnderRawCity|TestAssertRigPathWithinCityAcceptsWhenBothSidesResolved|TestProvisionRigFromGitRejectsPreexistingPath|TestProvisionRigFromGitRejectsEscapingRelativePath|TestProvisionRigFromGitRejectsAbsoluteClientPath|TestProvisionRigFromGitRejectsSymlinkedParent|TestControllerStateCreateRigPokesReconciler|TestControllerStateCreateRigRejectsDuplicateName|TestControllerStateCreateRigDetectsDefaultBranch|TestControllerStateCreateRigRejectsOutOfCityPath|TestControllerStateCreateRigDetectsDefaultBranchForRelativePath|TestControllerStateCreateRigInitializesStoreBeforePublishing)$'
go build ./...
go vet ./...
make test-local-full-parallel
PATH="<verified-ci-bd-v1.1.0>:$PATH" make test-fast-parallel
PATH="<verified-ci-bd-v1.1.0>:$PATH" make test-acceptance
PATH="<verified-ci-bd-v1.1.0>:$PATH" make test-worker-core-phase2-all
```
