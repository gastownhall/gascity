# Release gate: retry a preserved named session after a failed start

**Verdict:** **PASS**

- Deploy bead: `ga-fdgugf`
- Review bead: `ga-8qwjyl`
- Reviewed commit: `82358155acd8657630fcbde017e509e651c18e62`
- Gate base: `1e66362406a6ccd4100f9cfd34146bd0fa1f651c`
- Merged test commit: `d3de0a02b2d8c29caef47849f6243cc29f9265be`

`docs/PROJECT_MANIFEST.md` is absent. This record uses the deployer release criteria, `TESTING.md`, and `engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer `ga-8qwjyl` issued a final PASS for the resolved commit above. No review carryover. |
| 2 | Acceptance criteria met | PASS | After a configured named session start fails, `commitStartFailure` preserves its session bead and clears only the failed attempt's `last_woke_at` lease. The next tick can retry; startup health still quarantines after five failed attempts. The two new tests cover both paths. |
| 3 | Tests pass | PASS | The documented full 40-job local CI-equivalent suite passed twice on the materialized merge tree. The auditable run recorded 102,994 PASS, 0 FAIL, and 335 SKIP result lines, including subtests. Both diff-owned tests passed by name in process and integration shards, and `TestTutorial01` passed in the required process lane. All skips are outside the changed test file. Required policy, lint, generated-file drift, build, and vet lanes passed. |
| 4 | No high-severity review findings open | PASS | Review `ga-8qwjyl` reports no blocker or unresolved high finding. |
| 5 | Final branch is clean | PASS | This record is the only added file and is committed as the isolated deploy branch tip; the final branch and index are clean after commit. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed commit merged cleanly with recorded base `1e66362406a6ccd4100f9cfd34146bd0fa1f651c`; the materialized merge above passed `go build ./...` and `go vet ./...`. A later read-only merge-tree check against `e38ce9cc55d15fa351199c042f0d07aba1a347af` was also clean (tree `4fbd1e7aedccba19b500ca31184bfc6299934f7c`). |
| 7 | Single feature theme | PASS | The commit changes only the session reconciler's failed-start lease handling and its two regression tests in `cmd/gc/`. |

## Criterion 3 evidence

- `test_cmd`: `make test-local-full-parallel` on the materialized merged tree, through `load-gate-run.sh --threshold 15 --max-wait 1800` and `isolated-test-run.sh`, with `GOFLAGS=-v`, rootless Podman socket, and Ryuk disabled. The gascity isolation wrapper reported PASS-THROUGH on this disposable merge worktree and supplied pinned test bd v1.3.1. This target runs 40 jobs, including the required `cmd-gc-process` shards, `TestTutorial01`, and integration shards.
- `test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `ci_lane_run: n/a (no CI configuration in the reviewed diff)`.
- `test_counts: 102,994 PASS, 0 FAIL, 335 SKIP` result lines, including subtests; 40/40 jobs passed. `diff_tests_executed: TestReconcileSessionBeads_ConfiguredNamedSessionRetriesAfterTransientStartFailure PASS; TestReconcileSessionBeads_ConfiguredNamedSessionStartFailureLoopQuarantines PASS`. Each passed in both the `cmd-gc-process` and integration `cmd/gc` shards. `TestTutorial01` also passed in `cmd-gc-process-4-of-6`. The owned tests were identified by `diff-owned-tests.py`; `diff_owned_evidence: /var/tmp/ga-fdgugf-diff-owned.txt`.
- `skip_justification`: all 335 SKIP lines are outside the changed test file and neither diff-owned test has a skipped subtest. The full-suite job split accounts for process-only tests skipped in the unit/integration jobs and exercised by the six `cmd-gc-process` shards. Other skips are platform gates (for example Darwin launchd/kqueue tests on Linux), optional external integrations, and helper-process guards. These do not exercise the modified failed-start lease arm. `not_owned_skip_evidence: /var/tmp/gc-heavy-gate/runs/ga-fdgugf.c3-logs/skip-audit.tsv` enumerates all 335 skips with nearby output and marks zero as belonging to the changed test file; the complete 40 per-job logs are in the same run directory.
- `policy_lane: PASS` — `make test-ci-policy`, static guards, `test-native-doltlite-beads`, `lint-affected`, `fmt-check-changed`, and `check-docs` on fresh views of the recorded merge tree. Pinned golangci-lint v2.12.0 matched. `drift_lane: PASS` — `make bazel-sync` followed by `git diff --exit-code` on a fresh tree. `go build ./...` and `go vet ./...` passed on the merge tree.
- First full run: `/var/tmp/gc-heavy-gate/runs/ga-fdgugf.c3`, `GATE_RUN_EXIT rc=0`, 40/40 jobs passed. `load_threshold=15`, `load_waited_seconds=300`, `load_wait_timed_out=0`, `run_start_load=14.56`, `run_max_load=29.30`, `run_mean_load=21.12`, `run_readings=60`, `read_errors=0`. Its per-test logs were deleted by the runner's default success cleanup, so it does not supply counts.
- Auditable rerun: `/var/tmp/gc-heavy-gate/runs/ga-fdgugf.c3-logs`, preserving all 40 per-job logs in `jobs/`. `GATE_RUN_EXIT rc=0 state=complete`. `load_threshold=15`, `load_waited_seconds=0`, `load_wait_timed_out=0`, `run_start_load=5.94`, `run_max_load=25.05`, `run_mean_load=17.58`, `run_readings=51`, `read_errors=0`.
- `test_bd`: `GASCITY_TEST_BD version=v1.3.1 bin=/home/jaword/.local/bd-versions/v1.3.1/bd reports='bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)' ref_check=match:c1c4b642a origin=preexisting pin_source=/var/tmp/gc-merge-validate.ga-fdgugf.7AuPaw/deps.env displaces='bd version 1.1.0 (0954be416)'`.
