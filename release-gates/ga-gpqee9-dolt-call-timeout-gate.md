**Verdict:** **PASS**

# Real-Dolt compact helper deadline release gate

Deploy bead `ga-gpqee9`; reviewed source `d9cdd2ba948d829d543f2530111a5fe8ce8b548f` (review `ga-orhaqs`). The gate ran on the clean materialized merge `62052e4a67327b9509c05b55975c6c954e937301`, whose first parent is the frozen `gate_base: 52c4e34c2de4a8db02f37f7ebfa51174f8f70372`. The later `origin/main` tip `7b0a606535cd98514c667a7672fd39721ac4174a` also merges cleanly with the reviewed source (tree `e9d6aaa641aa2c51ffa0769bd1d37fb62bb94db9`); its intervening change does not touch the two reviewed files. Mode: remote; push remote: fork. No self-rebase or waiver.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | `ga-orhaqs` records reviewer PASS on the exact source SHA. |
| 2 | PASS | The two integration-tagged test files replace fixed per-call Dolt CLI timeouts with a bound derived from the test deadline, preserve time for reporting and cleanup, and set `exec.Cmd.WaitDelay` for inherited output pipes. The two new test bodies passed by name in the full suite. The reviewer’s independent 35-second stall A/B reproduced `signal: killed` before the fix and passed after it. |
| 3 | PASS, with attributed failures | The documented full 40-job suite ran to completion. Raw output: 101625 PASS, 4 FAIL result lines, 335 SKIP; 38 jobs succeeded and 2 failed. The four lines represent two top-level tests, neither diff-owned. Both changed tests passed by name. The four-clause attribution and skip accounting are below; no raw failure is hidden. `waiver_ref: none` (Gas City has no waiver path). |
| 4 | PASS | Exact-source review records no unresolved high-severity security, style, or specification finding. |
| 5 | PASS | The role worktree and materialized merge scratch were clean before this gate record was written. Final branch cleanliness is checked after committing the record. |
| 6 | PASS | Materialized merge, `go build ./...`, and `go vet ./...` passed on the frozen base; the current-base merge-tree check also returned clean. |
| 7 | PASS | The reviewed diff contains only `examples/bd/dolt/compact_real_dolt_test.go` and its new companion test file. All three source commits cite build bead `ga-db2c94`; the ancestry guard accepted this lineage and found no forbidden path or second theme. |

## Criterion 3 evidence

- `test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/gc-gpqee9-full-logs TMPDIR=/var/tmp/gotmp make test-local-full-parallel`
- `test_cmd_scope: full-suite`; `heavy_mode: none`; detached run `/var/tmp/gc-heavy-gate/runs/ga-gpqee9.c3`; `GATE_RUN_EXIT rc=2` due to the two attributed jobs. The suite started all 40 jobs and finished; it did not short-circuit on a failure.
- `test_counts: 101625 PASS, 4 FAIL, 335 SKIP`, counting terminal test and subtest result lines across the 40 shard logs. The changed `examples/bd/dolt` package itself reported 499 PASS, 0 FAIL, 0 SKIP in `integration-packages-core-1-of-4.log`.
- `diff_tests_executed: TestDoltCallContextIsBoundedByTheTestDeadline PASS` (all four subtests PASS); `TestCompactScriptRunnerDoesNotBlockOnPipeHeldByLeakedChild PASS`. No diff-owned test skipped or failed. `diff-owned-tests.py --explain` reports exactly these two owned test bodies.
- `skip_justification: 335 SKIP lines are outside the changed package; that package has zero skips. The diff changes only test files in that package, so existing conditional and host-specific skips in other packages cannot mask execution of the changed tests.`
- `load_threshold: 15`; `load_waited_seconds: 1801`; `load_wait_timed_out: 1`; `run_start_load: 16.66`; `run_max_load: 46.72`; `run_mean_load: 30.82`; `run_readings: 92`. Wait-only readings: first 42.01, max 46.29, mean 27.27, 61 readings. The ordinary gate proceeds after a bounded load wait; the timeout itself is not a failure.
- `policy_lane: PASS` on the frozen merge via `fresh-tree-run.sh`, `gate-base.sh run`, and pinned lint 2.12.0: `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads lint-affected fmt-check-changed check-docs`, zero lint issues. Detached record `/var/tmp/gc-heavy-gate/runs/ga-gpqee9.policy` ended rc 0.
- `drift_lane: PASS`: fresh-tree `make bazel-sync` followed by `git diff --exit-code`, zero modified or untracked files; log `/var/tmp/gc-gpqee9-drift.log`.
- `ci_lane_run: n/a`; the reviewed diff changes no CI job, matrix, timeout, or required-check list.
- `failure_attribution: TestMaintenanceOrdersOnRealBdTopologies -> ga-vkhfnj (mechanism proof 3a); TestCmdStopForceDelegatesImmediateControllerStop -> ga-x46g3m (mechanism proof 3a)`.
- `inconclusive-guard: not needed; mechanism proof landed for both failures. The diff changes no production code, resource census, or suite test target.`

### Attributed failures

1. `TestMaintenanceOrdersOnRealBdTopologies` in `examples/gastown`, `integration-packages-core-2-of-4.log:309-319` → open tracker `ga-vkhfnj`. Its direct-server subtest hit a MySQL read timeout/invalid connection; the mixed topology hit the pinned bd's 10-second `dolt version` probe SIGKILL. Clause 1: neither test body is in the diff. Clause 2: the tracker predates this run and covers full-suite Dolt/host contention; this sighting was appended to it. Clause 3(a): the fixture runs its own shell gc router and bd; it cannot execute the changed integration test helpers in `examples/bd/dolt`. Clause 4: no path overlap. The probe signature is a repeat, but this deploy's build `ga-db2c94` is stamped `gc.fixes_tracker=ga-vkhfnj` and the designated probe fix `ga-z4jzmd` is still unlanded (`gc.work_outcome=blocked`), satisfying the fix-carrying exception. The direct-server timeout is recorded separately in the same sighting.
2. `TestCmdStopForceDelegatesImmediateControllerStop` in `cmd/gc`, `integration-packages-cmd-gc-3-of-6.log:4668-4673` → open tracker `ga-x46g3m`. The test timed out waiting five seconds for a standalone controller stop under host load. Clause 1: its body is unchanged. Clause 2: the tracker was created during this discovering run and read back. Clause 3(a): the changed files are integration-tagged `_test.go` files in a different package, so the cmd/gc test binary cannot execute them; this landed mechanism proof permits the same-round tracker. Clause 4: no path overlap. The older closed `ga-ob7cd5` addressed a different two-second delegated-signal wait and recorded a later recurrence; it is not used as this run's open tracker.

Raw suite logs: `/var/tmp/gc-gpqee9-full-logs/`. Frozen merge scratch: `/var/tmp/gc-merge-validate.ga-gpqee9.ba5yX0`. Neither attribution replaces the full-suite run or the named diff-owned results.
