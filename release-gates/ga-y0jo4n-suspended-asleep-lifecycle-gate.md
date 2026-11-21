# Release gate: suspended-to-asleep lifecycle coherence (`ga-y0jo4n`)

- Deploy bead: `ga-y0jo4n`
- Reviewed commit: `659546b24270f20a70d1f87a129429fa838acd2f`
- Base evaluated: `origin/main@06d073fe62c300df4b4f415f706b289075a8e664`
- Evaluation date: 2026-09-13
- Verdict: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-f1rv7d` records `VERDICT: PASS` for the exact reviewed commit and no requested changes. The recorded SHA resolves to a commit in this repository. |
| 2 | Acceptance criteria met | **PASS** | The reviewed diff keeps one coherent lifecycle vocabulary across the affected transitions: suspended/drained wake clears `suspended_at` and stamps `slept_at`; a fresh session reopen clears stale sleep metadata; `SleepPatch` clears `suspended_at`; `Manager.Suspend` clears stale sleep metadata; and legacy asleep records can fall back to `suspended_at` for pruning. The corresponding regression tests all report PASS in the full-suite logs. |
| 3 | Tests pass | **PASS** | The documented full-scope command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel` completed **39/40 jobs green**, with **50,269 PASS / 1 FAIL / 210 SKIP** top-level test executions. The sole failure was `TestCleanInstallTutorialPath` in `integration-rest-full-2-of-8`: `gc rig add` resolved the stale host `bd` 1.1.0 from `PATH`, initialized below the linked module's schema ceiling, and the pinned rc.2 code refused 27 shared-server migrations (`v39 -> v66`). Under the mayor's standing authorization on tracker `ga-lejnse`, the exact failing test was rerun on reviewed candidate `659546b24270` with a freshly built pinned `bd` v1.3.0-rc.2 first on `PATH`; it **PASSed** in 28.06s. The required comparison on current base `06d073fe62c3` also **PASSed** in 25.18s. This satisfies the authorized disposition for the stale-host-binary condition; all diff-owned tests also passed with zero diff-owned FAIL or SKIP. `test_cmd_scope: full-suite`; `waiver_ref: none`; `authorization_ref: ga-lejnse MAYOR STANDING AUTHORIZATION 2026-09-13 13:35Z and mail gm-wisp-3d8d0q`; `ci_lane_run: n/a (no CI-config change)`; full log: `/var/tmp/ga-y0jo4n-full.log`; shard logs: `/var/tmp/gc-local-tests.LuHQj2`; pinned comparison logs: `/var/tmp/ga-y0jo4n-pinned-candidate.log`, `/var/tmp/ga-y0jo4n-pinned-base.log`. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed every required Python and Go policy check. `go vet ./...`, `go build ./...`, and `make lint-new` also passed; lint reported 0 issues. Logs: `/var/tmp/ga-y0jo4n-policy.log`, `/var/tmp/ga-y0jo4n-vet.log`, `/var/tmp/ga-y0jo4n-build.log`, and `/var/tmp/ga-y0jo4n-lint-new.log`. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-f1rv7d` records PASS with no changes requested and no unresolved high-severity finding. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty at the reviewed commit before this gate record was created. |
| 6 | Branch diverges cleanly from main | **PASS** | The already-merged pre-flight found no pull request carrying reviewed SHA `659546b24270`. After fetching current main, `git merge-tree --write-tree --messages origin/main 659546b24270f20a70d1f87a129429fa838acd2f` exited 0 and produced tree `ca3c239f520170c9572e6a0e8363d6a209843e88`. `assert_deploy_ancestry_scope` passed for `ga-y0jo4n`, `ga-we6bj3`, `ga-7owgg2`, and `ga-f1rv7d`. |
| 7 | Single feature theme | **PASS** | The three commits and all production changes address one session lifecycle-coherence theme: preserving consistent suspended/asleep timestamps and vocabulary across wake, sleep, reopen, suspend, and prune paths. The carried historical gate record documents the same feature chain. |

## Criterion 3 details

`diff_tests_executed`:

- `TestSuspendThenWakeIsSingleVoiced` — PASS
- `TestAsleepWithoutSleptAtIsUnprunable` — PASS
- `TestSleepPatchClearsSuspendedAt` — PASS
- `TestSuspendClearsSleepVocabulary` — PASS
- `TestClearWakeBlockersPatchClearsOnlyWakeBlockerMetadata` — PASS
- `TestLifecycleTransitionPatchesSetCompleteMetadata` — PASS
- `TestEmitSessionWakeRefused_FreshWakeClearsGuardAndReemits` — PASS
- `TestEmitSessionWakeRefused_HeldSessionNotQuarantinedAtThreshold` — PASS
- `TestEmitSessionWakeRefused_SingleRefusalEmitsAndBumpsOnce` — PASS
- `TestEmitSessionWakeRefused_TenConsecutiveTicksGuardHolds` — PASS

The 210 skips are pre-existing suite-controlled opt-ins and platform/provider exclusions. None is in a test file added or modified by this diff.

`failure_attribution_candidate: TestCleanInstallTutorialPath -> ga-lejnse | clause 1: not diff-owned; clause 2: exact condition tracker predates this run and was opened, with this sighting appended; clause 3: mechanism proof — failure occurs in external bd schema initialization during gc rig add, before the changed session wake/suspend/prune paths can run; clause 4: test/integration has no path overlap with the diff.`

`repeat_condition_disposition: satisfied — ga-lejnse records the same temporary-city schema-ceiling refusal across unrelated reviewed candidates and now carries a mayor standing authorization for this exact condition. The pinned-bd candidate/base comparison passed, confirming the stale host binary as the failed run's cause. The permanent host-binary fix remains tracked by ga-5qvsyq.`

All seven release criteria pass. Prepare the isolated deploy branch at the exact reviewed commit, commit this checklist, push it, open the pull request, and publish deploy clearance before routing the merge-request. The deployer does not merge.
