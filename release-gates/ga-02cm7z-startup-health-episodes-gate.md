# Release Gate: persistent startup-health episodes

- Deploy bead: `ga-02cm7z`
- Acceptance bead: `ga-o04bfr.1.1`
- Remediation bead: `ga-8kjbf1`
- Review bead: `ga-u4vqax`
- Reviewed source: `50dd6309a6c3ffd212659a0607e0268ba11b812e`
- Merge base: `734f18f45915399a900561c3f964e3af96cace0b`
- Base evaluated: `origin/main@26e5ba94010471b250202ab086148bbbeae02674`
- Deploy mode: remote
- Date: 2026-09-04
- Overall verdict: **PASS**

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at the reviewed source; this gate therefore
uses the deployer release criteria together with `TESTING.md`, the Makefile, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-u4vqax` records `verdict: pass`, no security or architecture finding, and pins the reviewed source exactly to `50dd6309a6c3ffd212659a0607e0268ba11b812e`. The SHA resolves to the evaluated commit; no review carryover is involved. |
| 2 | Acceptance criteria met | PASS | The candidate persists a typed episode keyed by session name, accrues failures across replacement beads and controller restarts, applies the existing five-failure/five-minute quarantine before `Provider.Start`, mirrors active state through typed session metadata, excludes bookkeeping records from Ready/session projections, and clears/rearms only after a confirmed successful start. Named-session, pool, transition, store, expiry, recovery, and error tests cover these behaviors. |
| 3 | Tests pass | PASS WITH ATTRIBUTED FAILURES | The documented `make test-local-full-parallel` union ran all 40 jobs: 36 PASS jobs, 4 raw FAIL jobs, 0 skipped/omitted jobs. The four failures are non-diff-owned and attributed below to trackers that predate the run. A supplemental exact-name run confirmed all 31 diff-owned top-level tests PASS, 0 FAIL, 0 SKIP (36 terminal PASS events including five subtests). `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS WITH ATTRIBUTED FIRST FAILURE | All required policy, formatting, vet, build, docs-sync, native-DoltLite, module-replace, dependency-surface, event-export, and core-boundary checks passed. The first `lint-affected` attempt widened to `./...` and encountered three tracked findings in an ignored dashboard `node_modules` tree; tracker `ga-u8z8j6` predates the run. The identical command passed at the exact reviewed SHA in a pristine disposable checkout with an isolated GolangCI-Lint cache. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check-list change in the triple-dot candidate diff)`. |
| 4 | No high-severity review findings open | PASS | The fresh review records no security finding, no architecture violation, and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | Before this checklist was created, `git status --short` was empty and `git diff --check origin/main...50dd6309a6c3ffd212659a0607e0268ba11b812e` completed cleanly. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main 50dd6309a6c3ffd212659a0607e0268ba11b812e` exited 0 and produced tree `7e249892f42f571a0175fd5c42b5e10be417e1d8`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The production and test changes form one startup-health persistence and quarantine feature across `cmd/gc`, `internal/session`, and the Beads Ready-exclusion predicate. The historical failed gate for this same feature is the only non-product artifact in the commit range. |

## Criterion 2: acceptance evidence

1. `internal/session/startup_health.go` defines the typed,
   session-name-keyed episode and its Store load/save front door. Tests cover
   metadata projection, persistence, duplicate-preventing upsert, malformed
   values, read-only loads, and store errors.
2. Each failed start records count, kind, timestamps, bounded detail, and alert
   disposition. The fifth consecutive failure applies the existing five-minute
   quarantine; a sixth provider start is blocked until expiry.
3. The durable record survives pending-create rollback, replacement beads,
   configured named-session materialization, pool expansion, and reconciler
   restarts. It is uniquely addressed by session name rather than transient
   bead identity.
4. Active count and kind are mirrored through `session.Store.ApplyPatch` onto
   the visible session record. The bookkeeping type remains excluded from
   Ready and session listing projections.
5. A durably committed successful start clears the episode and visible mirror,
   rearming the threshold. Continuation metadata and other non-success changes
   do not clear it.
6. The accounting remains a side channel to the existing pending-create
   rollback behavior. Wake-failure, rate-limit, terminal-error, and
   circuit-breaker handling are unchanged, and the complete test union found no
   candidate-owned regression in those paths.

## Criterion 3: full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true"
LOCAL_TEST_LOG_DIR=/var/tmp/ga-02cm7z-gate.3pZ6TFDk/jobs
LOCAL_TEST_JOBS=2
CMD_GC_PROCESS_TOTAL=6
GO_TEST_TIMEOUT=30m
make test-local-full-parallel
```

The rootless Podman socket was active before the run. No candidate or test path
pins a testcontainers image tag. Complete runner log:
`/var/tmp/ga-02cm7z-gate.3pZ6TFDk/full-suite-rerun.log`.

- `test_cmd_scope: full-suite`
- job counts: 36 PASS, 4 raw FAIL, 0 skipped/omitted
- `diff_tests_executed`: 31/31 top-level tests PASS, 0 FAIL, 0 SKIP
  (36 terminal PASS events including five subtests)
- supplemental diff-owned log:
  `/var/tmp/ga-02cm7z-gate.3pZ6TFDk/diff-owned-tests.jsonl`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

An earlier launcher attempt pointed `LOCAL_TEST_LOG_DIR` at a directory that
had not been created, so all workers exited before loading tests. That harness
setup error is not treated as product evidence; the corrected command above
then ran all 40 jobs without rerunning or hiding any candidate test result.

Diff-owned results by file:

- `cmd/gc/startup_health_materialization_test.go`: 2/2 PASS — configured named
  and pool-expanded real-materialization paths.
- `cmd/gc/startup_health_reconcile_test.go`: 6/6 PASS — accrual across
  replacement beads, pre-expiry blocking/post-expiry retry, count/kind mirror,
  success clear, mirror clear, and load-error logging.
- `cmd/gc/session_pending_create_rollback_desired_test.go`: 3/3 modified tests
  PASS — rollback after repeated failures, retry after quarantine expiry, and
  claim release while creation remains pending.
- `internal/session/startup_health_test.go`: 19/19 PASS — metadata projection,
  threshold/time/kind/detail transitions, reset/rearm, store round-trip,
  duplicate prevention, errors, read-only load, and listing exclusion.
- `internal/beads/beads_test.go`: `TestIsReadyExcludedType` PASS.
- `cmd/gc/session_reconcile_test.go`: the changed shared store fixture was
  exercised by the complete package matrices; it adds no test function and no
  test in that file failed or skipped.

`skip_justification`: zero full-suite jobs and zero diff-owned tests skipped.
The unit-core JSON stream contained 189 non-diff-owned conditional SKIP events,
covering subprocess helpers, platform/privilege checks, live-provider opt-ins,
and integration flags. None names a test added or modified by this candidate.

### Attributed raw test failures

Each tracker existed before the run, covers the root condition rather than an
individual test, received this run's sighting, and was read back afterward.
None of the failing test files overlaps the candidate diff.

- `TestBdFlagManifestCurrent` -> `ga-f0uceo` (mechanism proof). The test shells
  out to the independently installed host `bd --help` and compares that surface
  with `internal/bdflags/bdflags.go`. The candidate changes neither the test,
  manifest, installed binary, nor `internal/bdflags`. Log:
  `/var/tmp/ga-02cm7z-gate.3pZ6TFDk/jobs/integration-packages-core-1-of-4.log`.
- `TestHumaBinary_CityCreateAsync`, `TestCleanInstallTutorialPath`, and
  `TestGraphWorkflowFailureRunsCleanup` -> `ga-esyijp` (mechanism proof). Each
  failed during temporary city/store initialization with the tracker's exact
  beads#4566 dirty-schema migration condition, before startup-health
  reconciliation can run. The candidate changes neither the failing
  integration tests nor schema migration/provider bootstrap. Its
  `internal/beads/beads.go` change is limited to the Ready-excluded-type
  predicate and cannot execute before a store opens. Logs are the corresponding
  `integration-rest-full-{1,2,7}-of-8.log` files under the job directory above.

`failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo (mechanism:
installed-host-bd manifest drift); TestHumaBinary_CityCreateAsync,
TestCleanInstallTutorialPath, TestGraphWorkflowFailureRunsCleanup -> ga-esyijp
(mechanism: beads#4566 dirty-schema migration during temporary store init)`.

## Criterion 3b: policy and lint evidence

- `policy_lane: make test-ci-policy` — PASS.
- `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make
  fmt-check-changed` — PASS.
- `make vet` — PASS.
- `go build ./...` — PASS.
- `make check-docs` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected`
  — PASS in a pristine disposable checkout at the exact reviewed SHA, with an
  isolated GolangCI-Lint cache.

The first `lint-affected` invocation in the long-lived deployer checkout
conservatively selected the full repository because a historical gate file is
deleted relative to current `origin/main`. It then reported only two `govet`
and one `revive` finding from
`internal/api/dashboardspa/web/node_modules/flatted`, an ignored dependency
tree absent from a clean checkout. This exact full-fallback scope leak is
tracked by pre-existing bead `ga-u8z8j6`; the candidate does not touch the
dashboard or lint configuration, and the pristine exact-head run passed.
`policy_attribution: dashboard node_modules/flatted findings -> ga-u8z8j6
(mechanism: ignored worktree dependency admitted by full-repository fallback;
pristine exact-head result PASS)`.

## Decision

**Gate PASS.** Cut the isolated `deploy/ga-02cm7z-gate` branch at the exact
reviewed source, commit this checklist, push it to the internal fork, open a
pull request against `gastownhall/gascity:main`, publish deploy clearance on
the exact gated PR head, and route the merge request to the merge authority.
The deployer does not merge.
