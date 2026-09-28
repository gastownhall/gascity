# Release gate: respect blocked work outcomes in dependency readiness

- Deploy bead: `ga-0rqcer`
- Build bead: `ga-a7v0ex`
- Review bead: `ga-156msp`
- Reviewed source: `d6e6cd54a232e463abbd331b633fa3fa852b47f4`
- Reviewed source branch: `builder/ga-a7v0ex` (provenance only)
- Base checked: `origin/main@57a02b297906b506edd9b7db09c360a70f44e4b7`
- Planned deploy branch: `deploy/ga-0rqcer-gate`
- Evaluated: 2026-09-06
- Verdict: **PASS with attributed raw test and lint failures**

## Gate checklist

The target pre-flight ran before criterion 6. The recorded source resolved to
the full commit above and is not associated with an existing pull request.
The source and current base merge without conflict, so all seven deployer
criteria were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-156msp` records `verdict: pass` for the exact reviewed source. The reviewer ran the complete `internal/beads` package and recorded 1,312 PASS, 0 FAIL, and 6 suite-controlled SKIP events. |
| 2 | Acceptance criteria met | **PASS** | The canonical `DependencySatisfied` predicate now requires a dependency to be closed and rejects the closed-plus-`gc.work_outcome=blocked` state. Closed dependencies with no outcome retain legacy satisfaction, and closed dependencies with non-blocked or unknown outcomes remain satisfied. MemStore, CachingStore, SQLiteStore, NativeDoltStore, ExecStore, FileStore, and BdStore now apply that contract at their appropriate query or post-filter boundary. The new truth-table test and both new readiness-conformance scenarios passed for every conformance implementation. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local runner completed all 40 jobs: **34 PASS / 6 raw FAIL jobs**. It reported 48,020 top-level PASS, 6 top-level raw FAIL, and 210 top-level SKIP events. All six failures are non-diff-owned and attributed under criterion 3a. Every diff-owned test and conformance scenario passed; the native BdStore integration job also passed. `test_cmd_scope: full-suite`; no waiver is used. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` is the installed-CLI drift tracked by `ga-f0uceo`. `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` and `TestIsAgentRunning` match exact cross-candidate fleet-contention signatures tracked by `ga-vkhfnj`. The remaining three tests stopped during fixture initialization on the beads#4566 dirty-schema condition tracked by `ga-esyijp`, before their scenario assertions. Every tracker predates this run; current sightings were appended and read back. No failing test file is changed by this candidate. |
| 3b | Policy and static lanes | **PASS with attributed raw lint findings** | `make test-ci-policy`, repository boundary checks, native DoltLite tests, changed-file formatting, docs synchronization, build, vet, and `git diff --check` passed. `make lint-affected` reported eight findings in seven unchanged files; an exact disposable `origin/main@57a02b297906b506edd9b7db09c360a70f44e4b7` worktree reproduced all eight. The four gofumpt findings are tracked by `ga-d3m213`, the three SA1019 findings by `ga-t88402`, and the SA4006 finding by `ga-egkp4x`. All seven files are blob-identical between candidate and base and absent from this diff. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, or required-check change)`. |
| 4 | No unresolved HIGH review findings | **PASS** | Unresolved HIGH findings: 0. The review-of-record found no blocking correctness, security, specification, or coverage issue. Its one non-blocking SQL-construction observation concerns compile-time constants only and exposes no untrusted input. |
| 5 | Final branch clean | **PASS** | Before this checklist was added, `git status --short` produced no output and `git diff --check` passed. The checklist is the only deployer-authored file and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main d6e6cd54a232e463abbd331b633fa3fa852b47f4` exited 0, produced tree `cfaa16397ea0b55e5b920fc57f5a0564250aad07`, and reported no conflict. The source is 7 commits behind and 2 commits ahead of current `origin/main`. `assert_deploy_ancestry_scope` passed; no source rebase was needed. |
| 7 | Single feature theme | **PASS** | The reviewed RED/GREEN pair changes only `internal/beads` for one behavior: a closed dependency whose persisted work outcome is blocked must not make its dependent work ready. No independent feature is bundled. |

## Full-suite test evidence

The full documented local runner used rootless Podman for provider-backed
coverage and completed all 40 scheduled jobs:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m GOFLAGS=-v \
LOCAL_TEST_LOG_DIR=/var/tmp/gc-deploy-ga-0rqcer-full-20260906-r2 \
  make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 34 PASS jobs, 6 attributed raw FAIL jobs; 48,020 top-level PASS, 6 top-level raw FAIL, 210 top-level SKIP events`
- `all_level_counts: 85,075 PASS, 13 FAIL, 302 SKIP events, including nested subtests`
- `raw_test_failures: 6 attributed FAIL, none diff-owned`
- `diff_tests_executed: DependencySatisfied truth table plus both new readiness scenarios across FileStore, MemStoreHonoringIDs, MemStore, NativeDoltStore, SQLiteStore, and ExecStore — all PASS`
- `skip_justification: existing suite-controlled platform, privilege, helper-process, live-provider, and opt-in skips; no candidate test was skipped`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`
- `raw_logs: /var/tmp/gc-deploy-ga-0rqcer-full-20260906-r2`

The complete changed package and the BdStore/provider integration coverage
were therefore exercised inside the required full run. Reviewer-focused
coverage independently reported 1,312 PASS, 0 FAIL, and 6 suite-controlled
SKIP events for `go test -count=1 -v ./internal/beads/...`.

## Raw failures and attribution

| Raw result | Test | Tracker and proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` | Open tracker `ga-f0uceo`. The test compares the host-installed `bd` command's evolving help surface with the source manifest. This candidate does not change `internal/bdflags` or the installed executable, and the same signature has repeatedly reproduced on unrelated candidates and exact base worktrees. |
| **FAIL — ATTRIBUTED** | `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` | Open fleet-contention tracker `ga-vkhfnj`. Its history records the exact held-open live-Dolt-directory signature on unrelated candidates `ga-nht26j` and `ga-n3zvcg`. The candidate changes dependency readiness in `internal/beads`, not process reaping or filesystem cleanup. |
| **FAIL — ATTRIBUTED** | `TestIsAgentRunning` | Open fleet-contention tracker `ga-vkhfnj`; closed bug `ga-14s617` records the exact `zsh`/setup-shell misidentification on an unrelated candidate. The candidate does not touch the `cmd/gc` liveness test or process inspection. |
| **FAIL — ATTRIBUTED** | `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | Open root-condition tracker `ga-esyijp`. Fixture `gc init` stopped on the exact beads#4566 dirty-`issues` schema-migration guard before formula assertions. The candidate changes Ready filtering, not migration or bootstrap, and the failing test file is unchanged. |
| **FAIL — ATTRIBUTED** | `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | Open tracker `ga-esyijp`. Fixture initialization stopped on the same dirty-schema migration condition before recovery behavior ran. Exact prior occurrences on unrelated candidates are recorded on the tracker. |
| **FAIL — ATTRIBUTED** | `TestCleanInstallTutorialPath` | Open tracker `ga-esyijp`. Fixture initialization hit the same beads#4566 dirty-schema condition plus the associated missing `leases` table before tutorial assertions. Exact prior occurrences on unrelated candidates are recorded on the tracker. |

`inconclusive-guard: n/a — every attribution has a root-mechanism or exact
cross-candidate proof, and no failure is diff-owned.` The candidate changes no
test runner, resource census, target, timeout, or provider-infrastructure
configuration.

## Acceptance and store-contract evidence

The source adds one canonical predicate with this truth table:

| Dependency state | Satisfied? |
|---|---|
| not closed, any outcome | no |
| closed, `gc.work_outcome=blocked` | no |
| closed, no work outcome | yes |
| closed, any other or unknown work outcome | yes |

That shared contract is applied without leaking provider behavior upward:

- MemStore and CachingStore filter against the canonical predicate.
- SQLiteStore and NativeDoltStore encode the same predicate at the query edge.
- ExecStore's conformance fixture evaluates the same persisted outcome.
- BdStore keeps native `bd ready` as its candidate source, then narrowly vetoes
  dependents whose closed blockers carry the blocked work outcome.
- FileStore and the ID-honoring memory wrapper inherit the common conformance
  contract.

The two new conformance scenarios are
`ReadyExcludesDependentWhenBlockerClosedAsWorkOutcomeBlocked` and
`ReadyIncludesDependentWhenBlockerClosedWithNoWorkOutcome`. Both passed for all
six registered conformance implementations. The direct
`TestDependencySatisfied` truth table and the changed cache/BdStore regression
tests also passed.

## Policy and static evidence

```text
make build && ./bin/gc version && ./bin/gc --help >/dev/null            PASS
make test-ci-policy                                                     PASS
make check-gomod-replace check-native-dependency-surface                PASS
make check-eventexport-isolation check-core-boundary                    PASS
make test-native-doltlite-beads                                         PASS
make check-docs                                                         PASS
LINT_CHANGED_SCOPE=tracked \
  LINT_CHANGED_REF=0d3dcdfac5e13dcc2fc3c2177b1a67f23d7f53d6 \
  make fmt-check-changed                                                PASS
go vet ./...                                                            PASS
git diff --check                                                        PASS
git config --get core.hooksPath                                         .githooks
```

The native dependency guard measured:

```text
modules=727 aws=25 azure=9 dolthub=15 googleapi=1 binary_bytes=172814442
```

`make lint-affected` expanded through reverse dependencies and reported the
following base-reproduced findings:

- gofumpt: `cmd/gc/controller_stop_client_test.go`,
  `cmd/gc/dolt_cleanup_human_test.go`, `cmd/gc/error_store.go`, and
  `internal/storebinding/storebinding_test.go` (`ga-d3m213`)
- SA1019: two uses in `cmd/gc/session_beads_test.go` and one use in
  `cmd/gc/session_lifecycle_parallel_test.go` (`ga-t88402`)
- SA4006: `internal/api/huma_handlers_sessions_command.go` (`ga-egkp4x`)

An exact detached worktree of current `origin/main` reproduced all eight
findings. Blob comparison proved all seven diagnostic-bearing files identical
between candidate and base. Current sightings were appended to all three open
trackers and read back before this gate was recorded.

## Source, pre-flight, and ancestry evidence

- The reviewed source passed the hex-only guard and resolved exactly with
  `git rev-parse --verify --quiet`.
- `gh api repos/gastownhall/gascity/commits/d6e6cd54a232e463abbd331b633fa3fa852b47f4/pulls`
  returned an empty array, so no merged or closed-PR reconciliation applies.
- The source range from merge base
  `0d3dcdfac5e13dcc2fc3c2177b1a67f23d7f53d6` is the TDD pair
  `42f4f40e68` (RED) and `d6e6cd54a2` (GREEN), both citing build bead
  `ga-a7v0ex`.
- `assert_deploy_ancestry_scope origin/main d6e6cd54a232e463abbd331b633fa3fa852b47f4 ga-0rqcer ga-a7v0ex`
  passed. No unrelated commit or `.claude/**` path rides in the deploy range.

## Disposition

Gate PASS. Create `deploy/ga-0rqcer-gate` from the exact reviewed source,
commit this checklist, push only the isolated branch, open a pull request
against `main`, publish `release-gate/deploy-clearance=success` on the exact
pull-request head, and route the merge request to mayor/mpr. The deployer does
not merge.
