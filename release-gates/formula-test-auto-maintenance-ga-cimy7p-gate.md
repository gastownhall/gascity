**Verdict:** **PASS**

# Release gate: formula test repositories do not spawn auto-maintenance

- Deploy bead: `ga-cimy7p`; source/build bead: `ga-vb1a3n`; review bead: `ga-d1c85h`.
- Reviewed source: `9483c66ecc6c7c249e0b0cda6d365e827be1fd33`.
- Base: `origin/main` at `248c0c0bc1bfc84ba791026990280940d85f20d6`.
- Canonical synthetic merge: `6c54f5e0da351bd869ae7db119fcd044062d18af`, tree `fb9a1d493a8060cdc1864cb15af628e415de7733`.
- Scope: one test file, `internal/formula/source_test.go` (+35 lines). No CI configuration or production file changed.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-d1c85h` reviewer round 1 PASS on the exact reviewed SHA; no blocking findings. |
| 2 | Acceptance criteria met | PASS | `initRepo` sets `maintenance.auto=false` in its disposable Git repositories. New `TestInitRepoCommitSpawnsNoAutoMaintenance` checks a real commit with `GIT_TRACE`, verifies the trace contains the commit, and rejects a maintenance spawn. Independent reviewer reproduced RED without the config and GREEN with it, with before/after stress; the deploy full suite executed the new test and all helper callers successfully. |
| 3 | Tests pass | PASS (one unrelated failure attributed) | Full CI-equivalent local suite covered fast unit, cmd/gc process, and integration shards: 39 of 40 jobs green. One unchanged `examples/gastown` test failed only in TempDir cleanup after its body; attributed to pre-existing tracker `ga-vkhfnj` under criterion 3a below. The only diff-owned test passed. |
| 4 | No high-severity review findings open | PASS | Review notes report zero blocking or high-severity findings; two information-level observations only. |
| 5 | Final branch clean | PASS | Canonical merged scratch was clean before/after gates; release file committed on isolated deploy branch, with clean working tree checked before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree` returned tree `fb9a1d493a8060cdc1864cb15af628e415de7733` without conflict; build and vet on that exact tree passed. |
| 7 | Single feature theme | PASS | The single source commit changes only the throwaway formula test repository helper and its regression test. |

## Criterion 3 evidence

- `test_cmd`: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-cimy7p-full-logs LOCAL_TEST_JOBS=4 SHELL=/bin/sh DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 make test-local-full-parallel`, detached via `gate-detached-run.sh` on the canonical synthetic merge. This is the documented full local suite; its 40 jobs include the required cmd/gc process and integration lanes for `internal/**` changes.
- `test_cmd_scope`: `full-suite`.
- `test_counts`: 56,779 top-level PASS; 1 top-level FAIL attributed below; 236 top-level SKIP. Jobs: 39 PASS, 1 FAIL. `GATE_RUN_EXIT rc=2 state=failed` reflects that one attributed failure, not a zero-failure run.
- `diff_tests_executed`: `TestInitRepoCommitSpawnsNoAutoMaintenance` PASS in `unit-core.log:33976-33977`. The changed `initRepo` helper's 11 calling tests also passed by name in the unit job.
- `skip_justification`: No diff-owned SKIP. The changed test passed and the touched `source_test.go` has no skipped test in this run. Other SKIPs are unchanged platform, optional fixture, or lane gating conditions in other tests; the `internal/formula` skip `TestCompileBugReportFlowV2` is in untouched `compile_test.go` and requires an absent `/home/ubuntu/tooling` formula fixture. The test-only helper cannot reach skips in other packages.
- `failure_attribution`: `TestReaperOwnerProtectionOnRealBd` -> `ga-vkhfnj`, the existing whole-suite/host-load contention tracker (opened and new sighting verified). Clause 1: `diff-owned-tests.py --site examples/gastown/maintenance_bd_integration_test.go:558` says `UNCHANGED`, with no hunk or shared hunk in that file. Clause 3(a): this diff changes only `internal/formula/source_test.go`, compiled into a different package's test binary; the failing test shells out to real bd and runs the reaper, then `t.TempDir` cleanup finds `.beads/dolt/.dolt/noms` nonempty. The changed helper cannot execute there. The 0.01s new test's `unit-core` job completed before this integration shard started, so it supplied no concurrent test load. Clause 4: no path overlap. First exact-test sighting on this tracker; no repeat-signature escalation. The test's assertions did not fail. Source: `integration-packages-core-2-of-4.log:315-317`.
- `waiver_ref`: none; gascity has no waiver path.
- `heavy_mode`: none (`heavy-composite-gate.sh classify` returned `HEAVY_MODE mode=none`).
- `ci_lane_run`: not applicable; this diff changes no CI job, matrix, timeout, or required-check list.
- `policy_lane`: PASS. Fresh view of the synthetic merge; `gate-base.sh run` pinned `LINT_BASE` to the gated base; repo policy checks, `make lint-affected`, `make fmt-check-changed`, and `make check-docs` passed. `GLT_RECORD` pin/resolved `golangci-lint` 2.12.0, mismatch false, zero issues. `FRESH_TREE_RECORD` modified=0 untracked=0 ignored=0. Log: `/var/tmp/ga-cimy7p-policy.log`.
- `drift_lane`: PASS. CI's BUILD-files step `make bazel-sync` followed by `git diff --exit-code` in its own fresh view; no drift. `FRESH_TREE_RECORD` modified=0 untracked=0 ignored=0. Log: `/var/tmp/ga-cimy7p-bazel-drift.log`.
- `format/build/vet`: PASS. `git diff --check`, `go build ./...`, and `go vet ./...` on the canonical merged tree. Logs: `/var/tmp/ga-cimy7p-build.log`, `/var/tmp/ga-cimy7p-vet.log`.
- `test_bd`: `GASCITY_TEST_BD version=v1.3.1 bin=/home/jaword/.local/bd-versions/v1.3.1/bd reports='bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)' ref_check=match:c1c4b642a origin=preexisting pin_source=/var/tmp/gc-merge-ga-cimy7p-real.79WZNl/deps.env displaces='bd version 1.1.0 (0954be416)'`.
- `load_threshold`: 15; `load_waited_seconds`: 0; `load_wait_timed_out`: no; `run_start_load`: 13.35; `run_max_load`: 40.67; `run_mean_load`: 22.95; `run_readings`: 60; `wait_first_load`: 13.35; `wait_max_load`: 13.35; `wait_mean_load`: 13.35; `wait_readings`: 1; `read_errors`: 0.
- The first detached attempt did not execute tests because its shard log directory did not exist (`GATE_RUN_EXIT rc=2`, `run_readings=0`). The directory was created and the same full command rerun deliberately. Only the second run supplies test evidence above. Run directory: `/var/tmp/gc-heavy-gate/runs/ga-cimy7p.c3`.
