# Release gate: live herdr pump test under a long HOME

**Verdict:** **PASS**

- Deploy bead: `ga-mwa7kw`; reviewed build: `ga-sux0ij`; review: `ga-3jef5x`.
- Reviewed source commit: `62d249799df112f42041cd1dac7c5b224fbb9882`.
- Pinned gate base: `origin/main` at `8d8a6be86089d3c8e6a2ec6c870a4e10c42db641`; materialized merge commit `5aff8a2ada2c8798aa2464bed2a83ecdcaa00ea2`, tree `700a249223edebdd621e096db0263ce20b174919`.
- Retained gate run: `/var/tmp/gc-heavy-gate/runs/ga-mwa7kw.c3`; `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-mwa7kw.c3/logs`.
- Public bug: [#7218](https://github.com/gastownhall/gascity/issues/7218). `waiver_ref: none`.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | **PASS** | Reviewer bead `ga-3jef5x` recorded PASS on the exact reviewed source commit. This was a fresh review after conflict reconciliation, with no carryover. |
| 2 | Acceptance criteria | **PASS** | The changed live herdr test gives the server a short private `/tmp` config root on Linux, pins the pane shell to `/bin/sh`, and prints pane/pump diagnostics on timeout. The reviewer reproduced the old long-HOME failure and verified the revised test. The independent full suite executed `TestSessionEventPumpLiveHerdr` under a long private HOME and it passed. The diff changes only `cmd/gc/session_event_pump_live_test.go` (+63/-3). |
| 3 | Tests pass | **PASS** | `test_cmd_scope: full-suite`. The documented `make test-local-full-parallel` ran through the load gate and isolation wrapper on the materialized merge tree, with rootless Podman, the pinned Dolt image and pinned `bd`. All 40 jobs completed with **57,675 PASS, 0 FAIL, 235 SKIP** by `gate-test-evidence.py`. The sole diff-owned test, `TestSessionEventPumpLiveHerdr`, had a real PASS in `cmd-gc-process-3-of-6.log:16606`; its separate integration-package SKIP does not remove that PASS. `TestTutorial01` passed in `cmd-gc-process-4-of-6.log:11145`. Other SKIPs are existing platform, helper, and opt-in conditions outside this test-only diff, including tests intentionally reserved for the process lane, which ran here. No diff-owned test was left unexecuted or failed. |
| 4 | No high-severity review findings | **PASS** | The review recorded no blocker or major security, spec, or style finding. |
| 5 | Final branch clean | **PASS** | The materialized merge scratch remained clean after build, vet, policy, drift, and the full suite; the isolated deploy branch is checked clean after committing this record. |
| 6 | Clean divergence from main | **PASS** | `git merge-tree --write-tree` succeeded against the pinned base and produced the exact tested tree. During the run, main advanced to `b0c59549c6547653c2176c583489fa0bc83f9d0b` through other PRs; a fresh merge-tree against that ref also succeeded, producing `ff9f868969f109cd3abdd34f58dd544a337f9543`. |
| 7 | Single feature theme | **PASS** | All three source commits concern the one live herdr pump test, cite the confirmed fix/build beads, and touch one test file. |

## Criterion 3 detail

- `policy_lane: PASS`: `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads` in a separate pinned fresh tree; log `/var/tmp/ga-mwa7kw-policy.log`.
- `drift_lane: PASS`: `make bazel-sync` followed by `git diff --exit-code` in a separate fresh tree; log `/var/tmp/ga-mwa7kw-bazel-drift.log`.
- `go build ./...`, `go vet ./...`, and `git diff --check`: PASS on the materialized merge. `heavy_mode: none`. `ci_lane_run: n/a (no CI configuration changed)`.
- `load_threshold=15`, `load_waited_seconds=1801`, `load_wait_timed_out=1`, `run_start_load=26.00`, `run_max_load=54.40`, `run_mean_load=39.97`, `run_readings=73`. The bounded wait timed out and proceeded under the ordinary gate rule; all jobs passed. Wait observations: first `26.59`, max `46.96`, mean `30.18`, 61 readings, 0 read errors.
- `test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env HOME=/var/tmp/gc-heavy-gate/runs/ga-mwa7kw.c3/home XDG_CONFIG_HOME=/var/tmp/gc-heavy-gate/runs/ga-mwa7kw.c3/home/.config DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOCACHE=/var/tmp/gc-heavy-gate/runs/ga-iufo27.c3-diskcache/build-cache TMPDIR=/var/tmp/gotmp SHELL=/bin/sh GOFLAGS=-v LOCAL_TEST_JOBS=4 LOCAL_TEST_LOG_DIR=/var/tmp/gc-heavy-gate/runs/ga-mwa7kw.c3/logs make test-local-full-parallel`.
- The isolation wrapper reported pass-through because the disposable merge scratch is outside its configured isolate list. It confirmed no inherited `BD_`, `BEADS_`, `GC_`, or `DOLT_` variables and pinned test `bd` 1.3.1 from `deps.env`; the run used its explicit private HOME and container environment on the tested scratch tree.
