# Release gate: mol-dog-stale-db close actor guard

- Deploy bead: `ga-qhr85l`
- Review bead: `ga-je7i97` — PASS
- Build bead: `ga-b15hwm`
- Reviewed source: `c895cb82b1f7b5f305fac31342a3195987fbd3cb`
- Base: `origin/main@e2902e0417ee2125caafffa3d7ef9fb9e81691d1`
- Deploy mode: `remote`; push remote: `fork`
- Pre-flight: the reviewed source has no associated pull request, so it has not already merged
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-je7i97` records `verdict: pass` and pins the exact reviewed source `c895cb82b1f7b5f305fac31342a3195987fbd3cb`. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The final `bd close` in `mol-dog-stale-db` now supplies the session identity explicitly through `--actor "${GC_ALIAS:-${GC_SESSION_NAME:-${GC_SESSION_ID:-}}}"`, without weakening the ownership guard with `--force`. The adjacent append-note behavior and fail-open abort paths are unchanged. The new fleet-wide, per-line pack-lint test requires `--actor`, `--force`, or an acknowledged exception on raw guarded `bd` mutations. RED/GREEN, live actor-mismatch, shell-rendering, and clean-close evidence are recorded below. |
| 3 | Tests pass | **PASS with six attributed raw failures** | The documented isolated full local CI union completed all 40 jobs: **34 green / 6 attributed raw failures**, with **47,117 PASS / 6 FAIL / 225 SKIP** top-level test executions. Four failures are the pre-existing concurrent-initializer/shared-Dolt schema condition tracked by `ga-e2z1zb`; the other two are tracked retry/cleanup conditions covered by `ga-j88sfp` and `ga-xjh1p4` and explicitly attributed for this deploy by mayor ruling `gm-wisp-mdl4bl`. The diff-owned `TestRawBdMutationRequiresActorOrForce` ran and passed in both selecting tiers: **2 PASS / 0 FAIL / 0 SKIP**. `test_cmd_scope: full-suite`; `waiver_ref: none`; `authorization_ref: ga-lejnse standing authorization plus gm-wisp-mdl4bl`; `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The review records no unresolved HIGH finding, security blocker, or specification blocker. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain=v1` was empty at the reviewed source. `git diff --check origin/main...c895cb82b1f7b5f305fac31342a3195987fbd3cb` and `gofmt -l test/packlint/bd_raw_mutation_actor_test.go` were also empty. This checklist is committed on the isolated deploy branch in the next step. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main`, `git merge-tree --write-tree origin/main c895cb82b1f7b5f305fac31342a3195987fbd3cb` exited 0 and produced tree `ff09ffff113aa6fde7883795554b4e291e470355`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two commits form one formula-safety theme: supply the claimed session identity to the stale-DB work-bead close and prevent recurrence of the same raw-`bd` actor omission. `assert_deploy_ancestry_scope origin/main HEAD ga-qhr85l ga-b15hwm` exited 0; no unrelated commit or `.claude/**` path is present. |

## Acceptance checks

- The final close carries the exact nounset-safe identity chain `GC_ALIAS -> GC_SESSION_NAME -> GC_SESSION_ID -> empty` and does not use `--force`.
- The `bd update --append-notes` call is unchanged, and `fail_open_after_drain` still leaves the work bead open on a hard-abort path.
- `TestRawBdMutationRequiresActorOrForce` scans the existing `examples/**` and `internal/bootstrap/packs/**` runnable-content roots, matches guarded raw `bd close`, `unclaim`, `heartbeat`, and `update --status closed` forms per line, and recognizes only `--actor`, `--force`, or `# guard-ack:<slug>` as exemptions.
- Builder and reviewer evidence records the guard test failing at RED commit `b2d2aea4a83aca69631e7585e3980872a52f93a8` on the unguarded close and passing at the reviewed source after the actor fix.
- Builder and reviewer notes record live negative and positive controls against a real claimed bead: the ambient mismatched actor was rejected, while the fixed formula line closed the same bead without `--force` or a guard error.
- In the deployer's full-suite run, `TestRawBdMutationRequiresActorOrForce` passed twice. `TestStaleDBFormulaRuntimeContract`, `TestStaleDBFormulaRenderedShellIsStrictAndValid`, and `TestStaleDBFormulaCleanApplyClosesWorkAndUsesDBThreshold` also each passed twice. A supplemental isolated focused run of those three formula tests passed 3/3.
- Candidate paths are exactly:
  - `examples/bd/dolt/formulas/mol-dog-stale-db.toml`
  - `test/packlint/bd_raw_mutation_actor_test.go`

## Criterion 3 evidence

- `test_cmd`: rootless Podman environment (`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, mirrored through `EXTRA_TEST_ENV`) with `GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/gc-gate-ga-qhr85l-verbose.dTn5cs/jobs make test-local-full-parallel`, invoked through `packs/actual/all/scripts/isolated-test-run.sh`.
- `test_cmd_scope: full-suite`
- `test_counts: 47,117 PASS / 6 raw FAIL / 225 SKIP`; job result `34 PASS / 6 raw FAIL / 0 omitted`.
- `required_job_coverage`: all 40 jobs started and completed, including `unit-core`, all six process shards, product metrics, package and `cmd/gc` integration shards, all three tmux shards, review-formula shards, bd-store, and all ten REST shards.
- `diff_tests_executed: TestRawBdMutationRequiresActorOrForce — 2 PASS / 0 FAIL / 0 SKIP` in `unit-core` and `integration-packages-core-2-of-4`.
- `skip_justification`: the 225 skips are existing suite-controlled platform, helper-process, real-provider, opt-in persistence, and tier-selection exclusions. The full union includes the process and integration lanes referenced by the unit-tier skip messages. None is diff-owned. The load-sensitive `TestHumaBinary_CityUnregisterAsync` skip names its existing upstream tracker `#2090` in the test output.
- `waiver_ref: none` for diff-owned tests.
- `authorization_ref`: mayor standing authorization on `ga-lejnse` for the random-cursor condition; mayor ruling `gm-wisp-mdl4bl` for the two tracked repeat conditions in this deploy. The earlier `gm-wisp-gz0rld` ruling covered a tracked exec-cancellation failure seen in the preceding non-verbose full run; that test passed in the countable run above and is not one of its six failures.
- `ci_lane_run: n/a (no CI workflow, matrix, timeout, or required-check configuration changed)`.
- `policy_lane: make test-ci-policy — PASS`.
- `go build ./...` — PASS.
- `go vet ./...` — PASS.
- `make check-hooks` — PASS (`core.hooksPath` is `.githooks`).
- `gofmt -l test/packlint/bd_raw_mutation_actor_test.go` — empty/PASS.
- `git diff --check origin/main...c895cb82b1f7b5f305fac31342a3195987fbd3cb` — empty/PASS.
- Rootless Podman 5.8.4 was live before the run. The module checksum file carries indirect testcontainers entries, but no Go source imports testcontainers and no test code pins a testcontainers image tag, so there was no cached tag to refresh for this suite.
- Full log: `/var/tmp/gc-gate-ga-qhr85l-verbose.dTn5cs/full.out`.
- Per-job logs: `/var/tmp/gc-gate-ga-qhr85l-verbose.dTn5cs/jobs`.
- Supplemental formula log: `/var/tmp/gc-gate-ga-qhr85l.mNf8hM/formula-focused.out`.
- Policy, vet, and build logs: `/var/tmp/gc-gate-ga-qhr85l.mNf8hM/{policy,vet,build}.out`.

### Criterion 3a failure attribution

All six raw failures satisfy clauses 1, 2, and 4: none is diff-owned, every condition has a tracker that predates this run, and none overlaps either candidate path. The four schema refusals also have a landed mechanism proof. For the other two, the mayor independently checked the inconclusive-path guards and authorized attribution for this deploy in `gm-wisp-mdl4bl`.

| Failure | Attribution |
|---|---|
| `TestAdoptPRFormulaCompileAndRun` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture stopped in `gc init` on the known concurrent-initializer random-cursor refusal (`v40 -> v66`) before formula adoption or candidate behavior. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture stopped in `gc init` on the same condition (`v52 -> v66`) before the retry formula ran. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. The fixture stopped in `gc init` on the same condition (`v53 -> v66`) before the retry formula ran. |
| `TestCleanInstallTutorialPath` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Rig initialization stopped on the same condition (`v63 -> v66`) before tutorial behavior could run. The sightings were appended to `ga-e2z1zb` and read back. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `failure_attribution: ... -> ga-j88sfp | clause 3: INCONCLUSIVE; mayor-authorized`. The recovery flow produced only attempt 1. `inconclusive-guard: reachable_production_code=no added_test_load=no`: no integration test imports or invokes the changed stale-DB formula, `test/packlint` is an existing target, and its unit job completed before this shard. Mayor ruling: `gm-wisp-mdl4bl`. |
| `TestGraphWorkflowSuccessPath` | `failure_attribution: ... -> ga-xjh1p4 | clause 3: INCONCLUSIVE; mayor-authorized`. The convoy retained `work_dir` after cleanup. `inconclusive-guard: reachable_production_code=no added_test_load=no`: this test uses `graph-workflow.toml`, not the changed stale-DB formula; the existing pack-lint target had already completed. The mayor noted this is the same tracked cleanup-worktree phase but a different assertion from the first sighting. Mayor ruling: `gm-wisp-mdl4bl`. |

The six raw failures therefore do not weaken the direct evidence for the actor guard: its diff-owned test and the relevant stale-DB formula tests executed and passed in both full-suite selecting tiers.
