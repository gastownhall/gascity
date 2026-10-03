# Protected Dolt process rollup release gate

**Verdict:** **PASS**

Deploy bead ga-xscg40; reviewed source `dc1cb6ff76e1a5e0c7e36d4c55aeb79fa0b39fc4`; pinned base `93a1f646bb21f4d4927f26390b44278b814df363`; canonical merged scratch commit `e784d2968b2b74149068f3a654462e9ab5e5bd98`, tree `50a5dea859a8843b03a978991ac68844f7ee8fe4`. `origin/main` advanced after that suite to `c3d461c78ef1e7cf925f036ef73d2f500300808a`; a fresh merge-tree check against the new base passed with tree `eb161fab79f4fc646a7051be22f8e3369bc06f8c`.

| # | Result | Evidence |
| --- | --- | --- |
| 1. Review | PASS | ga-9j7mku records verdict PASS on exact source SHA, no style/security/spec findings. |
| 2. Acceptance | PASS | Reviewed code adds protected_total and protected_by_kind to cleanup summary, keeps schema v1, uses one rollup for JSON/text, and warns on non-baseline protected processes in the stale-db formula. New regression test covers protected>0 with zero reap targets. |
| 3. Tests | PASS | Documented full suite `make test-local-full-parallel` completed 40/40 jobs with 55,571 PASS, 0 FAIL, 236 SKIP; both diff-owned tests PASS by name. Path-selected Beads topology and proxied-native acceptance used pinned bd v1.3.1 and Dolt 2.1.7. Proxied-default PASS 467.01s; shared-server init proxy-readiness failure attributed under 3a; M1 PASS with nine child checks; proxied-native safety/lifecycle PASS; optional pre-journal migration and M5 SKIP. Unconditional Tier A acceptance PASS (366 PASS / 0 FAIL / 19 documented optional SKIP test/subtest lines); all four worker phase-2 CI profiles PASS, with 52/52 JSON reports in pass status and 104/104 checks passed. |
| 3a. Failure attribution | PASS | Full suite: zero failures. The supplemental shared-server acceptance test failed during gc init proxy readiness at host load 24.96; attributed under clauses 1-4 to pre-existing host-load/Dolt-readiness tracker ga-vkhfnj, with a command-path mechanism proof: this diff changes gc dolt-cleanup functions and a formula, neither executed by gc init. The failing test is unchanged in test/acceptance, with no path overlap. All 236 full-suite skips are pre-existing lane filters, helper-only tests, platform or privileged-environment cases, or unrelated opt-in tests; no Dolt-cleanup test skipped and both diff-owned tests passed. |
| 3b. Policy/lint | PASS | Pinned `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-core-boundary check-native-dependency-surface check-eventexport-isolation check-routed-test-rows check-split-topology-rows check-residency-boundary check-docs test-native-doltlite-beads` completed all targets on the clean merged tree against base `93a1f646bb21f4d4927f26390b44278b814df363`; lint found 0 issues, native DoltLite test passed, and pinned linter record resolved 2.12.0 with no mismatch. Log: `/var/tmp/ga-xscg40-regate/policy.log`. |
| 3c. CI config | PASS | No CI configuration changed. |
| 3d. Heavy packages | PASS | Classifier mode=none. |
| 4. HIGH findings | PASS | Reviewer records zero unresolved HIGH findings. |
| 5. Clean branch | PASS | Isolated `deploy/ga-xscg40-gate` cut from the exact reviewed SHA; worktree clean after committing this record. |
| 6. Main compatibility | PASS | Original merge-tree predicted the tested scratch tree; current `origin/main` at `c3d461c78ef1e7cf925f036ef73d2f500300808a` also merges cleanly (tree `eb161fab79f4fc646a7051be22f8e3369bc06f8c`). Full `go build ./...` and `go vet ./...` passed on the tested scratch. |
| 7. Single theme | PASS | Three changed files address one protected-process visibility gap. |

`test_cmd: LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m GOFLAGS=-v TMPDIR=/var/tmp/gotmp make test-local-full-parallel`

`test_cmd_scope: full-suite`

`policy_lane: pinned make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-core-boundary check-native-dependency-surface check-eventexport-isolation check-routed-test-rows check-split-topology-rows check-residency-boundary check-docs test-native-doltlite-beads — PASS`

`test_run_dir: /var/tmp/gc-heavy-gate/runs/ga-xscg40.c3`

`test_checkout: /var/tmp/ga-xscg40-merge.udxtgg`

`test_bd: v1.3.1 (c1c4b642ac1c08d8c828007a1c2f96e47e43ef7c), pinned by deps.env and isolation wrapper`

`test_counts: 55,571 PASS / 0 FAIL / 236 SKIP across 40/40 full-suite jobs`

`skip_justification: 236 skips are existing lane filters for real-process and managed-bd tests (their cmd-gc-process jobs ran), helper-only tests, Linux-inapplicable Darwin behavior, and unrelated opt-in/privileged-environment tests. No Dolt-cleanup-named test skipped; both diff-owned tests passed in process and integration jobs. No skip is caused by the changed protected-rollup paths.`

`failure_attribution: TestBeadsProxiedIgnoresUserLevelSharedServer -> ga-vkhfnj | clause 3(a) MECHANISM: failure in gc init provider-owned proxy startup; changed cleanup-reporting functions run only under gc dolt-cleanup; formula not invoked. Clause 1: unchanged test body/file. Clause 2: tracker created 2026-08-29, this sighting appended during this run. Clause 4: failing test/acceptance package does not overlap diff in cmd/gc or examples/bd/dolt. Full suite had 0 failures. Original custom acceptance attempt with GIT_CONFIG_GLOBAL=/dev/null was invalid environment evidence and excluded.`

`diff_tests_executed: TestCleanupReportJSONShape PASS (cmd-gc-process-5-of-6, integration-packages-cmd-gc-1-of-6); TestRunDoltCleanup_ProtectedRollupByKind PASS (cmd-gc-process-4-of-6, integration-packages-cmd-gc-6-of-6). TestTutorial01 PASS in cmd-gc-process-5-of-6, meeting the path-gated cmd/gc process lane.`

`waiver_ref: none`

`load_threshold: 15`

`load_waited_seconds: 1801`

`load_wait_timed_out: 1`

`run_start_load: 33.75`

`run_max_load: 44.02`

`run_mean_load: 31.33`

`run_readings: 103`

Test logs: `/var/tmp/ga-xscg40-regate/shards`; detached result `/var/tmp/ga-xscg40-regate/gate-output.log` (`GATE_RUN_EXIT rc=0 state=complete`). Policy log: `/var/tmp/ga-xscg40-regate/policy.log`. Acceptance supplement logs: `/var/tmp/ga-xscg40-regate/acceptance-*.log`; follow-on `/var/tmp/gc-heavy-gate/runs/ga-xscg40.preflight-worker`. Pinned-tooling continuation load: threshold 15, waited 1591s, timeout 0, start 14.55, max 19.92, mean 17.92, 12 readings. Corrected first acceptance attempt load: threshold 15, waited 1801s, timeout 1, start 24.96, max 26.15, mean 23.59, 17 readings.

CI path-selected lanes: Beads topology (proxied-default PASS, shared-server failure attributed to ga-vkhfnj under 3a, M1 PASS, optional legacy migration/M5 SKIP), Beads proxied-native (safety and lifecycle PASS), cmd/gc process (all six local shards PASS, including `TestTutorial01` and both diff-owned tests), integration (all local shards PASS), and worker phase-2 matrix (claude/codex/cursor/gemini PASS). Unconditional Tier A acceptance PASS with CI-style optional-tool absence. Worker JSON report summary: `/var/tmp/ga-xscg40-regate/worker-summary.md`.

Preflight/worker load: threshold 15, waited 480s, timeout 0, start 14.26, max 17.59, mean 16.23, 8 readings.
