# Release gate: systemd drift test unit directory (ga-oyyeq5)

**Verdict:** **PASS**

Reviewed source: `05d9f05e6454cc920f99081c098edf13384dd86b` (review `ga-q1mu2s`, round 2 PASS). Base: `origin/main@85c73ed6d2587d85567b00d92fb43e1180ecacf4`. The tested merge tree is `6550c1696cc7d6dc3eec42b7034fe8432a55a37b`.

| # | Result | Evidence |
| --- | --- | --- |
| 6. Clean merge | **PASS** | The reviewed source merges without conflict into the stated base; the materialized merge tree builds and vets cleanly. |
| 1. Review | **PASS** | `ga-q1mu2s` records round 2 PASS on the exact reviewed commit, with the prior comment findings resolved. |
| 2. Acceptance | **PASS** | `systemdUserUnitDir()` now selects `$XDG_RUNTIME_DIR/systemd/user`, which the user manager searches under an overridden HOME. Cleanup is registered before daemon reload and start. The named systemd restart test passed and reported a 5.6 s restart against its 15 s budget. |
| 3. Tests | **PASS (one attributed failure)** | Full 40-job `make test-local-full-parallel` through `load-gate-run.sh` and `isolated-test-run.sh` on the materialized merge tree: 39 job PASS, 1 job FAIL, 0 job SKIP observed. The sole failure is the tracked, unrelated `TestPhase2StartupOutcomeBounds/zcode/tmux-cli/Failed` timing bound (2.287268895 s > 2.02 s); attribution is below. The diff-owned `TestStartDrift_SystemdManaged_RestartsToNewBuildID` was selected by passing full-suite shard `integration-rest-full-5-of-8` and separately reported a named PASS with `-count=1 -v`. No waiver was used. |
| 3a. Failure attribution | **PASS** | Tracker `ga-hvjhay` predates this run and names the exact `WC-BRINGUP-001` signature; this sighting was appended and verified. Clause 1: the diff modifies only `test/integration/start_drift_test.go`, not the failing test. Clause 3(a): the failing `internal/worker/workertest` package does not import the changed integration test; the changed test cannot affect its startup timing. Clause 4: no path overlap. The candidate is fix-carrying (`ga-2qe684` fixes `ga-ltjdum`), while the separate condition's fix `ga-e4bhca` remains open and unlanded, satisfying the documented repeat-failure exception. |
| 3b. Policy and lint | **PASS** | `make test-ci-policy`, `make check-docs`, `go build ./...`, and `go vet ./...` passed on the merge tree. |
| 3c. CI configuration | **PASS** | The reviewed diff changes no CI job, matrix, timeout, or required-check list. `ci_lane_run: n/a`. |
| 4. High findings | **PASS** | Review `ga-q1mu2s` has no unresolved high-severity finding. |
| 5. Clean branch | **PASS** | The materialized merge tree and deployer worktree were clean before this gate record was written. |
| 7. One theme | **PASS** | The reviewed commit set changes only the systemd drift integration test. |

`test_cmd_scope: full-suite`  
`test_cmd: make test-local-full-parallel` (wrapped by `load-gate-run.sh` and `isolated-test-run.sh`)  
`test_counts: 39 PASS, 1 attributed FAIL, 0 observed SKIP` (job outcomes; the non-verbose suite does not enumerate individual skipped tests)  
`diff_tests_executed: TestStartDrift_SystemdManaged_RestartsToNewBuildID PASS, 0 FAIL, 0 SKIP`  
`waiver_ref: none`  
`policy_lane: make test-ci-policy PASS`  
`load_threshold: 15`  
`load_waited_seconds: 450`  
`load_wait_timed_out: 0`  
`load_start: 33.89`  
`load_max: 54.97`  
`load_mean: 27.62`

The full-scope raw exit was 2 because the runner preserves the attributed shard failure. The prior attempt with a private HOME lacking `.zshrc` was interrupted after an unrelated tmux shell test encountered zsh's first-run menu; the qualifying full run used a private HOME with `.zshrc`, and that tmux shard passed. The full and focused logs were captured at `/var/tmp/gc-gate-ga-oyyeq5-rerun-full.log` and `/var/tmp/gc-gate-ga-oyyeq5-diff-test.log`.
