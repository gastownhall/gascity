# Release gate: wake committed sessions after command changes (ga-k0bsw3)

**Verdict:** **PASS**

Reviewed source: `bc5e47f48df2d7bc93da89260565066fc0f7bced`. Base: `ded4b491745f2bba8a17736b0241915180413183` (`origin/main`). The clean, materialized merge commit `44909d3f86db06cba31682c5ac94b58cf7b9248d` has tree `5ac2645a9327ffdf5bf95c00cf7bd01c9df8e00b`, matching `git merge-tree`. Tests and policy checks ran on that merged tree. The isolated PR branch was cut at the reviewed source commit.

| # | Criterion | Evidence | Result |
|---|---|---|---|
| 1 | Review PASS | Review bead `ga-3xsue3` passed the exact reviewed source commit. | PASS |
| 2 | Acceptance | The R1 prepared-command prefix check applies to every row. The R2 unchanged-since-enqueue check applies only to wakes of committed rows; pending creates retain rollback behavior. Both new tests passed, including 13/13 command-gate table cases. All 12 tests in the unchanged upstream async drift test file passed. The supervisor log had 174 prior `desired command changed during startup` lines; the required observation across a later model bump is a post-merge check for the merge authority. | PASS |
| 3 | Tests, lint, policy | Documented full local suite: 40/40 jobs passed on the materialized merged tree, with 55,301 top-level PASS, 0 FAIL, 236 SKIP. Both diff-owned tests passed by name in the process and integration lanes. Twelve deterministic build and policy checks passed, including pinned-base policy, lint, Bazel sync, build, vet, spec, and schema checks. See details below. | PASS |
| 4 | High review findings | Review reported zero unresolved high-severity findings. Its three notes were nonblocking. | PASS |
| 5 | Final branch clean | Isolated `deploy/ga-k0bsw3-gate` contains the reviewed commit and this gate record; `git status` was clean after committing. | PASS |
| 6 | Clean merge | The reviewed source merges with pinned `origin/main` without conflict; the materialized tree matches the predicted merge tree. `origin/main` was still at the pinned SHA when the full suite completed. | PASS |
| 7 | Single feature theme | The reviewed diff changes only the async-start command gate, its tests, and Bazel test registration in `cmd/gc`. Ancestry scope passed for this bead and its named predecessor fixes; there is no `.claude` path. | PASS |

## Criterion 3 evidence

- `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=2` through `gate-base.sh run`, `load-gate-run.sh`, and `isolated-test-run.sh`; `test_cmd_scope: full-suite`; `heavy_mode: none`; `ci_lane_run: n/a (no CI configuration change)`; `waiver_ref: none`.
- `test_counts: 55,301 top-level PASS, 0 FAIL, 236 SKIP` across 40 completed jobs. Including subtests: 99,473 PASS, 0 FAIL, 335 SKIP. The two added tests ran and passed in both `cmd-gc-process` and integration `cmd/gc` shards. `diff_tests_executed: TestAsyncStartCommandGate_OnlyAChangeDuringStartupIsStale PASS (13/13 table cases); TestCommitAsyncStartResult_CommitsWhenStoredCommandUnchangedDuringStartup PASS`.
- `skip_justification:` No diff-owned test skipped. Existing skips cover platform-specific cases, subprocess helpers, optional live external environments or tool versions, and tests deliberately skipped in the fast tier but run in the process tier. None exercises the changed async-start predicate. The unchanged async drift tests ran and passed in the process tier.
- `policy_lane: PASS` on the pinned merge tree: `make check-hooks`, `make bazel-sync` with no generated diff, `make test-ci-policy`, format, `make lint`, `go build ./...`, `make vet` (`go vet ./...`), `make spec-ci`, `make check-schema`, and clean status. The deterministic detached run completed 1/1 unit with 12/12 checks passing.
- The full-suite runner executed 40 jobs with a per-job `env -i` environment and pinned Git config. `isolated-test-run.sh` reported pass-through for this scratch tree, so no extra clone was made; the suite's own job environment scrubbed ambient variables. Rootless Podman socket and pinned Dolt image were checked before the run; the suite's real Dolt lifecycle cases used the pinned test `bd` binary.
- Full-suite load gate: `load_threshold: 15`, `load_waited_seconds: 30`, `load_wait_timed_out: 0`, `run_start_load: 14.75`, `run_max_load: 23.82`, `run_mean_load: 17.69`, `run_readings: 110`. Deterministic lane: waited 0 seconds, no timeout, start 14.40, max 21.14, mean 17.68, 26 readings.
- Full-suite detached run: `/var/tmp/gc-heavy-gate/runs/ga-k0bsw3.ded4-suite`, state complete, 1/1 unit, zero failed. Deterministic run: `/var/tmp/gc-heavy-gate/runs/ga-k0bsw3.ded4-deterministic`, state complete, 1/1 unit, zero failed. Shard logs and scripts: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-k0bsw3/20261002T0626Z`.
