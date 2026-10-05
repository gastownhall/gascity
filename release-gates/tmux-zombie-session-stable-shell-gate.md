**Verdict:** **PASS**

# Stable tmux zombie-session test release gate

Deploy bead `ga-3945eg`; review bead `ga-peo49r` (PASS); reviewed source `39b9ce3744078b981e5c87b2b08c6b5c1e07ca51`. Remote mode; push target is `fork`. The gate ran on the clean materialized merge `dab7ff28f9565d8f58b55b93b5ee05fe08595bd5`, tree `899cae68dc04907e72b2776bfedebcdb67f3acf7`, with frozen `gate_base_sha: e38ce9cc55d15fa351199c042f0d07aba1a347af`. Later `origin/main` tips `188e271eefcd3a7ffdd9e29b8a772ca295151a0c` and `f44152a7934dfe6b3312a72685e9fc7598478547` also merge cleanly (latest tree `7d464926199d0c14930862941b36173de1c87afd`). No self-rebase or waiver.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | `ga-peo49r` records reviewer PASS for the exact source SHA. No carryover is used. |
| 2 | PASS | The changed integration test now starts a plain shell, waits for `pane_current_command=sh`, and verifies that `EnsureSessionFresh` changes the pane PID. The fixed-sleep ledger is reduced together from 489 to 488 in code, TOML, and `TESTING.md`. The ledger test and all five tmux-manifest tests passed in the full suite; the changed test passed under a slow zsh HOME 3/3. |
| 3 | PASS | The full 40-job suite, fast policy and drift lanes, and triggered CI-path supplement passed. The changed test ran and passed; no diff-owned test skipped. See below. |
| 4 | PASS | Review `ga-peo49r` reports no unresolved HIGH finding. |
| 5 | PASS | Role worktree and merge scratch were clean before the gate record was written. Recheck the isolated branch after committing the record. |
| 6 | PASS | `git merge-tree --write-tree --no-messages` returned 0, and `go build ./...` plus `go vet ./...` passed on the frozen merged tree. A later current-base merge-tree check also returned 0. |
| 7 | PASS | `origin/main...source` contains only the tmux test and three matching fixed-sleep ledger edits. The stack-aware ancestry guard passed for confirmed related build/fix IDs `ga-3yauld`, `ga-5ioiw9`, `ga-kmwwcx`, with no forbidden path or second theme. |

## Criterion 3 evidence

- `test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 GOFLAGS=-v GO_TEST_TIMEOUT=30m LOCAL_TEST_LOG_DIR=/var/tmp/gate-ga-3945eg-suite-logs TMPDIR=/var/tmp/gotmp make test-local-full-parallel`
- `test_cmd_scope: full-suite`; `heavy_mode: none` from `heavy-composite-gate.sh classify`; detached run `/var/tmp/gc-heavy-gate/runs/ga-3945eg.c3` ended `GATE_RUN_EXIT rc=0 state=complete`; 40/40 jobs passed.
- `test_counts: 102990 PASS, 0 FAIL, 335 SKIP` across all result lines, including subtests; top-level lines: 56860 PASS, 0 FAIL, 236 SKIP. The first detached attempt failed in setup because the log directory was absent; all jobs stopped before tests ran. After creating that directory, the same pinned full-suite command was rerun successfully.
- `diff_tests_executed: internal/runtime/tmux TestEnsureSessionFresh_ZombieSession PASS` in `integration-packages-runtime-tmux-1-of-3.log` (0.14s). It did not fail or skip. The ledger test and all five tmux-manifest tests also passed in the full suite.
- `skip_justification: none of the 12 top-level runtime-tmux skips is diff-owned. Six unchanged skip sites in tmux_test.go were checked with diff-owned-tests.py --site (line_in_hunk=no, func_touched=no); six are in unchanged files. The new helper functions are called only by the changed test and cannot reach any skipped test. Other skips are in unrelated packages or explicit platform/opt-in paths.`
- `load_threshold: 15`; `load_waited_seconds: 0`; `load_wait_timed_out: 0`; `run_start_load: 13.04`; `run_max_load: 24.55`; `run_mean_load: 19.37`; `run_readings: 63`. Wait-only: first/max/mean 13.04, one reading; read errors 0.
- `test_bd: pinned v1.3.1`; `ref_check: match:c1c4b642a`; preexisting binary `/home/jaword/.local/bd-versions/v1.3.1/bd`; source pin `deps.env`. Podman socket active and required Dolt image cached before the suite.
- `policy_lane: PASS` via `fresh-tree-run.sh` and frozen `gate-base.sh run`: `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary lint-affected fmt-check-changed check-docs`; pinned golangci-lint 2.12.0, zero issues. `make check-hooks` also passed.
- `drift_lane: PASS`: fresh-tree `make bazel-sync`, then `git diff --exit-code`; no modified or untracked files. Fresh tree record used the frozen merge commit.
- `supplemental_acceptance: PASS`: `make test-native-doltlite-beads`; `go vet -tags integration ./internal/runtime/tmux`; 3/3 runs of the changed test with a two-second zsh login-profile delay.
- `ci_path_jobs: PASS` local equivalents for required `Preflight / acceptance A` (576.979s), `Beads / topology acceptance` (511.757s), `Beads / proxied-native acceptance` (85.816s), `Worker core` (four profiles), and `Worker core phase 2` (four profiles). Detached run: `/var/tmp/gc-heavy-gate/runs/ga-3945eg.ci-supplement`, `GATE_RUN_EXIT rc=0 state=complete`; load start 8.77, max 9.14, mean 8.13. The two legacy-server migration cases skipped because the legacy binary is absent, matching the CI job's documented exception. No supplement test failed. The full suite already covered required `cmd/gc process` shards (with `GC_FAST_UNIT=0`) and all integration packages, including the complete runtime-tmux manifest partition. The local runner uses three tmux shards whereas CI uses six; both partition the same manifest, and `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce` passed.
- `ci_lane_run: n/a`; no CI config, matrix, timeout, or required-check diff.
- `failure_attribution: n/a`; `waiver_ref: none` (Gas City has no waiver path).

Raw full-suite logs: `/var/tmp/gate-ga-3945eg-suite-logs/`. Frozen merge scratch: `/var/tmp/gc-merge-validate.ga-3945eg.Bf9AGD`.
