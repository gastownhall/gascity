# Release gate: bounded nudge-queue flock hold time

- Deploy bead: `ga-xc19j7`
- Reviewed candidate: `21b2526cf49e36b0da19751d30e3c97105b133db`
- Original gate base: `origin/main@0f156de0532ee95eb1da91fbb0e0754ce55bf30a`
- Resume base: `origin/main@9c974557832facbd82f75175f1a58285fc1bae3f`
- Deploy mode: remote; push remote would be `fork`
- Evaluated: 2026-09-10
- Overall disposition: **PASS**. Mayor granted the exact criterion-3 waiver
  requested by this gate as
  `mayor-2026-09-10-ga-xc19j7-c3`. The waiver is limited to
  `TestSendReloadControlRequestNoChange` at the exact reviewed commit and does
  not cover any other test. The unchanged source still merges cleanly with the
  refreshed base.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Original review bead `ga-850t8o` records PASS. Its delta verdict independently pins and PASSes the rebased candidate `21b2526cf49e36b0da19751d30e3c97105b133db`; the deploy bead's body and `metadata.commit` agree on that resolved commit. |
| 2 | Acceptance criteria met | PASS | See the acceptance-evidence section below. |
| 3 | Tests pass | **PASS WITH INDEPENDENT WAIVER** | The documented full-suite command ran all 40 jobs: 27 jobs PASS, 13 jobs FAIL, 0 omitted; test output contains 45,779 PASS, 16 FAIL, and 199 SKIP results. All six diff-owned tests ran twice and PASSed both times with zero FAIL/SKIP. Fifteen failures are attributed to tracked conditions outside the diff. Mayor independently verified that `TestSendReloadControlRequestNoChange` executes none of the changed production statements and granted narrow waiver `mayor-2026-09-10-ga-xc19j7-c3` for its exact five-second initial-reconcile timeout on this exact reviewed source. No other failure is waived. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make vet`, `LINT_BASE=origin/main LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected`, and `git diff --check origin/main...HEAD` all exited 0. Affected lint selected the full repository and reported `0 issues`. |
| 3c | CI-config lane | PASS | `n/a (no CI-config change)`; the candidate changes only two `cmd/gc` files and two `internal/nudgequeue` files. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer recorded no style or security findings and no uncovered acceptance criteria; unresolved HIGH count is 0. |
| 5 | Final branch clean | PASS | `git status --porcelain` was empty in the detached candidate worktree before this gate record was written. |
| 6 | Branch diverges cleanly from main | PASS | On resume, `git merge-tree --write-tree --messages origin/main 21b2526cf49e36b0da19751d30e3c97105b133db` exited 0 against refreshed `origin/main@9c974557832facbd82f75175f1a58285fc1bae3f` and produced tree `5556a0d0ef469c3299c98ac431c493225549c648`. The source is 284 commits behind and 4 commits ahead of the refreshed base; no bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Four commits and four files form one nudge-queue lock-bounding theme: bounded flock acquisition, bounded maintenance, debounce, lock-free liveness read, and their tests. Ancestry scope passed for `ga-xc19j7`, `ga-850t8o`, `ga-2kzci3`, `ga-ls3owy`, and `ga-jcvvm5`. |

## Acceptance evidence

- `WithState` delegates to a bounded nonblocking `LOCK_EX|LOCK_NB` polling
  implementation with one tunable default wait budget and a descriptive timeout
  error naming the queue lock and wait duration.
- Every production maintenance call is bounded by
  `nudgeEnqueueMaintenanceBudget`. `noMaintenanceDeadline()` remains only as an
  unused test helper/declaration; there are no production calls. The implementation
  covers seven current call sites, including the sweep added after the original
  six-site acceptance text was written.
- `shouldKeepNudgePollerAlive` uses the lock-free state snapshot path rather than a
  mutating maintenance read.
- Diff-owned tests cover both bounded-wait entry points, bounded deep-backlog
  convergence with preservation, fake-clock convergence, same-tick debounce, and
  nonblocking liveness reads. Each test ran twice in the full suite and PASSed.
- `git diff ... -- internal/nudgequeue/store.go` is empty, preserving the
  bead-shadow/state-file coherence boundary and on-disk format.
- `make vet` passed and the full documented suite was executed.

## Criterion 3 evidence

Environment was prepared before testing with rootless Podman 5.8.4 at
`unix:///run/user/1000/podman/podman.sock`,
`TESTCONTAINERS_RYUK_DISABLED=true`, and the pinned Dolt image cached.

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-xc19j7-full.n4yVBg make test-local-full-parallel
test_cmd_scope: full-suite
job_counts: PASS=27 FAIL=13 OMITTED=0 TOTAL=40
test_counts: PASS=45779 FAIL=16 SKIP=199
waiver_ref: mayor-2026-09-10-ga-xc19j7-c3
ci_lane_run: n/a (no CI-config change)
```

The 199 skips are suite-declared platform, feature, integration-opt-in, or
short-mode skips from the full command. None is diff-owned. No command-line
filter or package narrowing was used.

`diff_tests_executed` (all PASS twice; zero FAIL/SKIP):

- `TestRunNudgeQueueMaintenanceSweep_BoundedPassPreservesBacklogThenConverges`
- `TestListQueuedNudgesForTarget_BoundedMaintenancePreservesBacklogOnStaleNow`
- `TestQueuedNudgeMaintenanceDebounce_SkipsRedundantSameTickSweep`
- `TestShouldKeepNudgePollerAlive_DoesNotBlockOnHeldQueueLock`
- `TestWithState_TimesOutInsteadOfBlockingForever`
- `TestWithStateBounded_TimesOutInsteadOfBlockingForever`

### Failure attribution

| Failure(s) | Tracker | Attribution evidence |
|---|---|---|
| `TestSendReloadControlRequestNoChange` | `ga-vcxrxa` (the internally-authored fix awaiting rebase); prior exact sightings/root cause `ga-movzgb`; waiver `mayor-2026-09-10-ga-xc19j7-c3` | Not diff-owned and the exact raw five-second initial-reconcile poll has failed on unrelated candidates under full-suite contention. The present failure is the canonical `timed out waiting for initial reconcile` at 6.73s. The failing package is `cmd/gc`, so the deployer correctly declined to self-attribute it. Mayor independently ran coverage on this exact source and confirmed the test executes 0 of 89 statements in the candidate's changed production regions, then granted the narrow waiver recorded above. |
| `TestCompactScriptRealDoltRemotePush`, `TestRunSnapshot_Integration_RealDoltRoundTrip`, `TestSweep_ReapsRealDoltDataDirAfterSIGKILL`, `TestReaperWorkflowRootCleanupRealDoltSemantics` | `ga-cp7r41` | Each external `dolt init` setup process was killed after 30–69s before the scenario under test began. None of the failing packages overlaps the four-file candidate diff. |
| `TestBdStoreConditionalWriterConformance` | `ga-vkhfnj` | External `bd init` timed out after 2m under the same full-suite host-contention run; its scaffold subtest and subsequent real-bd release tests passed. Package `internal/beads` has no diff path overlap. |
| `TestHumaBinary_CityCreateAsync`, `TestGraphWorkflowSuccessPath`, `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaRetriesTransientReviewerStep` | `ga-esyijp` | All failed during external `bd` initialization on pre-existing dirty schema tables (`issues`, `dependencies`, `labels`, `comments`) and cite `gastownhall/beads#4566`, before candidate behavior. No path overlap. |
| `TestE2E_SuspendResume_City` | `ga-dc9utn` | Exact proven `citysus.report`-missing suspend/resume condition at 94.33s. The candidate does not touch the reconciler suspend/wake path or the failing test package. |
| `TestCatalogMatchesProductionWiringAndDocumentation` (unit and integration copies) | `ga-cojd80` | Deterministic expired `runtime.Provider` waiver ledger; candidate does not touch `internal/testutil/providerledger`. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Installed-`bd` flag manifest drift; candidate does not touch `internal/bdflags`. |
| Both `TestGetKeyBinding_CapturesDefaultBinding*` variants | `ga-k3fxvj` | Host tmux exposes empty default bindings; candidate does not touch `internal/runtime/tmux`. |

All cited records were opened and verified to predate this run, and the new
sightings were appended and read back. Criteria 3a clauses 1, 2, and 3 are met
for the reload-poll failure, but clause 4 is not; the bead's mayor ruling directs
the deployer to request a waiver in precisely that case. That request was made
as peek-verified mail `gm-wisp-wdznl`, and mayor supplied the narrow waiver
recorded above.

### Pre-push fast-gate attribution

The ordinary push ran the repository's 10-job fast gate. All six `cmd/gc`
shards, both gate self-tests, and the filesystem cross-compile lane passed.
`unit-core` raw-failed on exactly two pre-existing conditions:

- `TestProviderLiveClaudeKindPath` -> `ga-iepsvr`: the live `herdr` provider
  reported the tracker's exact `agent_pane_busy` plus startup-delivery timeout
  signature. The tracker predates this run, and
  `go list -deps ./internal/runtime/herdr` contains neither changed production
  package. The candidate has no `internal/runtime/herdr` test or path overlap.
- `TestCatalogMatchesProductionWiringAndDocumentation` -> `ga-cojd80`: the
  provider catalog reported the tracker's expired `runtime.Provider` waivers.
  The tracker predates this run, and
  `go list -deps ./internal/testutil/providerledger` contains neither changed
  production package. The candidate has no provider-ledger path overlap.

Both sightings were appended to their open trackers and read back. They meet
all four non-diff-owned attribution clauses via mechanism proof, so the shared
protocol authorizes a `--no-verify` retry for this exact head. Raw log:
`/var/tmp/gc-local-tests.AyJDDn/unit-core.log`.

```text
pre_push_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | clause 3(a) mechanism; no path overlap
pre_push_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | clause 3(a) mechanism; no path overlap
```

## Resume validation and disposition

- The deploy bead's authoritative `**Commit:**` field and `metadata.commit`
  still agree on `21b2526cf49e36b0da19751d30e3c97105b133db`, which resolves as
  a commit in this repository.
- GitHub's commit-to-pull-request lookup returned no existing pull request for
  the reviewed source. This is internally authored work, so no contributor
  interaction hold applies.
- `assert_deploy_ancestry_scope` passed again against the refreshed base with
  accepted beads `ga-xc19j7`, `ga-850t8o`, `ga-2kzci3`, `ga-ls3owy`, and
  `ga-jcvvm5`.
- Mayor's waiver explicitly names this exact source and carries across only a
  clean rebase whose stable patch ID is unchanged. No rebase occurred here; the
  reviewed commit itself is unchanged.
- The full-suite evidence remains evidence on the exact final feature commit;
  only the moving-base conflict check was invalidated, and criterion 6 was
  therefore rerun against `origin/main@9c974557832facbd82f75175f1a58285fc1bae3f`.

All release criteria now pass. Cut the isolated `deploy/ga-xc19j7-gate` branch
from the exact reviewed source, commit this amended checklist, push only that
isolated branch to the fork, open a pull request against
`gastownhall/gascity:main`, publish
`release-gate/deploy-clearance=success` on the exact pull-request head, and
route the merge request to mayor. The deployer does not merge.
