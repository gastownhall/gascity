# Release gate: semantic `cmdStop` test deflake

- Deploy bead: `ga-imv1fb`
- Build bead: `ga-4zsjqm`
- Review bead: `ga-lqkf81`
- Reviewed source: `16302822b2db29c899af3a8240596f539076c9e2`
- Base: `origin/main@145cc2be9b2be3b16aedfd64fe884820a27c6d3e`
- Deploy mode: remote
- Push remote: `origin`
- Gate result: **PASS**

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-lqkf81` records `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | **PASS** | Both semantics-only tests now avoid arbitrary positive outer timeout inputs; force escalation retains interrupt/stop/exit assertions, and the shutdown-error test observes asynchronous completion through the existing hang detector while retaining the same error/output assertions. Dedicated timeout-owner tests, the justified city-runtime latency test, and the package `hangBudget` are untouched. Builder evidence records 20/20 loaded-host runs. |
| 3 | Tests pass | **PASS with attributed failures** | Fresh full-scope run completed 36/40 jobs PASS and 4/40 FAIL with 4 failing functions and 0 observed SKIP. Both diff-owned tests were selected in green full-suite shards and independently passed by name. Every raw failure has a tracker predating this run, is outside `cmd/gc/cmd_stop_test.go`, and cannot be caused by this test-only diff. |
| 3b | Policy/lint lane | **PASS with attributed baseline failure** | Policy, docs, build, vet, native-DoltLite, changed formatting, and affected lint pass. The tracked native binary-size baseline remains above its ceiling (`270281232 > 270000000`, `ga-5flk3r`); a test-only diff cannot change the `gc` binary dependency graph. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead records no HIGH, security, or spec finding. Its comment-precision observation is explicitly non-blocking and does not affect behavior or assertions. |
| 5 | Final branch clean | **PASS** | The exact reviewed source was clean before this gate record; `git diff --check origin/main...HEAD` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main HEAD` returned no conflicts and tree `9776599711ddf4ed5789a211b610b40319623d73`. Source is 8 commits behind and 1 ahead of main. |
| 7 | Single feature theme | **PASS** | One commit modifies one test file to deflake two related semantics-only `cmdStop` cases while preserving timeout ownership. |

The mandatory deploy-source ancestry audit passes:

```text
assert_deploy_ancestry_scope origin/main 16302822b2db29c899af3a8240596f539076c9e2 ga-imv1fb ga-4zsjqm ga-lqkf81
ANCESTRY_SCOPE=PASS
```

## Acceptance evidence

- `TestCmdStopForceEscalatesInProgressControllerStop` passes `0` to both
  `cmdStop` invocations while retaining the existing interrupt, force-stop,
  completion, and exit-code assertions.
- `TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails` invokes
  `cmdStop(..., 0, false)` asynchronously and bounds observation with the
  existing package helper; the shutdown-error and false-success assertions are
  unchanged.
- The reviewed diff has only two hunks in `cmd/gc/cmd_stop_test.go`; it does not
  touch either dedicated wall-clock timeout owner, the city-runtime latency
  assertion, the separate held force-delegation recurrence, or `hangBudget`.
- Builder and reviewer evidence records 20/20 clean loaded-host package runs.

## Test evidence

- `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 36 PASS jobs, 4 FAIL jobs, 0 observed SKIP jobs`
- `failing_test_functions: 4`
- `full_log: /var/tmp/ga-imv1fb-full-suite-20260825.log`
- `shard_logs: /var/tmp/gc-local-tests.QRMWtI`
- `diff_tests_executed: TestCmdStopForceEscalatesInProgressControllerStop PASS in cmd-gc-process-1-of-6 and integration-packages-cmd-gc-3-of-6; TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails PASS in cmd-gc-process-3-of-6 and integration-packages-cmd-gc-5-of-6`
- `named_check: go test -count=1 -v ./cmd/gc/... -run '^(TestCmdStopForceEscalatesInProgressControllerStop|TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails)$' — 2 PASS, 0 FAIL, 0 SKIP`
- `waiver_ref: none`

| Raw failing test(s) | Tracker | Attribution evidence |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Installed-`bd` flag-manifest drift. Candidate changes only a separate test file and cannot reach `internal/bdflags`. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-afqddr` | Exact host-tmux empty-default-binding class. Candidate changes only `cmd/gc/cmd_stop_test.go` and cannot reach `internal/runtime/tmux`. |
| `TestCleanInstallTutorialPath` | `ga-2ywyyf` | Exact fresh-temp-rig pre-existing `.beads` store signature. Tracker predates this run; the candidate changes no production code or suite target and cannot affect the integration fixture's isolated filesystem. |

`failure_attribution`: each failure satisfies clause 3(a), structural mechanism,
and has no path overlap. This diff changes test code only, adds no test target,
and does not alter resource-census baselines.

## Policy and static evidence

```text
make test-ci-policy                                      PASS
make check-gomod-replace                                 PASS
make check-eventexport-isolation                         PASS
make check-core-boundary                                 PASS
make check-docs                                          PASS
go build ./...                                           PASS
go vet ./...                                             PASS
make test-native-doltlite-beads                          PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=HEAD^ make fmt-check-changed  PASS
LINT_CHANGED_REF=HEAD^ make lint-affected                PASS (0 issues)
make check-native-dependency-surface                     FAIL-ATTRIBUTED: 270281232 > 270000000 (`ga-5flk3r`)
```

## Release disposition

**Gate PASS.** Cut isolated branch `deploy/ga-imv1fb-gate` from the exact
reviewed source, commit this checklist, push the isolated branch, open the PR,
publish exact-head deploy clearance, and route the merge-request to the merge
authority. The deployer does not merge.
