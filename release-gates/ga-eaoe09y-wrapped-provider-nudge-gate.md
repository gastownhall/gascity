**Verdict:** **PASS**

## Wrapped-provider wait-idle nudges

- Deploy bead: ga-eaoe09y; build: ga-adip45; review: ga-4atjqw6 (closed PASS).
- Reviewed source: `b52be3379c526d11883180ba4cded324032cb3cd`.
- Gate base: `35fc9bb2ee7f02de0ac9bcb28d9aed5470097516` (`origin/main` pinned when criterion 6 began).
- Tested merge: `a5380ffc20bafa442d1c94f5b5f8243dd0538f83`; tree: `80935d38398d1e640178f06289a85f09e2adb5d9`.
- Deploy branch: `deploy/ga-eaoe09y-gate`, cut at the reviewed source.
- Evaluated: 2026-10-13T03:58:55+00:00.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-4atjqw6 records PASS for the exact source, with style/security severity none and no blocker, major, or minor finding. |
| 2 | Acceptance criteria met | PASS | The nine provider-family rows pass. Builtin and wrapped Claude targets deliver live when idle and queue without entering the blocking idle wait when busy. The worker test verifies the caller-supplied family contract. The field comment documents that contract; downgrade notes retain the configured name. |
| 3 | Tests pass | PASS | All required Bazel CI lanes below passed fresh. 101775 PASS, 0 FAIL, 306 SKIP result lines; all four diff-owned tests passed by name. Existing skips are justified below. |
| 4 | No unresolved high-severity finding | PASS | Review records no blocker, major, minor, or security finding. Informational telemetry and scope notes are included in the PR text. |
| 5 | Final branch clean | PASS | Source checkout and merged scratch had empty git status before this record. This gate record is the only deployment addition; its commit is followed by a clean-status check before push. |
| 6 | Clean divergence from main | PASS | Materialized the conflict-free merge at the pinned base; full-tree Bazel build and nogo/vet passed. No self-rebase needed. A publish-time textual recheck against `f63ca535eb11d147855d83dbb1ab10b66450d701` also passed; main's intervening changes do not touch the four diff paths. Build/test evidence remains pinned to the tested merge above. |
| 7 | Single feature theme | PASS | Two commits, four files: provider-family resolution for both nudge gates, their regression tests, and the worker field comment. Ancestry scope check accepts ga-adip45/ga-eaoe09y; no denied .claude paths or unrelated theme. |

## Test evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`;
`failure_attribution: none`; `ci_lane_run: n/a (no CI-config change)`.

Every lane used `--config=ci --config=remote-exec --nocache_test_results --jobs=16 --remote_download_outputs=toplevel --rewind_lost_inputs --test_env=GO_TEST_WRAP_TESTV=1`, with its named tier config. The suite ran through `gate-detached-run.sh`, `load-gate-run.sh`, `isolated-test-run.sh`, and `gate-base.sh run` on the same tested merge.

| Required CI lane | Command | PASS targets | Retained test/shard results |
|---|---|---:|---:|
| Unit, nogo, format, generated, policy | `make check` (`bazel test //...` plus shell guards) | 251 | 335 |
| Acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 7 | 24 |
| Integration packages | `bazel test --config=integration //test:integration_packages` | 29 | 76 |
| Integration smoke | `bazel test --config=integration-smoke //test/integration:integration_test` | 1 | 14 |

`TestTutorial01` passed in integration-packages with the documented process configuration. Target summaries contain only PASSED; artifact retention reported zero errors in every lane. Counts are column-0 terminal result lines summed across retained logs, rather than unique test names.

`diff_tests_executed`:

- `TestNudgeTargetProviderFamily`: PASS (nine rows).
- `TestDeliverSessionNudgeWithProviderWaitIdleDeliversLiveToIdleClaudeFamily`: PASS (builtin/wrapped).
- `TestDeliverSessionNudgeWithProviderWaitIdleQueuesBusyClaudeFamilyWithoutBlocking`: PASS (builtin/wrapped).
- `TestRuntimeHandleWaitIdleGateTakesTheProviderFamily`: PASS (family/alias contract).

`skip_justification`: 229 unit, 13 acceptance, 63 integration-package, and one integration-smoke skip. All 105 process-gated unit skips subsequently PASS in integration-packages. Other skips are unchanged platform conditions, optional live providers/tools/credentials, legacy-binary or catalog fixtures, synthetic skip/harness tests, existing conformance capability limits, and explicit pre-existing placeholders. The smoke skip is the unconditional existing `TestBdStoreConformance` skip at `test/integration/bdstore_test.go:55`; it does not depend on this diff or the configured test bd. No skip event comes from either changed test file. The diff changes no skip condition or build tag. Its only shared test hunks are the sync import used by the new busy test and the helper called solely by the two new delivery tests, which both PASS.

`policy_lane: PASS`: pinned `make fmt-check` (golangci-lint 2.12.0, mismatch=false), shell guards, `make check-hooks test-ci-policy lint`, and 11 Bazel policy targets.
`drift_lane: PASS`: `make bazel-sync` followed by empty git diff/status, six generated-artifact targets, and the OpenAPI breaking check. Each fast lane ran through `fresh-tree-run.sh` and `gate-base.sh run`; all completed before suite launch.

Environment: wrapper PASS-THROUGH mode on the clean gascity scratch, no inherited BD_/BEADS_/GC_/DOLT_ variables, pinned test bd v1.3.1 with matching ref check. Podman socket active, testcontainer sweep enabled, cached Dolt 2.2.0 matches deps.env; Ryuk disabled with the required opt-out. Bazel tests use the repository's pinned remote-worker environment.

## Load and retained evidence

- `load_threshold: 15`
- `load_waited_seconds: 1801`
- `load_wait_timed_out: 1`
- `run_start_load: 57.22`
- `run_max_load: 58.53`
- `run_mean_load: 35.90`
- `run_readings: 36`
- `wait_first_load: 75.36`
- `wait_max_load: 82.18`
- `wait_mean_load: 63.25`
- `wait_readings: 61`; `read_errors: 0`

The ordinary gate proceeded after its bounded wait; the full suite passed. Logs were retained and read with `gate-test-evidence.py`.

- `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-eaoe09y.c3/logs`
- Suite output, BEP files, verified original logs/XML, and skip inventory: `/var/tmp/gc-heavy-gate/runs/ga-eaoe09y.c3`.
- Fast-lane evidence: `/var/tmp/gc-heavy-gate/runs/ga-eaoe09y.fast`.

PR requirements: Refs ga-adip45; explain the runtime-only State.Provider/operation-telemetry change from configured alias to builtin family, and disclose that cmd_start.go/launch_command.go comparisons remain untouched/unverified. No upstream issue or comment is authorized. The PR body carries these requirements; its stored body is checked after creation. Live qwen delivery remains unverified; regression coverage uses fake runtimes.
