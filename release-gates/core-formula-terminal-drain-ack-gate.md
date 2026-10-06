# Core formula terminal drain acknowledgements — ga-corsg5

**Verdict:** **PASS**

- Reviewed source: `ead24455000a22a66711b90e038f640feb186ebb`
- gate_base: `7b0a606535cd98514c667a7672fd39721ac4174a`
- Merge candidate: `c1800ad1dec81ca1e5a80df8c97c6291dbea3164`
- Latest checked `origin/main`: `6f5f6fec4aa4ba27c381ef84ccbb3898d21493e3`; the reviewed source still merges cleanly (`git merge-tree --write-tree`). The full suite ran on the pinned merge candidate above.
- waiver_ref: none (gascity has no local waiver path)

| # | Verdict | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Review bead `ga-w69ty4` passed on the exact reviewed source SHA. No review carryover was needed. |
| 2. Acceptance criteria | PASS | All four core formula drain acknowledgements now guard and close the claimed terminal step first. The push-conflict path records a hard failure and leaves the work bead open. The new cmd/gc harness covers the three affected terminal steps, the already-correct mol-do-work control, push conflict, and fail-closed guards. The reviewer reproduced RED before the formula changes and independently executed 61 guard checks. |
| 3. Tests pass | PASS | The documented full `make test-local-full-parallel` completed on the merge candidate through `load-gate-run.sh` and `isolated-test-run.sh`: all 40 jobs PASS, 0 FAIL, 0 SKIP at job level; `GATE_RUN_EXIT rc=0 state=complete`. The runner emits package results and selected names, not aggregate per-test PASS/SKIP events. All three diff-owned tests were selected in both process and integration cmd/gc shards, whose jobs passed. A separate JSON result on the same candidate confirmed 13 PASS test/subtest events, 0 FAIL, 0 SKIP for those names. No waiver. |
| 3a. Failure attribution | PASS | The completed run had no failure to attribute. |
| 3b. Policy/lint and drift | PASS | On fresh views of the same merge commit: `make fmt-check` PASS; `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary` PASS; CI `make lint-affected` with tracked diff against gate base PASS, 0 issues, pinned golangci-lint 2.12.0 (`GLT_RECORD` mismatch=false); `make bazel-sync` followed by `git diff --exit-code` PASS. Fresh-tree and gate-base records are in `/var/tmp/ga-corsg5-{fmt,policy,lint-affected,drift}.log`. |
| 3c. CI-config lane | PASS | No CI job, matrix, timeout, or required-check configuration changed. The BUILD.bazel change registers the new test source and passed the sync check. |
| 3d. Heavy package | PASS | `heavy-composite-gate.sh classify` reported `HEAVY_MODE mode=none shard_required=- watch=-`. |
| 4. Review findings | PASS | Reviewer recorded no style or security findings and no unresolved high-severity findings. |
| 5. Final branch clean | PASS | The isolated deploy branch has no uncommitted changes after this gate record is committed; verified before push. |
| 6. Clean merge and integration build | PASS | `materialize_merge_tree` produced the clean two-parent merge above from the reviewed source onto the gate base. `go build ./...` and `go vet ./...` both passed there; `git diff --check` and scratch status were clean. After main advanced, `git merge-tree --write-tree origin/main <reviewed source>` still succeeded against the latest main checked above. |
| 7. Single feature theme | PASS | The diff is the core formula terminal-step drain-ack fix plus its cmd/gc harness and BUILD registration. |

test_cmd: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env LOCAL_TEST_LOG_DIR=/var/tmp/ga-corsg5-suite-shards make test-local-full-parallel LOCAL_TEST_JOBS=4`

test_cmd_scope: full-suite

test_counts: 40 PASS, 0 FAIL, 0 SKIP at job level. The full-suite runner does not emit aggregate per-test PASS/SKIP counts; the diff-owned JSON run below did.

diff_tests_executed: `TestCoreDrainAckViolationsRecognizesTheFailClosedClose` PASS; `TestCoreFormulaDrainAckStepsCloseTheirOwnStepFirst` PASS; `TestMolPolecatCommitPushConflictFinalizesFailed` PASS. Each was selected in two passing full-suite shards. The separate `go test -json -count=1 -run '^(TestCoreDrainAckViolationsRecognizesTheFailClosedClose|TestCoreFormulaDrainAckStepsCloseTheirOwnStepFirst|TestMolPolecatCommitPushConflictFinalizesFailed)$' ./cmd/gc` run recorded 13 PASS test/subtest events, 0 FAIL, 0 SKIP (`/var/tmp/ga-corsg5-diff-owned.jsonl`).

ci_lane_run: n/a (no CI-config change)

heavy_mode: none

load_threshold: 15; load_waited_seconds: 1800; load_wait_timed_out: 1; run_start_load: 18.63; run_max_load: 41.21; run_mean_load: 27.48; run_readings: 106. The ordinary gate proceeded after its bounded load wait. The successful detached run is `/var/tmp/gc-heavy-gate/runs/ga-corsg5.c3`; final output is `/var/tmp/ga-corsg5-suite-output.log`.
