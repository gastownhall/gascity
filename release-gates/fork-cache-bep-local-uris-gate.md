# Release gate: fork-cache BEP local URIs

- Deploy bead: `ga-i8q4rf`
- Review bead: `ga-ac050o`
- Source bead: `ga-ifvl9f`
- Reviewed commit: `547ba9d8894cc39069e24e882e03ab3f7f80f8eb`
- Gate base: `origin/main@7146bfe3141ba3fad41f732dde5cf7f86514e7a4`
- Synthetic merge commit: `c63bf43f2a3faa862f70edbf96cc10950f59d9db`
- Synthetic merge tree: `1340a5c4f2d65db27cc864fb7b9dab8d29946f3e`

**Verdict:** **PASS**

`docs/PROJECT_MANIFEST.md` is absent in this checkout. This gate applies the
seven criteria from the active deployer protocol and the full-suite command
documented in `TESTING.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-ac050o` is closed with `verdict: pass`, `review_round: 1`, and `deploy_commit: 547ba9d8894cc39069e24e882e03ab3f7f80f8eb`. |
| 2 | Acceptance criteria met | PASS | The reviewed diff adds `--nobuild_event_json_file_path_conversion` and `--nobuild_event_binary_file_path_conversion` only to `build:fork-cache`, with three negative guard cases in `TestBazelForkCacheConfig`. The guard passed in the full suite, and the dispatched fork-cache lane completed successfully. |
| 3 | Tests pass | PASS | Detached `make test-local-full-parallel` completed all 40 jobs: 57,579 PASS, 0 FAIL, 235 SKIP. `TestBazelForkCacheConfig` passed by name in both the unit and integration core logs. The affected `bazel-test.yml` fork-cache workflow also passed. |
| 3a | Pre-existing failures | PASS | There were 0 failures to attribute. The 235 skips are environment, platform, opt-in, or helper-process cases outside this diff; none of the six tests in the sole touched test file skipped. The sole diff-owned test passed in two jobs. |
| 3b | Policy, lint, generated-file drift | PASS | Detached fresh-tree policy/lint run `/var/tmp/gc-heavy-gate/runs/ga-i8q4rf.policy` exited 0 with pinned golangci-lint 2.12.0 and 0 issues. Detached generated-file run `/var/tmp/gc-heavy-gate/runs/ga-i8q4rf.drift` ran `make bazel-sync` followed by `git diff --exit-code` on a separate fresh tree and exited 0. `go build ./...`, `go vet ./...`, and `git diff --check` also exited 0 on the pinned merge tree. |
| 3c | Affected CI lane | PASS | Dispatched `bazel-test.yml` with `rbe=cache` at the reviewed SHA: [run 37433570086](https://github.com/gastownhall/gascity/actions/runs/37433570086). Overall conclusion and both jobs are `success`. The 3,216-line side-by-side job log has no `Lost inputs no longer available remotely`, `PERMISSION_DENIED`, or cache breaker error. |
| 3d | Heavy-package composite | PASS | `heavy-composite-gate.sh classify` returned `mode=none` for this two-file diff. The ordinary full-suite path applies. |
| 4 | No high-severity review findings open | PASS | The review bead records no blocker, major, HIGH, or security finding. |
| 5 | Final branch is clean | PASS | The release-gate record is committed on the isolated deploy branch; the post-commit worktree check is clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree` against pinned base `7146bfe3141ba3fad41f732dde5cf7f86514e7a4` exited 0 and produced tree `1340a5c4f2d65db27cc864fb7b9dab8d29946f3e`; `go build ./...` and `go vet ./...` passed there. Recheck against current `origin/main@f4efff72cbe402d3422cd3010138429770d529f0` also exited 0 and produced tree `6dd12f2c0dbbc17b721f1f1e1044920b5c79e0a2`. Main's intervening paths do not overlap the two-file reviewed diff. |
| 7 | Single feature theme | PASS | The one reviewed commit changes only `.bazelrc` and `scripts/bazel_fork_cache_test.go` (16 added lines) for the fork-cache BEP URI behavior. |

## Test evidence

- `test_cmd_scope: full-suite`
- `test_cmd: make test-local-full-parallel` through the detached, load-gated runner, with `LOCAL_TEST_JOBS=2` and `GOFLAGS=-v`
- `test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-i8q4rf.c3-separated/logs`
- `test_counts: PASS=57579 FAIL=0 SKIP=235` across 40 retained logs (36 with test result lines; four compile/self-check jobs have no Go test result lines)
- `diff_tests_executed: TestBazelForkCacheConfig PASS` in `unit-core.log` and `integration-packages-core-3-of-4.log` (sole diff-owned test identified by `diff-owned-tests.py`)
- `skip_justification: all 235 top-level skips are outside the reviewed test body and reflect platform-specific cases (Darwin, root or secondary UID), opt-in/live integration settings, missing external fixture/tooling prerequisites, or subprocess/helper scenarios; none of the six tests in scripts/bazel_fork_cache_test.go skipped. The full reasons remain in the retained per-job logs.`
- `waiver_ref: none`
- `policy_lane: PASS` (pinned golangci-lint 2.12.0 and policy checks)
- `drift_lane: make bazel-sync && git diff --exit-code — PASS`
- `ci_lane_run: dispatched` ([run 37433570086](https://github.com/gastownhall/gascity/actions/runs/37433570086)); overall result `success` at exact reviewed SHA
- `heavy_mode: none`
- `load_threshold: 15`; `load_waited_seconds: 1800`; `load_wait_timed_out: 1`; `run_start_load: 55.81`; `run_max_load: 65.08`; `run_mean_load: 36.35`; `run_readings: 115`; `wait_first_load: 36.98`; `wait_max_load: 55.81`; `wait_mean_load: 27.57`; `wait_readings: 61`; `read_errors: 0`

The full suite runs on the materialized synthetic merge at
`/var/tmp/gc-merge-ga-i8q4rf.7YVxWf`, with rootless Podman at
`unix:///run/user/1000/podman/podman.sock`, Ryuk disabled, a private Go build
cache at `/var/tmp/gi8q4rf`, and normal short on-disk test temp paths at
`/var/tmp/gotmp`. The detached run
`/var/tmp/gc-heavy-gate/runs/ga-i8q4rf.c3-separated` survives session recycle.
Its load gate waited 1800 seconds for the five-minute host load to fall below
15, timed out, and started the suite at 08:26:53 UTC. The timed-out wait is
recorded above; the suite itself finished with all 40 jobs passing.

Earlier suite attempts ended before providing test evidence: the first lost
shared Go cache archives while `gocache-reaper.service` trimmed the cache
(tracked by `ga-euqey1`), and two private-cache setup attempts used unsuitable
test temp paths. Their logs remain in their respective run directories. They
are not counted as test PASS or diff-owned failure evidence.
