# Release gate: herdr activity working readiness

**Verdict:** **PASS**

- Deploy bead: `ga-znndgi`; reviewed fix: `ga-8nndgz`; review: `ga-a34jv9`.
- Reviewed source commit: `702bf49c61a9ad8c14ab5d5aa904f25ee52d706d`.
- Gate base: `origin/main` at `8b756cbf779597c4d3bd4a42192b7c67aa1399a4`; materialized merge commit `6324828cf032e02afa99996511aa9a15af0a62fd`, tree `550778b32f26ef03ad0bd6da34aa5204e3095eb6`.
- Gate run: `/var/tmp/gc-heavy-gate/runs/ga-znndgi.c3`. Retained `test_log_dir`: `/var/tmp/gc-heavy-gate/runs/ga-znndgi.c3/logs`.
- `waiver_ref: none`. Criterion 3's one raw failure is attributed below; the raw result remains in the retained log.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | **PASS** | Reviewer bead `ga-a34jv9` passed the exact reviewed commit. No review carryover was needed. |
| 2 | Acceptance criteria | **PASS** | The live test's working-leg wait now requires a timestamp after the pre-transition idle stamp. A new hermetic test delays the tracker poll past the old 30 ms window and exercises the same helper. The reviewed diff is limited to `internal/runtime/herdr/activity_test.go` and `activity_live_test.go` (+45/-9); no production code changed. |
| 3 | Tests pass | **PASS (one attributed raw failure)** | `test_cmd_scope: full-suite`. The documented `make test-local-full-parallel` ran through `load-gate-run.sh` and `isolated-test-run.sh` on the materialized merge tree, with rootless Podman, the pinned Dolt image, pinned `bd`, private herdr config, and retained per-job logs. All 40 jobs finished. `gate-test-evidence.py` counted **57,622 PASS, 1 FAIL, 235 SKIP** across 40 logs. The single raw FAIL is `TestSessionEventPumpLiveHerdr` in `cmd-gc-process-5-of-6.log:16480-16482`; see attribution below. Both diff-owned tests have real PASS results: `TestActivityLive` in `integration-packages-core-3-of-4.log` and `TestActivityWorkingLegWaitsForObservedTransition` in that log and `unit-core.log`. The fast unit lane's expected opt-in SKIP for `TestActivityLive` is superseded by its live integration PASS. Remaining SKIPs are in unrelated platform, helper, or opt-in lanes; neither diff-owned test is left skipped. `TestTutorial01` also PASSed in `cmd-gc-process-6-of-6.log`. |
| 4 | No high-severity review findings | **PASS** | Reviewer recorded zero blocking or high-severity findings. |
| 5 | Final branch clean | **PASS** | Materialized merge scratch was clean after build, vet, fast lane, and suite. The isolated deploy branch is checked for a clean worktree after committing this gate record. |
| 6 | Clean divergence from main | **PASS** | `git merge-tree --write-tree` succeeded against pinned gate base, producing the exact materialized merge tree above. Main advanced during the run to `8d8a6be86089d3c8e6a2ec6c870a4e10c42db641` via unrelated doctor and doltorphan commits; a fresh `git merge-tree` against that ref also succeeded (tree `0b2eb9cb89c278d08906ec80591a45fab127b895`). |
| 7 | Single feature theme | **PASS** | Both reviewed commits cite fix bead `ga-8nndgz` and touch only herdr activity tests. The ancestry scope check accepted `ga-znndgi` and its confirmed fix bead, with no denied paths. |

## Criterion 3 detail

- `policy_lane: PASS` on a separate fresh tree: `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads`, with pinned golangci-lint 2.12.0. Log: `/var/tmp/ga-znndgi-policy.log`.
- `drift_lane: PASS` on a separate fresh tree: `make bazel-sync` followed by `git diff --exit-code`. Log: `/var/tmp/ga-znndgi-bazel-drift.log`.
- `go build ./...`, `go vet ./...`, and `git diff --check`: PASS on the materialized merge tree. No CI configuration was changed; criterion 3c is not applicable. `heavy_mode: none` from `heavy-composite-gate.sh classify`.
- `load_threshold=15`, `load_waited_seconds=0`, `load_wait_timed_out=0`, `run_start_load=12.61`, `run_max_load=41.88`, `run_mean_load=31.65`, `run_readings=80`. The runner reported `wait_first_load=12.61`, `wait_max_load=12.61`, `wait_mean_load=12.61`, `wait_readings=1`, `read_errors=0`.
- `test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env HOME=/var/tmp/gc-heavy-gate/runs/ga-znndgi.c3/home XDG_CONFIG_HOME=/var/tmp/gc-heavy-gate/runs/ga-znndgi.c3/home/.config DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOCACHE=/var/tmp/gc-heavy-gate/runs/ga-iufo27.c3-diskcache/build-cache TMPDIR=/var/tmp/gotmp SHELL=/bin/sh GOFLAGS=-v LOCAL_TEST_JOBS=4 LOCAL_TEST_LOG_DIR=/var/tmp/gc-heavy-gate/runs/ga-znndgi.c3/logs make test-local-full-parallel`.

### Attributed raw failure

`TestSessionEventPumpLiveHerdr` failed after 10.06 seconds because `ConfigureServer` did not become ready. Its private `HOME` path in this run exceeds the 30-byte boundary independently proven to overflow herdr 0.8.0's Unix socket path. The exact condition is tracked by open `ga-0og0ry`, created before this run; its designated fix bead `ga-yxizvd` has `gc.fixes_tracker=ga-0og0ry`, carries a blocked work outcome, and has not landed on `origin/main`. A sighting was added to the tracker and read back.

`failure_attribution: TestSessionEventPumpLiveHerdr -> ga-0og0ry | clause 1: not diff-owned (`cmd/gc` test, while the diff changes only two `internal/runtime/herdr` test bodies); clause 2: tracker predates this run and names this exact timeout; clause 3(d): the tracker records independent base-ref reproduction of the long-HOME socket-path condition; clause 4: no changed file in the failing test's package and no changed production code in its test binary. This deploy is fix-carrying: its `build_bead=ga-8nndgz` is stamped `gc.fixes_tracker=ga-fua7nj`, and the different condition's fix `ga-yxizvd` remains unlanded. The repeat-sighting exception therefore applies. The suite's nonzero exit and one raw FAIL are preserved, not replaced by a diagnostic green run.`
