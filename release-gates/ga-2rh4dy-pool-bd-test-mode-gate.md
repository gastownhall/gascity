# Release gate: pool real-bd tests use test mode

**Verdict:** **PASS**

- Deploy bead: `ga-2rh4dy`; build bead: `ga-1f81md`; review bead: `ga-6ci5y8`
- Reviewed source: `4f6eef6002cd219a6ee18839ef0dbea71a11cb8e`
- `gate_base: 042193bad63367f0a0892e30e19ed893430096bb`
- Materialized merge: `ac567c5c8d3111188f26f20ce1fcb12d03da2426` (tree `46a34cacb41b589fa3069b0d2e1840370b485e9b`)
- Deploy mode: remote; push remote: fork

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-6ci5y8` records a reviewer PASS on the exact reviewed source, with no blocker, major, minor, security, or specification finding. The recorded commit resolves to a real commit. |
| 2 | Acceptance criteria met | **PASS** | The one-file `cmd/gc/pool_test.go` change sets `BEADS_TEST_MODE=1` through `t.Setenv` inside the test-owned embedded-bd helper. Its new subprocess contract test passed in both process and integration-package lanes. All four real-bd fixtures using the helper passed by name in the process lane, with no `directory not empty` cleanup error. The reviewer independently measured zero detached send-metrics children with test mode on, versus 11 (bd 1.1.0) and four (bd 1.3.1) with it off. The original cleanup race was not reproduced before the fix, so this is mechanism and stressed-execution evidence, not a measured failure-rate reduction. |
| 3 | Tests pass | **PASS** | The documented full CI-equivalent local command `make test-local-full-parallel` ran through `load-gate-run.sh` and `isolated-test-run.sh` on the materialized merge: 40/40 jobs PASS; 56,587 top-level tests PASS, 0 FAIL, 236 SKIP. The diff-owned contract test had 2 PASS, 0 FAIL, 0 SKIP. All four affected real-bd fixtures PASS in `cmd-gc-process` with `GC_FAST_UNIT=0`; their integration-package SKIPs are expected because that lane selects only its own tests. The 236 skips are suite-controlled package, platform, provider, helper, or opt-in exclusions; none is diff-owned, and the affected process tests ran. `go build ./...` and `go vet ./...` passed. `test_cmd_scope: full-suite`; `diff_tests_executed: TestPinTestOwnedBDHomeRunsBDSubprocessesInTestMode, 2 PASS, 0 FAIL, 0 SKIP`; `waiver_ref: n/a`; `ci_lane_run: n/a` (no CI configuration changed); `heavy_mode: none`. |
| 3b | Policy/lint and generated-file drift | **PASS** | The CI-scoped `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary check-routed-test-rows check-split-topology-rows check-residency-boundary check-release-dist-ignore check-docs` passed via `gate-base.sh run`; pinned golangci-lint 2.12.0 reported zero issues. In a separate fresh tree, `make bazel-sync` followed by `git diff --exit-code` passed with no generated drift. Both lanes used the pinned gate base, not the moving `origin/main`. |
| 4 | No high-severity review findings open | **PASS** | The exact-source reviewer record names no blocker, major, minor, security, or specification finding. Follow-up `ga-u5azpz` concerns similar tests in `internal/doctor` and does not change this diff. |
| 5 | Final branch is clean | **PASS** | The source worktree was clean before the gate file; `git diff --check origin/main...4f6eef6002cd219a6ee18839ef0dbea71a11cb8e` passed. `make check-hooks` confirmed `.githooks` active. The isolated deploy branch is checked again after committing this file. |
| 6 | Branch diverges cleanly from main | **PASS** | The source merged textually into gate base `042193bad63367f0a0892e30e19ed893430096bb`, and the materialized merge built and vetted cleanly. After `origin/main` advanced to `aa66f2b601a8314b635894d8aa2779e43c141db3`, a fresh merge also had no conflict (tree `0bf63f32133642bb9f8570a65365a27689be49a9`) and passed `go build ./...` and `go vet ./...`. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | The source range changes only `cmd/gc/pool_test.go`: suppress the detached metrics child in test-owned embedded-bd fixtures and prove subprocess inheritance of test mode. The ancestry guard accepted only this deploy bead and build bead. |

## Test evidence and limits

- Detached run: `/var/tmp/gc-heavy-gate/runs/ga-2rh4dy.c3`; `GATE_RUN_EXIT rc=0`.
- Shard logs: `/var/tmp/ga-2rh4dy-suite/shards`; final output: `/var/tmp/ga-2rh4dy-suite-output.log`.
- Host load: threshold 15; waited 120 seconds; wait did not time out; run started at 14.43, reached 38.29, averaged 22.08 over 69 readings. No read errors.
- `isolated-test-run.sh` used pass-through mode for this gascity scratch path. It reported no inherited `BD_`, `BEADS_`, `GC_`, or `DOLT_` variables; the local sharded runner uses `env -i`. Rootless Podman and the pinned bd 1.3.1 were available.
- The `cmd/gc/**` CI path filter also schedules the beads topology, worker phase 2, process, product-metrics testhook, and integration jobs. The local 40-job suite includes the process, product-metrics, and integration lanes; the other path-gated CI jobs will report their own results on the PR. This pre-PR record does not claim those CI jobs have already run.
