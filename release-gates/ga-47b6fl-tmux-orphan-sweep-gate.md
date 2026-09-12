# Release gate: reap orphaned tmux test servers

- Deploy bead: `ga-47b6fl`
- Review bead: `ga-tojs77`
- Reviewed commit: `1dacc4f3d6ac45bc76fdeafbce4669de6815a098`
- Current base: `origin/main@41edfbef9153a9922c6635038e79c020566f216b`
- Deploy mode: `remote`; push remote: `origin`
- Isolated branch: `deploy/ga-47b6fl-gate`
- Evaluated: 2026-09-11
- Gate state: **PASS with attributed raw test failures**

`docs/PROJECT_MANIFEST.md` is absent at both the reviewed commit and current
base, so this checklist applies the release criteria from the deployer protocol
and `engdocs/contributors/release-gate-criteria-conventions.md`. The remote
pre-flight found no pull request carrying the reviewed commit, so there is no
already-merged or closed-PR disposition to reconcile.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-tojs77` records `verdict: pass` for the exact reviewed commit and no review carryover or SHA substitution. The recorded SHA resolves to a commit in this repository. |
| 2 | Acceptance criteria met | **PASS** | The orphan sweep now finds real tmux sockets below stale PID-prefixed test directories, targets each server through its explicit socket, waits for exit, and sends `SIGKILL` only to the exact captured PID when the bounded graceful shutdown does not finish. The live-server regression test and the synchronized resource-census checks pass. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union scheduled and completed all 40 jobs: 36 job PASS / 4 jobs with raw failures / 0 omitted. Preserved logs contain 49,988 PASS / 5 raw FAIL / 210 SKIP results. Both diff-owned tests ran in two independent lanes and passed every time, with 0 diff-owned FAIL and 0 diff-owned SKIP. All five raw failures satisfy criterion 3a below. |
| 3a | Non-diff-owned failures attributed | **PASS** | Four failures stopped during initialization on the exact shared-Dolt pending-migration condition tracked by predating tracker `ga-vkhfnj`; `TestE2E_SuspendResume_City` matches the independently proven suspend/reconciler defect tracked by predating tracker `ga-dc9utn`. Every sighting was appended to and read back from its tracker. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected`, `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exit 0. The lint lane reports `0 issues`, and `core.hooksPath` is `.githooks`. |
| 3c | CI-config lane run | **PASS / n/a** | The diff changes no CI workflow, job, matrix, timeout, required-check list, Makefile target, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The exact-head reviewer records no style, specification, security, blocker, or high-severity finding. |
| 5 | Final branch clean | **PASS** | The detached worktree remained clean at the exact reviewed SHA after the full suite and additive checks. This checklist is the deployer's only new file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree origin/main 1dacc4f3d6ac45bc76fdeafbce4669de6815a098` exits 0 and produces tree `0a32a1cf1e9b0f3b8252b7a0dbebb6f5bc35bb96`. Main has 3 exclusive commits and the candidate has 2; no bounded self-rebase is needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits change one test-infrastructure behavior: reap tmux servers before deleting their stale socket directories, with the exact resource-census and documentation ledger adjustments that behavior requires. `assert_deploy_ancestry_scope` passes for `ga-47b6fl`, `ga-tojs77`, `ga-05ci52`, and `ga-c3k0xe`. |

## Acceptance evidence

- `killTmuxServersUnder` walks only the already-eligible stale directory and
  identifies Unix-domain sockets before the existing directory removal.
- Both tmux commands use argv-array execution and an explicit `-S <socket>`
  target. No default tmux server is addressed.
- Graceful shutdown polls for at most two seconds; fallback termination targets
  only the PID read from the same explicit socket.
- `TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval` starts a real
  tmux server, runs the sweep, and confirms its exact process is gone: PASS in
  both `unit-core` and `integration-packages-core-3-of-4`.
- `TestBootstrapPolicyOwnsTmuxDebtAndExactMediumSetup` confirms the changed
  subprocess census: PASS in both `unit-core` and
  `integration-packages-core-1-of-4`.
- `TestRepositoryLedgerMatchesCensusAndDocumentation` independently scans the
  repository and verifies that `census.go`, `test/test-resources.toml`, and
  `TESTING.md` agree: PASS in both `unit-core` and
  `integration-packages-core-1-of-4`.
- `go build ./...`: PASS. `go vet ./...`: PASS. Changed-file formatting and
  `git diff --check`: PASS.

## Criterion 3 evidence

The container-backed environment was prepared before the full run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman 5.8.4 was reachable through that socket.
- `dolthub/dolt-sql-server:1.32.4` resolved to digest
  `sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d`.
- `dolthub/dolt:2.1.7` resolved to digest
  `sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`.

Test record:

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 36 jobs PASS / 4 jobs with raw failures / 0 omitted; 49,988 PASS / 5 raw FAIL / 210 SKIP
- `diff_tests_executed`: `TestSweepOrphanPIDPrefixedDirsKillsLiveTmuxServerBeforeRemoval=PASS` and `TestBootstrapPolicyOwnsTmuxDebtAndExactMediumSetup=PASS`, each in two independent full-suite lanes; 0 diff-owned FAIL and 0 diff-owned SKIP
- `skip_justification`: all 210 skips come from unchanged suite code and are explicit platform, optional-provider, live-infrastructure, integration-opt-in, shard-selection, or subprocess-helper guards. Neither diff-owned test skipped, and the required cmd/gc process lane ran.
- `waiver_ref`: none
- `ci_lane_run`: n/a (no CI-config change)
- `runner_log`: `/var/tmp/gc-deploy-ga-47b6fl-full.log`
- `shard_logs`: `/var/tmp/gc-local-tests.8l5DUp`
- `runner_log_sha256`: `5273a94435edfcd6c4f984132bc440871a63507d024ee04f8e67211b8c6adaf5`

### Raw failure attribution

- `failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix, TestPersonalWorkFormulaCompileAndRun, TestGraphWorkflowSuccessPath, TestCleanInstallTutorialPath -> ga-vkhfnj | clause 3(a) MECHANISM — attributed`
  - Clause 1: none of the four failing test files is added or modified by this
    diff.
  - Clause 2: open `gate-tracker` bead `ga-vkhfnj` predates this run and already
    records this exact pending-migration signature on clean `origin/main`. All
    four new sightings were appended and verified.
  - Clause 3: each failure stops during `bd init` or `gc init`, before its test
    scenario, because the external bd client refuses to apply 4–25 pending
    migrations to a shared Dolt server. Tmux socket cleanup cannot mutate Dolt
    schema versions or bd migration policy.
  - Clause 4: the failing files are `cmd/gc/cmd_bd_test.go` and three files
    under `test/integration`; the candidate paths are under `test/tmuxtest` and
    `internal/testpolicy/resourcecensus`, with no path overlap.
  - Raw logs: `cmd-gc-process-4-of-6.log`
    (`sha256:aa93c81a64cec4fde7c919578c7328730b29c2e51e34f2c99b028828c2c2e33e`),
    `integration-review-formulas-basic-2-of-2.log`
    (`sha256:e2f8653e40c1de6f2af0fe3579b61cac65a15463c72ecc33e613dc410215cadd`),
    `integration-rest-smoke-2-of-2.log`
    (`sha256:fc466057d52299b745b0499f9de39a1cbea149e93c474d31de1e908591032a81`),
    and `integration-rest-full-2-of-8.log`
    (`sha256:58b511b7761e7675d58f62975b31909d2afab53d08042a37099f3ed3a3e78e91`).

- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a) MECHANISM — attributed`
  - Clause 1: the candidate changes neither
    `test/integration/e2e_lifecycle_test.go` nor the production
    suspend/reconciler path.
  - Clause 2: open `gate-tracker` bead `ga-dc9utn` predates this run and records
    the independently proven root condition. This sighting was appended and
    verified.
  - Clause 3: the run reproduced the tracker's canonical 93.83-second missing
    `citysus.report` signature. The tracker proves that suspension empties the
    desired state and the reconciler retires the named session before resume.
    The candidate does not change that path, and its stale-directory sweep
    excludes the live parent PID of this running test server.
  - Clause 4: the failing integration-test path and the proven reconciler path
    do not overlap any candidate path.
  - Raw log: `integration-rest-full-2-of-8.log`
    (`sha256:58b511b7761e7675d58f62975b31909d2afab53d08042a37099f3ed3a3e78e91`).

All five attributions use affirmative mechanism evidence; none relies on an
inconclusive non-reproduction. The first candidate run is preserved and was not
retried into green.

## Policy and static evidence

- `make test-ci-policy`: PASS.
- `go vet ./...`: PASS.
- `go build ./...`: PASS.
- `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected`:
  PASS, `0 issues`.
- `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed`:
  PASS.
- `git diff --check origin/main...HEAD`: PASS.
- `core.hooksPath`: `.githooks`.
- Policy log: `/var/tmp/ga-47b6fl-policy.log`
  (`sha256:7dd7f214f5afaad877b12c2fe2a9323ea2b1411d24766f89ad8c5946e595512b`).
- Lint log: `/var/tmp/ga-47b6fl-lint.log`
  (`sha256:8d207048a3fa0baff80d182a4e3d71ec65f304190101276588ac2bd19ea1017f`).

## Disposition

Gate PASS. Cut `deploy/ga-47b6fl-gate` from the exact reviewed source, commit
this checklist there, push the isolated branch, and open a pull request. Merge
authority remains with mayor/mpr; the deployer does not merge.

## Addendum 2026-09-12: the merged head is not the gated head

Everything above is the record of a gate evaluated on 2026-09-11 against
commit `1dacc4f3` over base `41edfbef`. Both SHAs are accurate and are left
unchanged: they name what was actually run, and rewriting them to the merged
head would assert that the 40-job local CI union, its preserved logs, and its
failure attributions were collected against content they never saw.

The gate's PASS does not extend to what landed. PR #6296 squash-merged as
`a1deacc6` on 2026-09-12, and its `test/tmuxtest/orphan_sweep.go`
(blob `cd3ff9c9`) matches neither the gated tree nor the remediation authored
for the review's request_changes findings — `git log --all --find-object`
finds that blob in exactly one commit, the merge itself. The remediation
commit was authored 89 seconds after the merge and never reached the branch,
so #6296 merged while its remediation was still being written; this was not a
merge-queue race against an already-pushed approved head.

The post-merge review of the landed range re-raised those findings. The
follow-up PR that carries the remediation onto main is the current record of
this change's reviewed state; this checklist is retained as the historical
gate artifact for `1dacc4f3` only.
