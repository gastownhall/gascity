# ACP orphan-child ordering release gate

**Verdict:** **PASS**

- Deploy bead: ga-6q9nmky
- Review bead: ga-qaamctg; originating fix: ga-wohzib
- Reviewed source: `0ee987b7e6d004068b16eb62c9bfb691a265cc2f` (resolved as a commit)
- Deploy branch: `deploy/ga-6q9nmky-gate`; source branch is provenance only
- Deploy mode: remote; push remote: fork; base repository: gastownhall/gascity
- gate_base: `903df100f46620ae642406c06f7447bc0e05db26`
- Tested merge: `5c47572dd04f0d987af8ecb4481097b2b4bb2e17`
- Tested tree: `471bf3de718b70fec23f3e109e28f5d4fb090429`
- Evaluation: 2026-10-12 UTC

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS. ga-qaamctg records `verdict: pass` for the exact reviewed source, with no uncovered criteria. No review carryover used. |
| 2 | Acceptance criteria met | PASS. The tool child reports its PID through a FIFO after `setsid`; startup waits for that report before agent teardown. The child retains a stable shell identity, and cleanup kills its own process group. Existing scanner and orphan-reaping assertions remain intact. All ten tests in the affected file passed independently. The warmup portion is already on main and absent from this reviewed diff. |
| 3 | Tests pass | PASS. All four required Bazel lanes passed on the recorded merge tree: 288 passed targets, 0 failed targets, 0 skipped targets. Supplementary full ACP package: 344 passed Go cases including subtests, 0 failures, 0 skips. Details below. |
| 4 | No high-severity review findings open | PASS. Review notes contain no unresolved HIGH findings; considered helper error handling and timeout concerns were explicitly nonblocking. |
| 5 | Final branch is clean | PASS. The reviewed source checkout was clean; the sole deployer addition is this committed gate record. Final clean status is verified after committing it. |
| 6 | Branch diverges cleanly from main | PASS. The materialized merge against the pinned gate base had no conflicts, and `go build ./...` plus `go vet ./...` returned 0. A further clean merge against main `1c2f68718f54be8261d18a60c8632b1be4313698` also built and vetted successfully (scratch merge `80adb898bc4dc2f1ce3e2ca799527e9af3054ca8`). The full-suite evidence remains tied to the original pinned base. |
| 7 | Single feature theme | PASS. One reviewed commit changes only `internal/runtime/acp/proctable_linux_test.go` (+39/-26). No production code, dependencies, CI configuration, unrelated commits, or internal agent configuration are included. Ancestry scope check passed with the confirmed originating fix IDs. |

## Test evidence

The stale `docs/PROJECT_MANIFEST.md` and work-package references do not exist in this tree. Commands and required coverage were derived from `TESTING.md`, the Makefile, and `.github/workflows/bazel.yml`.

`test_cmd_scope: full-suite`

Common Bazel flags: `--config=remote-exec --config=ci --keep_going --test_output=all`, with a retained BEP JSON file per lane. No test-name filter was added.

| Required CI lane | Command | Target results |
|---|---|---|
| Unit, nogo, formatting, generated artifacts, policy, dashboard | `make check` (Bazel `//...`) | 251 PASS / 0 FAIL / 0 SKIP |
| Acceptance, including solo tests | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 7 PASS / 0 FAIL / 0 SKIP |
| Integration packages, including process-backed CLI tests | `bazel test --config=integration //test:integration_packages` | 29 PASS / 0 FAIL / 0 SKIP |
| Integration smoke | `bazel test --config=integration-smoke //test/integration:integration_test` | 1 PASS / 0 FAIL / 0 SKIP |

- test_counts: **288 PASS / 0 FAIL / 0 SKIP Bazel targets**, summed from each lane's `testSummary` events.
- supplementary_test_cmd: `go test -count=1 -v ./internal/runtime/acp`, on the same recorded merge tree.
- supplementary_test_counts: **344 PASS / 0 FAIL / 0 SKIP Go cases**, comprising 222 top-level tests and 122 subtests.
- diff_tests_executed: **none (no diff-owned test bodies)**. `diff-owned-tests.py --explain` reports five SHARED-HUNKS in the two helpers and their comments.
- affected_tests_executed: **10/10 PASS**, verified by `gate-test-evidence.py` against the retained verbose ACP log:
  - `TestFindRuntimesBySessionIDMarksLiveSessionTracked`
  - `TestFindRuntimesBySessionIDIgnoresOtherSessions`
  - `TestProviderRestartReportsAndReapsOrphanedRuntimes`
  - `TestSeamBackedProviderForwardsProcessTableScanner`
  - `TestTerminateRuntimeRefusesInitAndInvalidPIDs`
  - `TestFindRuntimesBySessionIDIgnoresDeadConn`
  - `TestFindRuntimesBySessionIDDuringHandshake`
  - `TestFindRuntimesBySessionIDTracksForeignOwnedSession`
  - `TestFindRuntimesBySessionIDTracksPerSessionAcrossSessions`
  - `TestFindRuntimesBySessionIDTracksOwnedHandshakeBeforeSocketBind`
- skip_justification: no skipped Bazel targets or ACP cases. Condensed Bazel logs do not provide a complete individual Go-case census for the other packages; the target counts above are explicitly target counts.
- test_log_dir: `/var/tmp/gc-heavy-gate/runs/ga-6q9nmky.c3/logs` (450 retained logs).
- test_bd: pinned v1.3.1 at `/home/jaword/.local/bd-versions/v1.3.1/bd`, verified against the repository's `deps.env` by the isolation wrapper.
- heavy_mode: none; classifier selected no heavy package.
- ci_lane_run: n/a (no CI-config change); the required lanes above ran independently here.
- waiver_ref: none; failure_attribution: none.

The initial collector stopped after the successful unit command because BEP artifact URLs used `bytestream://`. All 335 unit logs were recovered from Bazel's local test-log directory, with no missing target. The unit lane was not rerun. A detached continuation completed the other lanes and the supplementary ACP check on the same merge tree. This was an evidence-tooling repair, not a test failure or attribution.

## Fast lane and load evidence

The fast lane passed before the suite was launched. Each fast lane ran in a separate fresh view and left the suite scratch untouched.

- policy_lane: `make fmt-check` through `run-pinned-lint.sh`, the four shell guards required by `make check`, and `make lint` through `gate-base.sh run`, all PASS. Nogo used remote execution.
- GLT_RECORD: `{"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}`
- GATE_BASE_RECORD: `base=903df100f46620ae642406c06f7447bc0e05db26 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE`
- FRESH_TREE_RECORD policy: `head=5c47572dd04f tree=471bf3de718b modified=0 untracked=0 ignored=0`
- drift_lane: `make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec`, `git diff --exit-code`, and no untracked files, PASS.
- FRESH_TREE_RECORD drift: `head=5c47572dd04f tree=471bf3de718b modified=0 untracked=0 ignored=0`

The suite ran through `load-gate-run.sh` and `isolated-test-run.sh` as detached systemd services. The prescribed ordinary wait expired; its timeout behavior permits the run and is recorded here. The continuation had no remaining wait budget because this gate had already spent 1803 seconds waiting.

| Load field | Initial unit segment | Continuation segment |
|---|---|---|
| load_threshold | 15 | 15 |
| load_waited_seconds | 1803 | 0 (continuation after exhausted bound) |
| load_wait_timed_out | 1 | 1 |
| run_start_load | 43.67 | 56.54 |
| run_max_load | 50.02 | 73.70 |
| run_mean_load | 48.64 | 66.03 |
| run_readings | 2 | 4 |
| wait_first_load | 48.83 | 56.54 |
| wait_max_load | 86.11 | 56.54 |
| wait_mean_load | 63.85 | 56.54 |
| wait_readings | 61 | 1 |
| read_errors | 0 | 0 |

Run directories:

- Initial: `/var/tmp/gc-heavy-gate/runs/ga-6q9nmky.c3`
- Continuation: `/var/tmp/gc-heavy-gate/runs/ga-6q9nmky.c3-continuation` (complete, command rc=0)
- Build/vet, policy, drift and collector evidence: `/var/tmp/gc-deploy-evidence.ga-6q9nmky.ZWoAyu`

Logs are retained. No merge has been performed; merge authority remains with MPR.
