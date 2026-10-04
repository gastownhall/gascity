# Live contract store-conflict retry release gate

**Verdict:** **PASS**

- Work bead: `ga-upr9ql`; reviewed commit: `0be0c2a27f872f2ab86895a68eb01af1909310af`; review bead: `ga-p189vq`.
- Gate base: `origin/main` at `2f33858ba14280b07ba5ea8e740e70f78f59c7a6`. Fresh merge validation tree: `0e33f5c802d9dd04efb2ea04f90ec3717ecd03c8` (scratch commit `70e34222be62c4ea6a1f2588356e8f8c4ffa6e3b`). Origin main was still at the pinned base after the suite.
- Deploy mode: remote; target is an isolated `deploy/ga-upr9ql-gate` branch on fork.

| # | Verdict | Evidence |
|---|---|---|
| 1. Review PASS | **PASS** | Reviewer `ga-p189vq` approved the exact reviewed commit above. |
| 2. Acceptance criteria | **PASS** | The live contract helper reissues only a declared `503 store_conflict`, for at most 30 seconds; other responses remain final. New deterministic API and helper tests verify classification, reissue, and final-response behavior. No production API or schema behavior changes. |
| 3. Tests pass | **PASS** | Full 40-job `make test-local-full-parallel` on the clean merged scratch tree completed with exit 0. All five diff-owned tests passed by name; details below. The policy, lint, and generated-file drift fast lane passed first. |
| 4. High-severity review findings | **PASS** | No unresolved HIGH finding in the reviewer PASS. |
| 5. Clean final tree | **PASS** | The merge scratch and role worktree were clean before creating this gate record. The isolated deploy branch will contain only the reviewed commits and this gate record. |
| 6. Clean divergence from main | **PASS** | `git merge-tree --write-tree` merged reviewed commit with the pinned base without conflicts; `go build ./...` and `go vet ./...` passed on the merged tree. |
| 7. Single feature theme | **PASS** | Changes are limited to the live API contract retry helper, its deterministic tests, and generated Bazel source lists. Scope guard accepted the lineage citations for `ga-upr9ql`, `ga-19ii7x`, and `ga-9rsjmv`. |

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 GOFLAGS=-v TMPDIR=/var/tmp/gotmp LOCAL_TEST_JOBS=4 LOCAL_TEST_LOG_DIR=/var/tmp/gc-heavy-gate/runs/ga-upr9ql.c3/logs make test-local-full-parallel`, through `load-gate-run.sh --threshold 15 --max-wait 1800` and `isolated-test-run.sh`, detached by `gate-detached-run.sh`.
- `test_cmd_scope`: `full-suite`; `heavy_mode`: `none` from `heavy-composite-gate.sh classify`.
- `test_counts`: 56,541 top-level PASS, 0 FAIL, 236 top-level SKIP across 40 passing jobs. Including subtests: 102,334 PASS, 0 FAIL, 335 SKIP. Sharded targets execute some tests twice; these are execution counts from all retained `GOFLAGS=-v` logs.
- `diff_tests_executed`: `TestHandleSessionWake_SerializationConflictIsDeclaredRetryable503` PASS; `TestLiveContractStoreConflictIsOnlyTheDeclaredRetryable503` PASS; `TestLiveContractRequestReissuesDeclaredStoreConflict` PASS; `TestLiveContractRequestOneOfReissuesDeclaredStoreConflict` PASS; `TestLiveContractDoReturnsFinalResponsesWithoutReissue` PASS. Derived with `diff-owned-tests.py --explain` against the pinned base and reviewed head, then matched by name in the retained full-suite logs.
- `skip_justification`: No diff-owned test skipped. Other skips are unchanged platform, fixture, live-provider, helper, or explicitly gated scenarios. In the touched `gc_live_contract_test.go`, the parent contract test passed; its five read-sweep subtests skip on unchanged `probe.skipReason` before calling the changed request helper. `diff-owned-tests.py --site test/integration/gc_live_contract_test.go:1371` reports `UNCHANGED`; the modified helper cannot set that skip reason. The separate Dolt persistence, Kubernetes, macOS, root-only, and known flaky CI skips in the integration logs do not exercise this test-helper change. The rootless Podman socket and cached pinned Dolt image were available; the test command used a pinned bd v1.3.1.
- `policy_lane`: PASS — fresh-tree, pinned-base `make lint-affected`, `make fmt-check`, `make test-ci-policy`, and `make check-native-dependency-surface`; zero lint issues.
- `drift_lane`: PASS — `make bazel-sync` followed by `git diff --exit-code` in a fresh view; `make dashboard-ci`, `make spec-ci`, and `./scripts/check-generated-docs-drift.sh` in separate fresh views.
- `ci_lane_run`: n/a; this diff does not modify CI configuration.
- `waiver_ref`: none.
- `load_threshold`: 15; `load_waited_seconds`: 0; `load_wait_timed_out`: 0; `run_start_load`: 11.81; `run_max_load`: 34.55; `run_mean_load`: 21.23; `run_readings`: 61. The bounded load gate started below threshold; the host became busier during the passing run.
- Retained evidence: `/var/tmp/gc-heavy-gate/runs/ga-upr9ql.c3` (suite), `/var/tmp/gc-heavy-gate/runs/ga-upr9ql.policy.20261004` (policy), and sibling `drift`, `dashboard`, `spec`, and `docsdrift` run directories.
- Isolation wrapper reported pass-through because the temporary merge scratch path is not on its repository list. The command ran on the clean, materialized merge scratch tree; its environment contained no inherited `BD_`, `BEADS_`, `GC_`, or `DOLT_` variables. The pinned bd was injected by the wrapper.
