**Verdict:** **PASS**

# Proxied bd init retry release gate

- Deploy bead: `ga-x0ek0e`; build bead: `ga-78tq9n`; review bead: `ga-kmnnu6` (PASS).
- Reviewed source: `41542669ee72f9aaa084b4bd700ef7e984501b9f`. Its source branch is provenance only; the publication target is isolated `deploy/ga-x0ek0e-gate`.
- Test snapshot: `origin/main@cb130844e27304a6d029d3fa1a99eba966f9f384` plus reviewed source, materialized as scratch merge commit `28c47957bf1fbc364a36a81bad8b5ff8847bae27` with tree `781945588edd44fc64f789c1a5c254d43bb902ea`.
- Freshness check before publication: `origin/main@0bf48101d7b722d034726b182bf1aab47024e11d`; merge-tree exit 0, tree `bfbbce09bdde99515e306407ef69a1fb96917c81`. Main's changes since the test snapshot leave the fixture source, `deps.env`, `go.mod`, and `go.sum` unchanged; they update `examples/gastown/BUILD.bazel` as part of narrowing Bazel test data. Tests ran on the frozen snapshot above. PR CI will evaluate its own current merge candidate.
- Mode: remote; push remote: `fork`. Target source is not already on main. No rebase was needed. The ancestry guard passed when given this deploy bead and its confirmed build bead, which the reviewed source commit cites. No stacked PR.
- Issue: [#7234](https://github.com/gastownhall/gascity/issues/7234), filed with reproduction, impact, risk, and verification plan.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1. Reviewed | PASS | Reviewer `ga-kmnnu6` passed the exact source with no blocker; the recorded SHA resolves to a commit. |
| 2. Acceptance | PASS | The fixture retries only proxied `bd init` on pinned bd v1.3.1's exact proxy-start timeout, first signals that temporary scope's recorded proxy/backend PIDs and removes incomplete `.beads`, then permits one more attempt. Four new subtests passed by name. The real proxied topology test passed in the full suite; reviewer fault injection demonstrated base FAIL and changed source PASS under a port collision and a boot stall. |
| 3. Tests | PASS | `make test-local-full-parallel` completed all 40 jobs on the materialized merged tree, through the load gate and `isolated-test-run.sh`, with pinned bd v1.3.1 and a rootless Podman socket. Retained logs: **57,748 PASS, 0 FAIL, 235 SKIP**; the one diff-owned test passed by name, including its four subtests. See details below. |
| 4. Findings | PASS | Reviewer recorded zero unresolved high-severity findings; non-blocking fixture cleanup suggestions remain informational. |
| 5. Clean tree | PASS | Materialized merge scratch was clean after the run; the isolated deploy branch is checked again after the gate commit before publication. |
| 6. Main divergence | PASS | Merge-tree against the frozen test base and the fresh pre-publication base both returned 0. `go build ./...` and `go vet ./...` passed on the materialized merged tree. No conflict or self-rebase. |
| 7. Theme | PASS | The single reviewed commit changes only `examples/gastown/maintenance_bd_integration_test.go`, a single integration-fixture resilience fix. |

## Criterion 3 detail

- `test_cmd: make test-local-full-parallel`; `test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`.
- `test_counts: 57748 PASS, 0 FAIL, 235 SKIP`, counted from 40 retained per-job logs by `gate-test-evidence.py`; 36 logs contain per-test result lines and four utility jobs have no test results. The runner reported `All full jobs passed` and detached unit exit 0.
- `diff_tests_executed: TestRetryOnBdProxyStartTimeoutRetriesOnlyKnownSignature PASS (integration-packages-core-2-of-4.log)`; its four subtests all PASS. `TestMaintenanceOrdersOnRealBdTopologies/proxied_city_and_proxied_rigs` PASS in the same shard. Diff ownership was computed by `diff-owned-tests.py` against the frozen base and reviewed source.
- `skip_justification`: no diff-owned test or subtest skipped. The skips are unchanged tests for optional platform and service conditions, explicit goldens/tooling opt-ins, and unavailable external fixtures. In the changed package's shard, the skipped `TestTmuxKeybindingsAlternateScreenPassthrough` lives in unchanged `gastown_test.go` and says the embedded pack lacks a future `#{alternate_on}` marker until its `go.mod` pin changes. The changed fixture's own retry and all real topology bodies ran and passed. Those skip conditions cannot be reached by this test-only retry helper.
- `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-x0ek0e.c3/logs`; evidence file: `/var/tmp/gc-heavy-gate/runs/ga-x0ek0e.c3/test-evidence.txt`. Logs retained.
- `policy_lane: PASS` — pinned golangci-lint 2.12.0; `make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary check-docs` through `gate-base.sh run` on a fresh tree (`/var/tmp/ga-x0ek0e-policy.log`).
- `drift_lane: PASS` — `make bazel-sync` then `git diff --exit-code` on a separate fresh tree (`/var/tmp/ga-x0ek0e-bazel-drift.log`).
- `ci_lane_run: n/a`; this diff changes no CI configuration. `failure_attribution: none`.
- `load_threshold: 15`; `load_waited_seconds: 0`; `load_wait_timed_out: 0`; `run_start_load: 12.59`; `run_max_load: 55.57`; `run_mean_load: 32.87`; `run_readings: 59`. The run began below threshold; the later host load rose while concurrent shards ran. The load gate reported no read errors.

## Publication checks

- The first ordinary push failed in the default Bazel hook (`bazel test //... --config=fork-cache --keep_going`, invocation `3d15679d-72f1-41a7-be3d-b66cbebaea53`): 118 targets passed, 73 failed locally. Logs showed the tracked ICU74 loader abort (exit 127, `ga-sdon3o`) and an additional killed Git subprocess in unchanged `TestFreshMergeActionBehaviour/base_branch_cannot_be_fetched` (`ga-vkhfnj`). Sightings were appended to both trackers. This attempt published no branch; no attributed hook bypass was used.
- The hook's documented `GC_PREPUSH_SUITE=go` mode then passed all ten fast jobs and pushed the isolated branch. The detached run completed with exit 0 at `/var/tmp/gc-heavy-gate/runs/ga-x0ek0e.push-go`. Counts from ten retained logs: **28,421 PASS, 0 FAIL, 158 SKIP**. The skips are the fast tier's process/integration exclusions and unchanged optional fixture conditions. This supplementary tier excludes the integration-tagged diff-owned test; criterion 3 uses the complete 40-job run above, where that test and the real topology tests executed and passed.
- `TestFreshMergeActionBehaviour` passed in the Go hook (0.59s), including the previously killed subtest (0.07s). No Bazel failure was accepted as a passing result.
- Hook logs are retained at `/var/tmp/gc-heavy-gate/runs/ga-x0ek0e.push-go/logs`. The fast target stripped the requested `LOCAL_TEST_LOG_DIR`; all ten active log files were identified through this service's cgroup and preserved through open file handles before the runner's cleanup.
- Hook load evidence: `load_threshold: 15`; `load_waited_seconds: 1801`; `load_wait_timed_out: 1`; `run_start_load: 31.40`; `run_max_load: 30.82`; `run_mean_load: 28.43`; `run_readings: 14`; `wait_max_load: 41.33`; `read_errors: 0`. The ordinary bounded path proceeded after its wait timed out. The reported run maximum excludes the initial reading.
