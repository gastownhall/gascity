# Release gate: hermetic bd workspaces in tests

**Verdict:** **PASS**

Deploy bead: `ga-0nv3kr`. Reviewed source: `08a0d17302b5441ad42b0024e2039e0f85a60a57` (resolved commit). Base: `origin/main@248c0c0bc1bfc84ba791026990280940d85f20d6`. Candidate was tested as the clean synthetic merge `530ec93b1de49ab320db0288c08f48d0a9fd31df`, tree `61d926b8bf51db01b460153855563376add9b1c3`, materialized with `materialize_merge_tree`. Deploy branch: `deploy/ga-0nv3kr-gate`.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | **PASS.** Reviewer round 2 of 2 records PASS on the exact reviewed source; no review carryover was used. |
| 2 | Acceptance criteria met | **PASS.** `GuardedBdWorkspaceDir` creates a test-owned Git root below a bait `.beads` ancestor and checks for escaped writes during cleanup. Four real-bd custom-types tests use the guarded workspace. The eight changed tests pass by name in the full run. This mitigates inherited ancestor workspaces; finding the process that created the stray workspace remains tracked separately. |
| 3 | Tests pass | **PASS.** `test_cmd: make test-local-full-parallel` through `gate-detached-run.sh`, `load-gate-run.sh --threshold 15 --max-wait 1800`, and `isolated-test-run.sh`, with `GOFLAGS=-v`, `LOCAL_TEST_JOBS=4`, rootless Podman socket, Ryuk disabled, and pinned bd v1.3.1. `test_cmd_scope: full-suite`; `test_counts: 56786 PASS, 0 FAIL, 236 SKIP` terminal test lines across 40/40 passing jobs. `diff_tests_executed: 8/8 PASS`, listed below. `waiver_ref: none`. `heavy_mode: none` by classifier. Podman socket ping 200; cached `dolt-sql-server:2.1.7` matched `deps.env`. The wrapper reported pass-through because this was an unlisted synthetic scratch path; it found no inherited BD_/BEADS_/GC_/DOLT_ variables, and the Makefile shard runners scrub their environments. |
| 4 | No unresolved HIGH review findings | **PASS.** Reviewer reports no style or security blocker and zero unresolved HIGH findings. |
| 5 | Final branch clean | **PASS.** The tested synthetic merge was clean before and after the run; the isolated deploy branch started clean at the reviewed source. The gate file is committed on that branch, with no remaining worktree changes. |
| 6 | Clean divergence from main | **PASS.** `git merge-tree --write-tree origin/main <reviewed SHA>` returned 0 with tree `61d926b8bf51db01b460153855563376add9b1c3`; `origin/main` was fetched again after the suite and remained at the pinned base. No self-rebase. |
| 7 | One feature theme | **PASS.** The three source commits change only test workspace isolation in `internal/beads/beadstest` and its `internal/doctor` callers: four paths, +232/-5. No production code, dependency, CI configuration, or independent feature was added. |

## Criterion 3 evidence

- `policy_lane: PASS` — pinned-base `make lint-affected` with `LINT_CHANGED_SCOPE=tracked` and the repo's required policy checks in a fresh tree, golangci-lint 2.12.0, zero issues. The initial attempt hit a transient shared Go build-cache missing-export-data error; after `go build ./...` passed on the unchanged tree, the complete lane passed on retry. Logs: `/var/tmp/ga-0nv3kr-policy.log`, `/var/tmp/ga-0nv3kr-policy-retry.log`.
- `drift_lane: PASS` — `make bazel-sync` then `git diff --exit-code` in a separate fresh tree; no BUILD drift. Log: `/var/tmp/ga-0nv3kr-bazel-drift.log`.
- `format/build/vet: PASS` — `git diff --check`, `go build ./...`, `go vet ./...` on the canonical synthetic merge. Logs: `/var/tmp/ga-0nv3kr-build.log`, `/var/tmp/ga-0nv3kr-vet.log`.
- `ci_lane_run: n/a` — the diff changes no CI job, matrix, timeout, or required-check list.
- `load_threshold: 15`; `load_waited_seconds: 240`; `load_wait_timed_out: no`; `run_start_load: 14.44`; `run_max_load: 41.96`; `run_mean_load: 24.11`; `run_readings: 64`; `wait_first_load: 26.42`; `wait_max_load: 26.42`; `wait_mean_load: 19.77`; `wait_readings: 9`; `read_errors: 0`. The load gate opened below threshold; later load rose while the already-running suite completed successfully.
- `skip_justification: 236 SKIP lines are outside the eight diff-owned tests.` The full-suite log records platform-specific cases (Darwin tests on Linux), opt-in live and regeneration tests, harness subprocess helpers, unavailable localhost SSH/tooling fixtures, and persistence cases requiring a newer upstream bd or an explicit opt-in. Every diff-owned test executed with a real PASS, including the four doctor tests that would skip without bd or dolt on PATH.
- Full-run record: `/var/tmp/gc-heavy-gate/runs/ga-0nv3kr.c3-20261004` (key `61d926b8bf51db01b460153855563376add9b1c3-full-v1-63900a4316`); job logs: `/var/tmp/ga-0nv3kr-full-logs`.

`diff_tests_executed` (each shows a real `--- PASS` in the full-suite logs):

| Package | Test | Result |
|---|---|---|
| `internal/beads/beadstest` | `TestBaitDisturbanceNamesWhatBdLeftBehind` | PASS |
| `internal/beads/beadstest` | `TestGuardedBdWorkspaceDirIsItsOwnGitRoot` | PASS |
| `internal/beads/beadstest` | `TestGuardedBdWorkspaceDirReportsAnEscapeWhenTheTestEnds` | PASS |
| `internal/beads/beadstest` | `TestGuardedBdWorkspaceDirSitsBelowABaitWorkspace` | PASS |
| `internal/doctor` | `TestCustomTypesCheck_MissingTypes` | PASS |
| `internal/doctor` | `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` | PASS |
| `internal/doctor` | `TestCustomTypesCheck_TableDrift` | PASS |
| `internal/doctor` | `TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext` | PASS |
