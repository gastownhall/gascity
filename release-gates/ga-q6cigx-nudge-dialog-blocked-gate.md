**Verdict:** **PASS**

# Release gate: defer queued nudges blocked by dialogs

- Deploy bead: `ga-q6cigx`
- Build/rework bead: `ga-xo0m1j`
- Feature beads: `ga-1yqxh7.1`, `ga-1yqxh7.2`
- Review bead: `ga-fjvsdy`
- Reviewed source: `36c17c3b9a0d61b7cfce909cc4b5e15c7c5753c1`
- Base evaluated: `origin/main@a5e8598acbc808992786e7c8015b093d2121d3b1`
- Merge base: `ebbb019f528e0a6232ecb5748c681f33b6955a71`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-18

GitHub's commit-to-pull-request lookup returned no pull request carrying the
reviewed source, so there is no already-merged or closed target to reconcile.
`docs/PROJECT_MANIFEST.md` is absent from this source and current main; this
record applies the deploy protocol's current seven release criteria and the
source beads' acceptance contracts.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-fjvsdy` records `verdict: pass`, `tests_green: true`, no uncovered criteria, no blocker/major/minor security findings, and the exact reviewed commit above. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | The runtime exposes an optional detection-only `DialogAwareProvider`; tmux implements it by inspecting the agent pane without sending input. The queued-nudge poller checks for blocking dialogs after activity-idle evaluation but before claiming queue items, defers delivery without consuming an attempt, and emits the typed `nudge.dialog_blocked` event. Other providers retain the previous behavior. The nine diff-owned tests covering matcher, gate, event registration, payload encoding, and deferred delivery all passed in the full suite. Generated OpenAPI, Go client, TypeScript client, and dashboard bundle are consistent. |
| 3 | Tests pass | **PASS with attribution** | The documented 40-job full local suite completed 36 jobs green and 4 red, with **82,264 PASS / 4 FAIL / 320 SKIP** top-level and subtest results. Three failures stopped during external `bd init` on the predating shared-server schema-migration condition tracked by `ga-esyijp`; the fourth is the predating stale integration pin assertion tracked by `ga-rnwg5u`. All nine diff-owned tests passed, with zero FAIL/SKIP. Full details are below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `go build ./...`, and `LINT_BASE=origin/main make lint-new` exited 0; lint reported `0 issues`. `make spec-ci`, `make dashboard-ci`, `git diff --check`, and `gofmt -l` on every changed Go file were also clean. |
| 3c | CI-config lane | **PASS / n/a** | No workflow, CI job, matrix, timeout, or required-check configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no style, security, or specification findings and no uncovered acceptance criterion; unresolved HIGH count is 0. |
| 5 | Final branch clean | **PASS** | The sanctioned deployer worktree was clean at the detached reviewed SHA after the full suite, generated-artifact checks, and dashboard build. `.githooks` owns `core.hooksPath`, verified by `make check-hooks`. The built dashboard preview served HTTP 200 from `127.0.0.1:43179`. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree origin/main 36c17c3b9a0d61b7cfce909cc4b5e15c7c5753c1` exited 0 and produced tree `4e481e0c62cec039cb157fe45fa547cfae44634d`. The candidate is 8 commits behind and 3 ahead of the base; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The implementation, generated artifacts, prior failed-gate record, and rework commit all belong to one dialog-aware queued-nudge deferral feature. `assert_deploy_ancestry_scope` passed for `ga-q6cigx`, `ga-xo0m1j`, `ga-1yqxh7.1`, and `ga-1yqxh7.2`; no unrelated ancestry or `.claude/**` path is present. |

## Acceptance evidence

- `runtime.DialogAwareProvider` is an optional detection contract; providers
  without it follow the existing activity-idle behavior unchanged.
- `ContainsBugReportDraftModal` detects the known bug-report dialog and
  `ContainsAnyBlockingDialog` returns its stable kind without mutating the pane.
- The tmux implementation resolves the agent pane, captures the existing prompt
  observation window, and performs detection only. It never dismisses the
  dialog or sends a key that could destroy an agent-authored draft.
- `tryDeliverQueuedNudgesByPoller` performs the dialog check before queue claim,
  attempt increment, or nudge submission. A blocked pane therefore leaves the
  queued item pending and avoids duplicate re-paste.
- `nudge.dialog_blocked` is registered in `KnownEventTypes` with a typed
  `NudgeDialogBlockedPayload`; emission is best-effort from the gate and is not
  attached to dead-letter handling. No per-item debounce state was added.
- `make spec-ci` and `make dashboard-ci` found no drift in the generated API
  specifications, Go client, TypeScript client, or embedded dashboard bundle.

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-q6cigx-full.Xf7gVe/shards isolated-test-run.sh -- bash -c 'make test-local-full-parallel'
test_cmd_scope: full-suite
job_counts: PASS=36 FAIL=4 TOTAL=40
test_counts: PASS=82264 FAIL=4 SKIP=320
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
wrapper_tripwire: none
shard_log_manifest_sha256: 7256ec8f7010566e8bc4862ea4bfdba67bad1936ee3eec0272242b011417ca4b
```

The rootless Podman socket was active before the run. Cached
`dolthub/dolt-sql-server:2.1.7` and `dolthub/dolt:2.1.7` images matched
`deps.env`'s `DOLT_VERSION=2.1.7` pin, and Ryuk was disabled per the host
isolation contract. The 320 skips are suite-declared platform, live-provider,
optional-integration, external-contract, or helper-sentinel skips from unchanged
tests. No diff-owned test skipped.

`diff_tests_executed` (each PASS at least twice; zero FAIL/SKIP):

- `TestContainsBugReportDraftModal`
- `TestContainsAnyBlockingDialog`
- `TestPollerSessionIdleEnoughUnchangedWithoutDialogAwareProvider`
- `TestPollerSessionIdleEnoughReturnsFalseWhenBlockedByDialog`
- `TestPollerSessionIdleEnoughUnchangedWhenNotBlockedByDialog`
- `TestTryDeliverQueuedNudgesByPollerEmitsNudgeDialogBlockedWhenBlocked`
- `TestNudgeDialogBlockedIsAKnownEventTypeWithATypedPayload`
- `TestNudgeDialogBlockedPayloadRoundTrips`
- `TestNudgeDialogBlockedPayloadOmitsEmptyBeadID`

### Failure attribution

Both cited trackers predate this run, carry `gate-tracker`, remain open, and
were opened before attribution. This run's sightings were appended to each
tracker and read back successfully.

| Failure | Job | Criterion 3a evidence |
|---|---|---|
| `TestAdoptPRFormulaCompileAndRun` | `integration-review-formulas-basic-1-of-2` | `ga-esyijp`; clause 3(b), CROSS-PR. The tracker contains the exact test and shared-server pending-migration refusal on unrelated candidates, including `ga-lmy6yj` and `ga-966mj8`. The test stopped in fixture `gc init` before formula or nudge behavior. The candidate does not modify schema migration, store bootstrap, or `test/integration`. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `integration-review-formulas-retries-1-of-2` | `ga-esyijp`; clause 3(b), CROSS-PR. The exact test/signature appears on unrelated candidates including `ga-i4jdav`, `ga-vmhbzz`, and `ga-162qd2`. The test stopped in fixture `gc init` before formula behavior. No diff path overlaps the failing test. |
| `TestCleanInstallTutorialPath` | `integration-rest-full-2-of-8` | `ga-esyijp`; clause 3(b), CROSS-PR. The exact test/signature appears on unrelated candidates including `ga-d8g12r`, `ga-7t2jsc`, and `ga-3r36gw`. The test stopped during external store initialization before the tutorial scenario. No diff path overlaps the failing test. |
| `TestPinnedIntegrationBeadsModuleVersion` | `integration-rest-full-6-of-8` | `ga-rnwg5u`; clause 3(a), MECHANISM. This deterministic test only compares the `deps.env` `BD_VERSION=v1.3.0` pin with its unchanged hard-coded `v1.3.0-rc.2` expectation. The mismatch is identical at merge-base `ebbb019f…` and current `origin/main`; the candidate changes neither file and has no `test/integration` path overlap. |

## Disposition

All seven release criteria pass. Cut `deploy/ga-q6cigx-gate` from the exact
reviewed source, commit this checklist, push only that isolated branch, open a
pull request, publish exact-head deploy clearance, and route the merge request
to mayor for MPR. The deployer does not merge.
