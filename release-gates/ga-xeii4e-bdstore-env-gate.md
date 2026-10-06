# Release gate: isolate BdStore integration subprocess environments

**Verdict:** **PASS**

- Deploy bead: `ga-xeii4e`; reviewed build bead: `ga-vsyfjo`; review bead: `ga-ukl64o`.
- Reviewed source: `de5e425a0877eac932fb988ee8b26ed06df34a8c` on `origin/builder/ga-vsyfjo`; source base `ba938a50f5db2b4f21068a9fc1a20d7c0222de7d`.
- `gate_base: 8054ba17303e6d5172409b91133da594f2ac9b8d`.
- Materialized merge: `38fe4c983b153df2a3813e835ac76f051b05f1c1` (tree `b4284a7fe37f42c205b70b502a87f7f7d1c561ea`).
- Deploy mode: remote; push remote: `origin`; deploy branch: `deploy/ga-xeii4e-gate`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ukl64o` records round-2 PASS on the exact source SHA, following resolved round-1 findings. |
| 2 | Acceptance criteria met | **PASS** | The real `bd` runner passes the test-supplied environment to pinned `bd`, including HOME rebasing to GC_HOME, suppresses inherited Beads/Dolt routing variables, preserves JSON stdout and literal-`bd` production fallback and timeout behavior. The dedicated `bdstore` shard selects the new regressions and affected real-store tests. Reviewer independently verified env-leak, fallback, and HOME mutants. Production code is unchanged. |
| 3 | Tests pass | **PASS** | `test_cmd: make test-local-full-parallel` via the bounded load gate and isolation wrapper on the materialized merge, with `GOFLAGS=-v`, `LOCAL_TEST_JOBS=8`, private on-disk `GOCACHE`/short `TMPDIR`/`GOTMPDIR`, and rootless Podman; `test_cmd_scope: full-suite`; `test_counts: 57,426 PASS, 0 FAIL, 235 SKIP` across 40/40 successful jobs; `diff_tests_executed: six PASS by name, zero SKIP/FAIL` (listed below); `waiver_ref: n/a`; retained logs: `/var/tmp/ga-xeii4e-full-logs-c6`. |
| 3b | Policy/lint and generated-file drift | **PASS** | Fresh-tree `make lint-affected` and `make fmt-check-changed` passed against the pinned gate base (pinned golangci-lint 2.12.0, zero issues). Policy guards `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary check-docs check-routed-test-rows check-split-topology-rows check-residency-boundary check-release-dist-ignore` passed. `make bazel-sync` followed by `git diff --exit-code` passed in a fresh tree, with no generated drift. |
| 3c | CI-config lane run | **PASS** | `ci_lane_run: n/a` — the source diff does not change a CI job, matrix, timeout, or required-check list. |
| 3d | Heavy package path | **PASS** | `heavy_mode: none` — `heavy-composite-gate.sh classify` selected no heavy package. |
| 4 | No high-severity review findings open | **PASS** | Round-2 review has no blocker, major, minor, security, or specification finding. `ga-rqgzxc` and `ga-ma1lyw` are follow-ups after this change lands. |
| 5 | Final branch is clean | **PASS** | `make check-hooks` passed before branch cut and before commit; source `git diff --check` passed; deploy branch was cut at the exact reviewed SHA. The active pre-commit hook runs on this staged gate record, and clean worktree status is verified after commit. |
| 6 | Branch diverges cleanly from main | **PASS** | Reviewed source merged without conflict into pinned gate base, and `go build ./...` plus `go vet -p=4 ./...` passed on that materialized merge. `origin/main` advanced to `ed015759721683206a9ddaa3ad3600ebf3cddcc2`; its merge is conflict-free (commit `5b135a79c24e97e860ab7353605293d798757f18`, tree `236d1d193e23057b4ef79a2172c779d6c789fb51`). Clean-environment build and vet passed there; focused integration check: PASS on seven named tests: four BdStore regressions, `TestIntegrationEnvForIsolatesHome`, `TestIsolatedEnvRootsKeepGCOffTheRealHome`, and `TestStartDrift_RestartTimeout_ExitsNonZero` (clean pass-through wrapper, pinned bd v1.3.1, `/var/tmp/ga-xeii4e-current-merge-check-clean.log`). Latest `origin/main` remained at `ed015759721683206a9ddaa3ad3600ebf3cddcc2` before commit. |
| 7 | Single feature theme | **PASS** | The source range is 8 files, +180/-5, all integration-test runner environment isolation, shard placement, and test fixtures. |

## Test evidence and limits

- Detached run: `/var/tmp/gc-heavy-gate/runs/ga-xeii4e.c6`; `GATE_RUN_EXIT rc=0 state=complete`; retained logs: `/var/tmp/ga-xeii4e-full-logs-c6`; `GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name`.
- Load gate: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=35.63 run_max_load=46.30 run_mean_load=37.27 run_readings=90 wait_first_load=56.80 wait_max_load=56.80 wait_mean_load=37.43 wait_readings=61 read_errors=0`. The full suite passed despite host load above the threshold when the bounded wait expired.
- Diff-owned results: `TestBdStoreIntegrationShardRunsOnlyBdStoreTests` PASS; `TestProviderOverridesAndSuiteContractsCrossMakeIsolation` PASS; `TestBdStoreDeleteBatchOrphansExternalDependents` PASS; `TestBdStoreMailWispInsert` PASS; `TestPinnedBdStoreCommandRunnerReportsSilentFallback` PASS; `TestPinnedBdStoreCommandRunnerUsesExactEnvironmentAndKeepsStdoutJSON` PASS.
- The wrapper reports pass-through mode for the scratch path. It inherited no `BD_`, `BEADS_`, `GC_`, or `DOLT_` variables; each local test job scrubs its own environment. The test `bd` binary is pinned to v1.3.1.
- The 235 top-level SKIPs are outside the six diff-owned tests. They are unchanged platform, live-service, opt-in, and helper-harness cases, including `TestBdStoreConditionalWriterConformance` (pinned `bd` lacks `--if-revision`) and the pre-existing unconditional `TestBdStoreConformance` SKIP (`ga-e7z613`). The latter's function body is unchanged at `test/integration/bdstore_test.go:55`, and no shared hunk in that file reaches it. The added helper in `integration_test.go` cannot execute because this test skips before setup. No diff-owned test is skipped.
- Earlier attempts are not gate evidence: a shared build cache lacked compile facts; the first private cache used a too-long temporary socket path; the next run hit a stale-main restart fixture fixed by `e55798f4c4` (#7132). The retained c6 run uses a short private disk cache and the updated mainline merge.
