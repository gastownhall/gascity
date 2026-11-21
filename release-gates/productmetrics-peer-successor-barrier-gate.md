# Product metrics peer successor test barrier release gate

**Verdict:** **PASS**

- Deploy bead: `ga-ehrtsh`; build bead: `ga-6yan9f`; review bead: `ga-76cs8m`.
- Reviewed deploy source: `cbbd287e9a7b6f988db0b412c4baf5ee1e363705` (resolved commit).
- Gate base: `c460554c9c6e5914f4af6e2bc587231ad7fc8434`; merged-tree scratch: `bb29d83c2a39ad29524f27cd7fc70438e5131687`.
- Latest checked `origin/main`: `03a389046bae2fed5e269caedc8200c0a940605e`; the reviewed commit still merges cleanly, with no intervening productmetrics path change.

| # | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | Reviewer verdict on `ga-76cs8m` names the exact deploy source and reports no blocking findings. |
| 2. Acceptance criteria | PASS | The three peer successor tests now wait for the existing uploader-lock hook after `beginDisable` completes its state write. The changed test file has no production-code or dependency change. The review independently reproduced the old CI failure on the base and verified the fixed tests under the same injected delay. |
| 3. Tests | PASS | Full-scope `make test-local-full-parallel` on the merged tree, through `load-gate-run.sh` and `isolated-test-run.sh`: all 40 jobs passed; 55,889 PASS, 0 FAIL, 236 SKIP test executions, counting top-level test result lines across the shard logs. All three diff-owned tests passed by name in both the unit and integration package tiers. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 4. High-severity findings | PASS | The reviewer recorded no blocking finding; unresolved HIGH count is 0. |
| 5. Final branch clean | PASS | The isolated deploy branch is clean after committing this gate record (`git status --porcelain` empty). |
| 6. Clean divergence from main | PASS | `git merge-tree --write-tree origin/main cbbd287e9a7b6f988db0b412c4baf5ee1e363705` succeeded against the latest checked main. The criterion-3 suite ran on the merge materialized against the pinned gate base. |
| 7. Single feature theme | PASS | One test-only file, `internal/productmetrics/control_unix_test.go`, changed for the same peer successor synchronization issue. |

Test evidence:

- `diff_tests_executed`: `TestDisableAndPurgeExactTokenConflictAndPeerCleanRecovery` PASS; `TestDisableAndPurgeRejectsUnprovenPeerSuccessor` PASS; `TestDisableAndPurgeRejectsPeerSuccessorReplacedDuringCleanProof` PASS. Each has a PASS line in `unit-core.log` and `integration-packages-core-3-of-4.log`.
- `skip_justification`: the 236 skips are outside the three changed tests. This diff changes only their synchronization helper and call sites; all diff-owned tests executed and passed. The remaining skips are from other packages' platform, optional integration, or helper conditions and do not cover this test-only change.
- `heavy_mode: none`; `ci_lane_run: n/a` (no CI configuration change).
- `test_bd`: pinned `v1.3.1`, `c1c4b642a`, verified by the test wrapper against `deps.env`.
- `load_threshold: 15`; `load_waited_seconds: 1801`; `load_wait_timed_out: 1`; `run_start_load: 32.52`; `run_max_load: 64.26`; `run_mean_load: 48.20`; `run_readings: 112`. The ordinary load gate proceeded after its bounded wait; the full suite passed under the recorded load.
- `policy_lane`: PASS on the merged tree for `test-ci-policy`, `check-gomod-replace`, `check-native-dependency-surface`, `check-eventexport-isolation`, `check-core-boundary`, `test-native-doltlite-beads`, `lint-affected`, and `fmt-check-changed`; pinned golangci-lint 2.12.0 reported 0 issues. `go build ./...` and `go vet ./...` also passed.
- Detached run: `/var/tmp/gc-heavy-gate/runs/ga-ehrtsh.c3`, `GATE_RUN_EXIT rc=0 state=complete`.
