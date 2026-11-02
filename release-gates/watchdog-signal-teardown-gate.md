**Verdict:** **PASS**

# Watchdog signal teardown release gate

- Deploy bead: `ga-ki4b1e`
- Reviewed source: `7a41b36c18b5f91c8c84d7ce05d0d62561badf8f`
- Gate base: `58b61ecd5848494eb922986c7d14ff2724e40b74`
- Tested merge tree: `9739cdb644213d10109070a1d8270f6e7b2b1790` at `/var/tmp/gc-merge-validate.ga-ki4b1e.sjYaWF`
- Date: 2026-10-05 UTC

| # | Verdict | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Reviewer bead `ga-9a182x` records round 1 PASS on the exact reviewed source SHA. |
| 2. Acceptance criteria | PASS | On a signal, both shard runner scripts call `gc_harness_terminate_supervised`, which kills and reaps the separately grouped watchdog before draining the supervised run. The new regression test sends SIGTERM and requires the caller's output pipe to reach EOF; it passed in this gate's full suite. The investigator and reviewer independently reproduced RED on the old harness. |
| 3. Tests | PASS | The documented full-scope `make test` ran on the materialized merge tree through `load-gate-run.sh` and `isolated-test-run.sh`, detached as `/var/tmp/gc-heavy-gate/runs/ga-ki4b1e.c3`. Exit 0; 51,099 test/subtest PASS, 0 FAIL, 206 SKIP; 183 packages PASS, 0 FAIL, 22 without tests. The diff-owned `scripts/TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe` passed by name in that run (0.22 s). |
| 4. High-severity review findings | PASS | Reviewer recorded no security findings or open high-severity findings. The SIGKILL/OOM residual is separately tracked in `ga-ksfalp`. |
| 5. Final branch clean | PASS | The reviewed source and merge scratch were clean; the gate record is committed on the isolated deploy branch before push. |
| 6. Clean merge from main | PASS | Reviewed source merged without textual conflicts into the pinned base. The resulting tree passed `go build ./...` and `go vet ./...`. |
| 7. Single feature theme | PASS | The one source commit changes only the shard watchdog teardown and its regression test under `scripts/`. Ancestry scope check passed for `ga-ki4b1e` and its source build bead `ga-880tzy`. |

## Criterion 3 details

- `test_cmd`: `make test` via `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 make test`
- `test_cmd_scope`: full-suite
- `test_counts`: 51,099 PASS, 0 FAIL, 206 SKIP including subtests; top level 28,220 PASS, 0 FAIL, 159 SKIP
- `diff_tests_executed`: `scripts/TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe` PASS
- `skip_justification`: No SKIP occurred in `scripts`, the diff-owned package. The 206 skips are existing fast-unit/environment gates in other packages; the change touches no production package.
- `test_bd`: `GASCITY_TEST_BD version=v1.3.1 bin=/home/jaword/.local/bd-versions/v1.3.1/bd reports='bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)' ref_check=match:c1c4b642a origin=preexisting pin_source=/var/tmp/gc-merge-validate.ga-ki4b1e.sjYaWF/deps.env displaces='bd version 1.1.0 (0954be416)'`
- `load_threshold`: 15; `load_waited_seconds`: 0; `load_wait_timed_out`: 0
- `run_start_load`: 8.00; `run_max_load`: 9.50; `run_mean_load`: 8.62; `run_readings`: 10
- `failure_attribution`: n/a; `waiver_ref`: none
- `heavy_mode`: none (`heavy-composite-gate.sh classify`)
- `ci_lane_run`: n/a; no CI job, matrix, timeout, or required-check configuration changed

## Fast lane

- `make test-ci-policy`: PASS in a fresh tree at the same merge commit.
- `make lint-affected`: PASS, `./scripts` selected, 0 issues; pinned golangci-lint 2.12.0, no toolchain mismatch. `LINT_CHANGED_REF` was set to the pinned gate base and `LINT_CHANGED_SCOPE=tracked` so the changed package was actually selected.
- `make fmt-check-changed`: PASS with the same pinned selection and linter.
- `make bazel-sync` followed by `git diff --exit-code`: PASS in its own fresh tree, matching the CI BUILD-file drift check.
- Each lane reported `FRESH_TREE_RECORD` with the tested merge commit and zero modified, untracked, or ignored inputs; base-sensitive lanes reported `GATE_BASE_RECORD base=58b61ecd5848494eb922986c7d14ff2724e40b74`.
