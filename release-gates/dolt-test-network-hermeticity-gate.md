# Dolt test-home network hermeticity — ga-uhglip

**Verdict:** **PASS**

- Reviewed source: `7ee1dcf2b38cd79b115bc93d673a10337e0e0a8d`
- Gate base: `eec05755e197facf942b685b1b0d4ea63cd8bcbb`
- Tested merge candidate: `ae1f9ff24119e0723623f9532b7304ec8ed5a568`
- Latest checked `origin/main`: `789929fd87bc62f82695471743ed653d32f1dbe9`; `git merge-tree --write-tree origin/main <reviewed source>` succeeded with tree `72c3590208dcb4ba7339a3ce02635f4bbe588833`.
- `waiver_ref: none` (gascity has no waiver path).

| # | Verdict | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Review bead `ga-gkdppk` passed round 2 on the exact source SHA. No review carryover was used. |
| 2. Acceptance criteria | PASS | Test Dolt global config now seeds identity plus `versioncheck.disabled` and `metrics.disabled` through the shared helper. Test processes carry `DOLT_DISABLE_EVENT_FLUSH`; environments built from scratch add it explicitly. The shared-writer guard, black-holed proxy checks, and real bd lifecycle tests passed. The reviewer independently checked the RED/GREEN and mutation evidence. The census baselines are unchanged. PR text must cite sibling hermeticity PR #6974. |
| 3. Tests pass | PASS with one attributed raw failure | The documented full `make test-local-full-parallel` ran all 40 jobs on the pinned merge candidate: 39 job PASS, 1 raw job FAIL, 0 omitted. Preserved logs contain 56,264 top-level PASS, 1 raw FAIL, and 236 SKIP results. All 14 diff-owned tests reported PASS by name in their required lanes. The sole failure is attributed under 3a below. The two changed real-bd lifecycle tests also show redundant SKIPs in the integration package partition, which explicitly directs them to the process shard; both PASSed there. |
| 3a. Failure attribution | PASS | `TestGraphWorkflowFailureRunsCleanup` failed in `integration-rest-full-6-of-8` when `gc init` hit the fixed 2-second Dolt `user.name` config-probe timeout. Its body at `test/integration/graph_dispatch_test.go:107` is UNCHANGED. Open tracker `ga-vkhfnj` predates this run and contains the exact earlier signature through consolidated sighting `ga-davjgz`; this run is comment `fff72405-e148-5064-8e7f-06b72d24c0c4`. Clause 3 proof **b, cross-PR**: the exact signature occurred on `ga-vzckwr`, whose diff touched only tmux runtime and its manifest, and again on `ga-fiu7se`, whose diff touched only named-session code. Neither shares changed paths with this candidate. The candidate does touch the same test package's shared Dolt-config seed helper, so the stronger clause-4 guard is recorded: cross-PR proof landed; no new test file in `test/integration`, no census bump, and no new test target. This deploy's build bead `ga-z4jzmd` is `gc.fixes_tracker=ga-vkhfnj` stamped and its work record is `blocked`, so the tracker fix has not landed; the fix-carrying repeat exception applies. |
| 3b. Policy/lint and drift | PASS | On fresh views of the tested merge candidate: `make fmt-check`, pinned `make test-ci-policy`, affected lint against the gate base (golangci-lint 2.12.0, 0 issues), and `make bazel-sync` followed by `git diff --exit-code` all passed. `go build ./...` and `go vet ./...` passed on the merge candidate. |
| 3c. CI-config lane | PASS | No CI job, matrix, timeout, or required-check configuration changed. |
| 3d. Heavy package | PASS | `heavy-composite-gate.sh classify` reported `mode=none`. |
| 4. Review findings | PASS | Reviewer recorded no unresolved high-severity style, security, or spec findings. |
| 5. Final branch clean | PASS | The isolated `deploy/ga-uhglip-gate` branch contains the reviewed source and this gate record; post-commit `git status` is clean. |
| 6. Clean merge and integration build | PASS | The tested two-parent merge candidate materialized cleanly from the reviewed source onto the pinned base. Its build and vet passed. After main advanced, the reviewed source still merged cleanly into the latest checked `origin/main`. |
| 7. Single feature theme | PASS | All changed paths support one test hermeticity change: stop Dolt network checks and detached metrics forks in test environments. |

`test_cmd`: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 GOFLAGS=-v TMPDIR=/var/tmp/gotmp LOCAL_TEST_JOBS=4 make test-local-full-parallel`

`test_cmd_scope: full-suite`. The isolation wrapper reported pass-through for the scratch checkout; its environment contained no inherited `BD_`, `BEADS_`, `GC_`, or `DOLT_` selectors, and `scripts/test-local-parallel` starts every job with its pinned `env -i` allowlist. The pinned test bd was v1.3.1. Runner: `/var/tmp/gc-heavy-gate/runs/ga-uhglip.c3`; job logs: `/var/tmp/gotmp/gc-local-tests.J3OkwT`; `GATE_RUN_EXIT rc=2 state=failed` reflects the single attributed raw failure.

`test_counts: 56,264 PASS / 1 attributed raw FAIL / 236 SKIP` top-level test events; `39 PASS / 1 attributed raw FAIL / 0 omitted` jobs. The SKIPs are existing platform, opt-in, helper, and partition gates. Two are the duplicate integration-package selections of changed real-bd tests; their required process-shard executions both PASSed. No diff-owned test lacks a PASS result.

`diff_tests_executed` (all PASS): `TestClearProcessLiveEnvForTestsUnsetsInheritedState`; `TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace`; `TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses`; `TestSanitizedBaseEnv_CarriesDoltEventFlushDisable`; `TestRunSnapshot_Integration_RealDoltRoundTrip`; `TestInitDisablesDoltEventFlush`; `TestInitSkipsScrubInTestscriptSubcommandMode`; `TestOnlyTheSharedHelperWritesDoltGlobalConfig`; `TestNewEnvDisablesDoltEventFlush`; `TestDisableEventFlushEnvMatchesTheTestProcess`; `TestGlobalConfigDisablesNetworkChecks`; `TestUnroutableHTTPSProxyEnvBlackholesEveryRequest`; `TestWriteGlobalConfigNamesThePathItCouldNotWrite`; `TestWriteGlobalConfigSeedsDoltRoot`.

`failure_attribution: TestGraphWorkflowFailureRunsCleanup -> ga-vkhfnj | clause 1: graph_dispatch_test.go:107 UNCHANGED, failing body untouched; clause 2: open predating tracker and verified current comment; clause 3: b cross-PR, exact earlier signature on unrelated ga-vzckwr and ga-fiu7se diffs; clause 4: same test package's helper changed, proof=b, added_test_load=no (no census bump, new target, or new test file in that package); repeat exception: fix-carrying ga-z4jzmd, unlanded.`

`policy_lane: make test-ci-policy — PASS`; `drift_lane: make bazel-sync && git diff --exit-code — PASS`; `ci_lane_run: n/a (no CI-config change)`; `heavy_mode: none`.

`load_threshold: 15`; `load_waited_seconds: 1800`; `load_wait_timed_out: 1`; `run_start_load: 23.32`; `run_max_load: 47.92`; `run_mean_load: 34.16`; `run_readings: 105`. The ordinary gate proceeded after its bounded wait, as the release-gate procedure specifies.
