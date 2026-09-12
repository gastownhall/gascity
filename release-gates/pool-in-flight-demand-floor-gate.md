# Release gate: retain in-flight pending-create pool demand (`ga-jj113k`)

- Overall verdict: **PASS with attributed raw test failures**
- Evaluated: 2026-09-11 UTC
- Deploy mode: `remote`; push remote: `origin`
- Reviewed deploy source: `2228831215f5c00e6b9ab10a695608307656c1cd`
- Source branch: `builder/ga-nf5xlp.1` (provenance only)
- Base evaluated: `origin/main@411413b1055d660a1d8cb7beac98d5f5f4cd7216`
- Existing source PR: none

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit or on
`origin/main`, so this checklist uses the release criteria embedded in
`mol-deployer-gate`, the Deployer instructions, and
`engdocs/contributors/release-gate-criteria-conventions.md`. The pre-flight
found no PR carrying the reviewed commit, so there was no already-merged or
closed-PR disposition to reconcile.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-f3ltde` records `verdict: pass` against exact reviewed commit `2228831215f5c00e6b9ab10a695608307656c1cd`, with no review carryover or SHA substitution. The SHA resolves to that commit in this repository. |
| 2 | Acceptance criteria met | **PASS** | Fresh pending-create sessions now contribute a demand floor only while their existing lease is live. The helper delegates lease arithmetic to `pendingCreateLeaseExpiredForRollbackInfo`; an expired attempt still falls to zero. `capNewDemandCount` remains after the floor calculation, protected sessions still consume capped demand before in-flight sessions, both new regression tests pass, and unrelated HQ work-query issue `gm-u7ehpl` is untouched. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union scheduled all 40 jobs and completed **37 jobs PASS / 3 jobs with raw failures / 0 omitted**. Its top-level result lines contain **49,914 PASS / 4 raw FAIL / 210 SKIP**. Both diff-owned tests ran twice and passed every time, with 0 diff-owned FAIL and 0 diff-owned SKIP. All four raw failures are non-diff-owned and satisfy criterion 3a below. |
| 3a | Non-diff-owned failures attributed | **PASS** | Three failures are exact instances of the predating shared-schema tracker `ga-esyijp`; `TestE2E_SuspendResume_City` is the exact proven condition tracked by predating tracker `ga-dc9utn`. Each occurrence was appended to and read back from its tracker. All four attributions satisfy the required not-diff-owned, predating-tracker, mechanism-proof, and no-path-overlap clauses. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `LINT_CHANGED_REF=origin/main make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exited 0. `core.hooksPath` is `.githooks`. |
| 3c | CI-config lane run | **PASS / n/a** | The diff changes no workflow, CI job, matrix, timeout, required-check list, Makefile, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The exact-head reviewer verdict records no style, security, specification, or high-severity finding. |
| 5 | Final branch clean | **PASS** | The worktree remained clean at the exact reviewed SHA after the full suite and additive checks. This checklist is the deployer's only new file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree origin/main 2228831215f5c00e6b9ab10a695608307656c1cd` exited 0 and produced tree `dfa022c5e72311fdc7d68ecab3dbdc32c1cf031a`. The base has 5 exclusive commits and the candidate has 2; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two candidate commits change only `cmd/gc/pool_desired_state.go` and its test for one pool-demand calculation. `assert_deploy_ancestry_scope` passed for `ga-jj113k`, `ga-f3ltde`, and `ga-nf5xlp.1`; no unrelated ancestry or `.claude/**` path is present. |

## Build and acceptance evidence

- `go build ./...`: **PASS**.
- Fresh affected-package run, `go test ./cmd/gc/... -count=1 -v`: **PASS**, 9,956 top-level PASS / 0 FAIL / 100 SKIP.
- `TestComputePoolDesiredStates_InFlightPendingCreateEstablishesDemandFloor`: **PASS** in both the process and integration-package full-suite lanes, and in the fresh affected-package run.
- `TestComputePoolDesiredStates_InFlightPendingCreateDemandFloorExpiresWithLease`: **PASS** in both the process and integration-package full-suite lanes, and in the fresh affected-package run.
- `TestTutorial01`: **PASS** in the required `cmd/gc` process lane with `GC_FAST_UNIT=0`; its skip in the separate integration-package lane is therefore not a coverage gap.
- Source inspection confirms the new floor is calculated before `capNewDemandCount`, and the unchanged `protectedCount` calculation still precedes `inFlightCount`.
- Source inspection confirms the floor helper requires a pending-create claim, a rollback-eligible state, and a nonzero decision time before delegating to the reconciler's existing lease-expiry predicate.
- `git diff --check origin/main...HEAD`: **PASS**.
- The diff does not touch or claim to resolve `gm-u7ehpl`.

## Criterion 3 evidence

The container-backed environment was established before the full-suite run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman was reachable through the socket.
- `docker.io/dolthub/dolt-sql-server:1.32.4` was refreshed and resolved to `sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d`.
- Repository-pinned `docker.io/dolthub/dolt:2.1.7` was present at `sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`.

Test record:

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **37 jobs PASS / 3 jobs with raw failures / 0 omitted**; **49,914 top-level PASS / 4 raw FAIL / 210 SKIP**
- `diff_tests_executed`: `TestComputePoolDesiredStates_InFlightPendingCreateEstablishesDemandFloor=PASS` and `TestComputePoolDesiredStates_InFlightPendingCreateDemandFloorExpiresWithLease=PASS`, each in two independent full-suite lanes; 0 diff-owned FAIL and 0 diff-owned SKIP
- `skip_justification`: all 210 skips are from unchanged suite code and cover platform, permission, optional-provider/live-infrastructure, helper-process, opt-in integration, or intentional shard-selection guards. The required `TestTutorial01` process lane ran and passed. Neither diff-owned test skipped.
- `waiver_ref`: none
- `ci_lane_run`: n/a (no CI-config change)
- `shard_logs`: `/var/tmp/gc-local-tests.IojiMV`
- `runner_log`: `/var/tmp/ga-jj113k-full-suite-20260911.log`
- `runner_log_sha256`: `806d192a534da47d844f8e6c52e82dcb100420c458c74748f82569dd88305598`

### Raw failure attribution

- `failure_attribution: TestCleanInstallTutorialPath, TestGCLiveContract_BeadsAndEvents, TestGraphWorkflowFailureRunsCleanup -> ga-esyijp | mechanism proof — attributed`
  - Clause 1: the candidate changes neither these integration tests nor Beads schema/init handling; it changes only pool desired-state arithmetic and its test.
  - Clause 2: open `gate-tracker` bead `ga-esyijp` was created on 2026-08-29, before this run. All three sightings were appended and verified.
  - Clause 3: each test failed before its intended assertion because the external `bd init` refused to migrate a shared server: 35 pending migrations (`v31 -> v66`), 64 (`v2 -> v66`), and 4 (`v62 -> v66`), respectively. Pool-demand calculation cannot alter the separately installed `bd` binary or shared database schema.
  - Clause 4: the failing test paths are under `test/integration`; the candidate paths are `cmd/gc/pool_desired_state.go` and `cmd/gc/pool_desired_state_test.go`, with no path overlap.
  - Raw logs: `integration-rest-full-2-of-8.log` (`sha256:7184c50b816af1fa66dbf9353bb96c26f51e4ac70301631e2ae2965105c5a27d`), `integration-rest-full-5-of-8.log` (`sha256:5ca7bcebb6fc627a14ba0f206750da463589a92dc4e741dbe8872b499db2952a`), and `integration-rest-full-7-of-8.log` (`sha256:2ffb332f086c941a84c859eb815ee137858e57e9d6031cdf10a20e469d07cc41`).

- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn (fix ga-pmafyc) | proven mechanism — attributed`
  - Clause 1: the candidate changes neither `test/integration/e2e_lifecycle_test.go` nor the reconciler's suspend/resume retirement path.
  - Clause 2: open `gate-tracker` bead `ga-dc9utn` was created on 2026-09-09, before this run. The sighting was appended and verified.
  - Clause 3: the run reproduced the tracker's canonical 94.55-second missing `citysus.report` signature. The tracker proves that suspension produces an empty desired state and the reconciler retires and clears the named session before resume. Pending-create pool-demand arithmetic cannot affect that configured named-session retirement mechanism.
  - Clause 4: the failing integration-test path and proven `buildDesiredStateWithSessionBeads`/session-reconciler path do not overlap either candidate path.
  - Raw log: `integration-rest-full-2-of-8.log` (`sha256:7184c50b816af1fa66dbf9353bb96c26f51e4ac70301631e2ae2965105c5a27d`).

All four attributions use landed mechanism evidence, so the inconclusive
reachability/load guard is not invoked. The first candidate run is preserved;
it was not retried into green.

## Policy and static evidence

- `make test-ci-policy`: **PASS** (runner-policy, suite-coverage, `scripts/cipolicy`, `scripts/prwatchdog`, and static-scope contract tests).
- `go vet ./...`: **PASS**.
- `LINT_CHANGED_REF=origin/main make fmt-check-changed`: **PASS**.
- `git diff --check origin/main...HEAD`: **PASS**.
- `core.hooksPath`: `.githooks`.
- Policy log: `/var/tmp/ga-jj113k-policy.log` (`sha256:7061eaa3e6662bba7ca8951b491bc4caf6cce2eeabd9fcf46fb0dd96b1fb20db`).
- Affected-package log: `/var/tmp/ga-jj113k-affected.log` (`sha256:234f0ead54e5ffa4295d7f7c79ac5e074c292cb3a7dd697097b5e79dc2a64928`).

## Disposition

Gate PASS. Cut `deploy/ga-jj113k-gate` from the exact reviewed source, commit
this checklist there, push the isolated branch, and open a pull request. Merge
authority remains with mayor/mpr; the deployer does not merge.
