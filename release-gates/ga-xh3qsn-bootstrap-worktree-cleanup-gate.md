# Release gate: bootstrap worktree cleanup (`ga-xh3qsn`)

- Verdict: **PASS**
- Reviewed deploy source: `be7a507c2f4e9788355bb5b43ecbf9f9c8bfaff0`
- Base evaluated: `origin/main@e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`
- Deploy mode: `remote` (`origin = https://github.com/gastownhall/gascity.git`)
- Evaluation date: 2026-08-18
- Review bead: `ga-kgt3jv`
- Build bead: `ga-x1u5cr`

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this gate
uses the release criteria embedded in `mol-deployer-gate` and the deployer
prompt. The pre-flight query found no pull request associated with the reviewed
commit; the normal gate path therefore applies.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-kgt3jv` is closed with `verdict: pass` on the exact reviewed commit, 34 PASS / 0 FAIL / 0 SKIP in its scoped review run, and no style, security, or acceptance findings. |
| 2 | Acceptance criteria met | **PASS** | Independent diff inspection confirms both bootstrap formulas resolve the repository through `--git-common-dir`, anchor removal and pruning with `git -C "$REPO"`, and retain `rm -rf` plus `worktree prune` only as the removal-failure fallback. `mol-scoped-work` resolves the repository from `$WORKTREE`, where cwd is not guaranteed. Both regression tests pass by name. |
| 3 | Tests pass | **PASS** | The documented complete local matrix, `LOCAL_TEST_JOBS=2 GO_TEST_TIMEOUT=30m make test-local-full-parallel`, scheduled all 40 jobs and completed **36 PASS / 4 FAIL / 0 SKIP jobs**. The five failing test names are all non-diff-owned, have no path overlap, have a tracker, and either reproduced by exact name and signature on the exact base or carry the narrow prior mayor attribution described below. Under criterion 3a they are attributed pre-existing failures, so criterion 3 passes without retrying a product failure into green. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent`, both tmux default-binding tests, and `TestE2E_SuspendResume_City` reproduced by exact name and signature on `origin/main`. `TestCleanInstallTutorialPath` passed on base, but its candidate failure is the exact circuit-breaker stdout signature that mayor ruling `gm-wisp-g91bf7` authorizes attributing to `ga-rsktma` because the trigger is machine state. Full per-test evidence is below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed; `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` passed with `0 issues` using a fresh on-disk golangci cache; `make fmt-check-changed`, `go vet ./...`, and `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no style or security findings and no uncovered acceptance criteria. No HIGH finding remains open. |
| 5 | Final branch is clean | **PASS** | The exact reviewed-source checkout was clean before this gate record was written. This checklist is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main be7a507c2f4e9788355bb5b43ecbf9f9c8bfaff0` exited 0 and produced tree `b1211018a9cfcdf69fcd62b8a366bcc1c1372b79`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The reviewed commit range changes only the bootstrap pack worktree-cleanup snippets and their colocated regression tests: three files under `internal/bootstrap/packs/core`. |

## Criterion 3 evidence

Environment established before the run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- rootless Podman 5.8.4 reachable
- repository-pinned Dolt image tag `2.1.7` present in the local image cache

Test evidence:

- `test_cmd`: `LOCAL_TEST_JOBS=2 GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_counts`: **36 PASS / 4 FAIL / 0 SKIP jobs**; the four red jobs contain five failing test names
- candidate logs: `/var/tmp/gc-local-tests.nh4NhS`
- base focused logs: `/var/tmp/ga-xh3qsn-base-focused.DuJnk7`
- static/policy logs: `/var/tmp/ga-xh3qsn-static.POg6Wk`
- `skip_justification`: none needed (0 skipped jobs)
- `waiver_ref`: none
- failure-attribution authority for the one state-dependent signature: mayor message `gm-wisp-g91bf7`
- `diff_tests_executed`:
  - `TestMolPolecatCommitResolvesRepoBeforeRemovingWorktree`: **PASS**
  - `TestMolScopedWorkResolvesRepoBeforeRemovingWorktree`: **PASS**

The diff-owned test command was:

```text
go test -count=1 -v ./internal/bootstrap/packs/core -run '^(TestMolPolecatCommitResolvesRepoBeforeRemovingWorktree|TestMolScopedWorkResolvesRepoBeforeRemovingWorktree)$'
```

### Failure attribution

All five failures are outside the three-file diff and have no package/path
overlap with `internal/bootstrap/packs/core`:

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + origin/main exact-name repro FAIL`
  - Candidate and base both report the manifest missing flags exposed by the
    installed `bd` binary, including `--cpu-profile`, `--database`,
    `--mem-profile`, and `--no-color`.
- `failure_attribution: TestGetKeyBinding_CapturesDefaultBinding -> ga-afqddr + origin/main exact-name repro FAIL`
  - Candidate and base both capture an empty binding where the host tmux test
    expects `next-window`.
- `failure_attribution: TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-k3fxvj + origin/main exact-name repro FAIL`
  - Candidate and base both capture an empty binding where the host tmux test
    expects `choose-tree`.
- `failure_attribution: TestE2E_SuspendResume_City -> ga-yc0e3a + origin/main exact-name repro FAIL`
  - Candidate timed out after 94.18 seconds waiting for `citysus.report`; exact
    base timed out after 94.08 seconds waiting for the same report.
- `failure_attribution: TestCleanInstallTutorialPath -> ga-rsktma + mayor ruling gm-wisp-g91bf7`
  - Candidate received a circuit-breaker cleanup line on stdout before the
    expected issue prefix. Exact base passed after the triggering legacy file
    had been consumed. The ruling explicitly authorizes attribution for this
    signature because its trigger is machine state and does not authorize any
    broader waiver.

## Disposition

Gate PASS. Cut `deploy/ga-xh3qsn-gate` from the exact reviewed source, commit
this checklist there, and open a pull request. Merge authority remains with the
mayor/mpr; the deployer does not merge.
