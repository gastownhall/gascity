# Release Gate: persistent startup-health episodes

- Deploy bead: `ga-em8g4o`
- Build bead: `ga-o04bfr.1.1`
- Review bead: `ga-xhf54z`
- Reviewed source: `0a26e96432a514d892ca8824cda494217e6b20cf`
- Base evaluated: `origin/main@f07cfb68298afccc969508a247ef78c72e357eef`
- Deploy mode: remote
- Date: 2026-09-03
- Overall verdict: **FAIL**

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at the evaluated base; the gate therefore
uses the deployer release criteria together with `TESTING.md` and the Makefile's
documented full-suite and policy targets.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-xhf54z` is closed with `verdict: pass` for exact source `0a26e96432a514d892ca8824cda494217e6b20cf`. No review carryover is involved. |
| 2 | Acceptance criteria met | **FAIL** | The durable side-channel record and quarantine gate exist, but required behavior and coverage are missing: the model/codec has no failure-kind field; count and kind are not mirrored onto the visible session bead through the typed session front door; and the new reconciler tests use one auto-named non-pool harness plus manually-created replacement beads rather than named and pool-expanded desired-state/materialization paths. Details are below. |
| 3 | Tests pass | PASS WITH ATTRIBUTED FAILURES | The documented `make test-local-full-parallel` union completed all 40 jobs: **37 PASS jobs, 3 raw FAIL jobs, 0 omitted jobs**. Top-level test result lines: **48,419 PASS, 3 FAIL, 208 SKIP**. All 22 diff-owned tests emitted PASS twice, with **0 diff-owned FAIL and 0 diff-owned SKIP**. The three raw failures are attributed below to predating trackers with mechanism and path-overlap proof. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS WITH ATTRIBUTED LINT FINDINGS | `make test-ci-policy`: PASS. `fmt-check-changed`: PASS. `go vet ./...`: PASS. `go build ./...`: PASS. `lint-affected` expanded to a full-repository scan because the candidate predates an unrelated generated-dashboard rename, then reported four non-diff-owned findings; all four are attributed below. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check-list change)`. |
| 4 | No high-severity review findings open | PASS | Final review notes record `style_findings: none`, `security_findings: none`, and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the exact reviewed source before this checklist was created; `git diff --check origin/main...HEAD` was clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main 0a26e96432a514d892ca8824cda494217e6b20cf` exited 0 and produced tree `e705cd735ea5ab91f337a2b5bff1900d28844bb0`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Eight changed files implement and test one startup-health persistence/quarantine feature across the session and bead boundaries. `assert_deploy_ancestry_scope` passed for `ga-em8g4o`, `ga-o04bfr.1.1`, and `ga-xhf54z`. |

## Criterion 2: missing acceptance obligations

The following requirements from `ga-o04bfr.1.1` are not met by the reviewed
source:

1. **Failure kind is absent from the durable model and transition.**
   `internal/session/startup_health.go:17-23` defines metadata keys for session
   name, consecutive count, timestamps, detail, alert disposition, and
   quarantine only. `StartupHealthEpisode` at lines 43-51 likewise has no
   failure-kind field, and `RecordStartupFailure` at lines 112-137 accepts only
   detail. Consequently kind changes cannot be persisted or tested.
2. **Count/kind are not mirrored onto the visible session row.** The only
   production references to `StartupHealthConsecutiveMetadataKey` are the
   side-channel episode codec. The reconciler reads the episode to block a
   start (`cmd/gc/session_reconciler.go:3671-3679`) but never applies active
   episode count/kind metadata to the typed session front door. The acceptance
   contract explicitly requires the visible session row to preserve and expose
   those values when the threshold is reached.
3. **Named and pool-expanded pending-create paths are not covered.**
   `newSessionChaosHarness` configures a single ordinary agent with no pool
   bounds (`cmd/gc/session_lifecycle_chaos_test.go:997-1015`), and
   `createSessionIntent` creates an auto-derived ephemeral session without an
   explicit name (lines 1018-1029). The four new reconciler tests all use this
   harness; none constructs a configured named session or a pool-expanded
   pending-create session.
4. **Desired-state materialization is simulated rather than exercised.**
   `createReplacementPendingCreateBead` writes a raw replacement directly to
   the store (`cmd/gc/startup_health_reconcile_test.go:14-64`). The quarantine
   test states at lines 137-143 that its tick does not run production
   `syncSessionBeads`, then manually creates the candidate. It therefore does
   not prove the required materialization pass consults the durable episode,
   preserves/materializes one visible session row, and avoids a sixth provider
   start for both named and pool-expanded identities.

These are product-contract gaps, not review-style concerns. They require an
implementation and test update followed by a fresh review.

## Criterion 3: full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
LOCAL_TEST_LOG_DIR=/var/tmp/ga-em8g4o-full-gm-wisp-ybyl/run2/jobs
GOFLAGS=-v
make test-local-full-parallel
```

The rootless Podman socket was active before the run. Gas City's suite does not
pin a testcontainers image for this candidate path. The first invocation was
discarded as setup-only evidence because its configured job-log directory did
not exist; the command above is the corrected, complete run. Full aggregate
log: `/var/tmp/ga-em8g4o-full-gm-wisp-ybyl/run2/full-suite.log`.

- `test_cmd_scope: full-suite`
- job counts: 37 PASS, 3 raw FAIL, 0 omitted
- top-level test result counts: 48,419 PASS, 3 FAIL, 208 SKIP
- `diff_tests_executed`: 22/22 PASS, 0 FAIL, 0 SKIP; every name below emitted
  PASS in both the unit and integration-package matrices
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

Diff-owned tests:

- `internal/session/startup_health_test.go`: 17/17 PASS
  (`TestStartupHealthEpisodeFromMetadataProjectsVerbatim`, the seven
  `TestRecordStartupFailure*` cases, `TestClearStartupHealthEpisodeResetsToZeroValue`,
  and all eight `TestStore*StartupHealthEpisode*` cases)
- `internal/beads/beads_test.go`: `TestIsReadyExcludedType` PASS
- `cmd/gc/startup_health_reconcile_test.go`: 4/4 PASS
  (`TestPendingCreateFailuresAccrueStartupHealthEpisodeAcrossReplacementBeads`,
  `TestQuarantinedStartupHealthBlocksProviderStartUntilExpiry`,
  `TestStartupHealthEpisodeClearsOnFirstSuccessfulStart`, and
  `TestQuarantineGateLogsOnStartupHealthLoadError`)
- `cmd/gc/session_reconcile_test.go`: the changed shared fixture was exercised
  by the complete command matrices; no test function was added or modified

`skip_justification`: all 208 skips are non-diff-owned, explicit suite
conditions: delegated process/Dolt lanes, live-provider opt-ins, platform- or
privilege-specific cases, subprocess helper sentinels, and the pinned
`GC_INTEGRATION_BD_PERSISTENCE`/Kubernetes/tmux flags. None of the 22
diff-owned tests skipped.

### Test failure attribution

All sightings were appended to their predating trackers and read back before
this checklist was written.

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. Mechanism proof: the test shells
  out to the independently installed host `bd --help` and compares that surface
  with the static `internal/bdflags` manifest. This diff touches neither input;
  no changed path overlaps the failing test or manifest.
- `TestHumaBinary_CityCreateAsync` and
  `TestGCLiveContract_BeadsAndEvents` -> `ga-esyijp`. Both fail with the exact
  tracked beads#4566 dirty-table schema-migration signature during temporary
  city/rig store initialization. The first fails before session reconciliation;
  the second starts its pool session successfully and fails later while adding
  a rig. This diff does not change Dolt schema migration/store bootstrap, and no
  failing-test path overlaps the diff.

## Criterion 3b: policy and lint evidence

- `policy_lane: make test-ci-policy — PASS`
- `LINT_CHANGED_REF=f07cfb68298afccc969508a247ef78c72e357eef LINT_CHANGED_SCOPE=tracked make fmt-check-changed` — PASS
- `go vet ./...` — PASS
- `go build ./...` — PASS
- `LINT_CHANGED_REF=f07cfb68298afccc969508a247ef78c72e357eef LINT_CHANGED_SCOPE=tracked make lint-affected` — raw FAIL, fully attributed:
  - two `govet` inline findings and one `revive` package-comment finding under
    ignored `internal/api/dashboardspa/web/node_modules/flatted/...` ->
    `ga-039od0`. `git check-ignore` confirms the path is ignored; the candidate
    triple-dot diff touches neither it nor dashboard code.
  - `runDesiredPendingCreateTicks` parameter `ticks` always receives 30 in
    untouched `cmd/gc/session_pending_create_rollback_desired_test.go` ->
    `ga-emldy6`. The file's blob is exactly
    `85370adb95f2301c0d0e7036d5709f52ff7bf74c` at both candidate and base, which
    proves the candidate did not cause the warning. No diff path overlaps it.

The full-repository expansion was triggered by the two-dot view of a generated
dashboard asset rename that landed on newer main; the candidate's triple-dot
feature diff contains no dashboard path.

## Decision

**Gate FAIL on criterion 2.** Do not push a deploy branch, open a pull request,
publish deploy clearance, or route a merge request. Return `ga-em8g4o` to the
builder for the missing failure-kind state/metadata, typed visible-session
mirror, and named/pool/materialization coverage, then require a fresh review.
