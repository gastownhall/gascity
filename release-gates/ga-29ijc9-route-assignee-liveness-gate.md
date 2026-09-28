# Release gate: route-assignee liveness

- Deploy bead: `ga-29ijc9`
- Source review bead: `ga-qmcge6`
- Build bead: `ga-r22k2y`
- Reviewed commit: `41abebfa0841560e29e86dd72410302005bbeeeb`
- Base: `origin/main@48d68d13a77647eedeeabe772f362a32954a53b2`
- Deploy mode: `remote`; push remote resolved to `fork`
- Result: **PASS** with four non-diff-owned test failures attributed to predating open trackers.

## Pre-flight

- The recorded SHA resolved as a commit to the same full 40-character value.
- No PR carries the reviewed SHA, so there is no already-merged or closed PR to reconcile.
- The commit range is one feature theme: distinguish a bare route/template assignee from a dead named-session assignee when preserving or reopening routed work.
- The repository has no `docs/PROJECT_MANIFEST.md`; the standard seven deploy criteria and `engdocs/contributors/release-gate-criteria-conventions.md` were applied.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-qmcge6` records `REVIEW VERDICT: PASS` for the exact reviewed commit. The reviewer independently resolved the SHA, inspected the four-file diff, ran build/vet/full-package tests, and confirmed the substantive source logic matches the earlier reviewed implementation. No review carryover applies. |
| 2 | Acceptance criteria met | **PASS** | Source inspection confirms that a non-empty assignee equal to its route/template is preserved only while an open session for that same template and eligible store scope exists. With no live template session it is released; a distinct dead named-session assignee is still released even when another live session shares its template. Event text now describes the bare-template case without falsely calling the route name a dead session. All six behavioral cases passed twice in the required test paths. |
| 3 | Tests pass | **PASS** | The documented full command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel` ran on the exact reviewed SHA with rootless Podman active and the pinned Dolt images cached. Result: **37/40 jobs PASS, 3 FAIL, 0 not completed**; **49,916 top-level PASS, 4 FAIL, 210 SKIP**. All six diff-owned behavioral cases ran in both required paths: **12 PASS, 0 FAIL, 0 SKIP**. Every raw failure is attributed below under criterion 3a. The 210 skips are suite-controlled helper, opt-in, platform, provider, or shard exclusions; none is diff-owned. `test_cmd_scope: full-suite`; `waiver_ref: n/a`; `ci_lane_run: n/a (no CI-config change)`; `policy_lane: make test-ci-policy — PASS`; `go build ./... — PASS`; `go vet ./... — PASS`. Logs: `/var/tmp/ga-29ijc9-full-suite.log` and `/var/tmp/gc-local-tests.Bh1Wu2`. |
| 4 | No high-severity review findings open | **PASS** | The exact-head review records no blockers or unresolved HIGH findings. The only deferred item, `ga-jaj5wi`, is a non-blocking P3 test-coverage follow-up. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty before this checklist was written; `git diff --check origin/main...HEAD` passed; `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | Against `origin/main@48d68d13a77647eedeeabe772f362a32954a53b2`, the candidate is 1 behind / 2 ahead. `git merge-tree --write-tree origin/main 41abebfa0841560e29e86dd72410302005bbeeeb` returned 0 with tree `46ffcaf50dbf2e948c94fdc3cc5789e38ea30b07`. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | Both production edits and their tests address the same routed-work reclamation distinction: a bare route/template assignee may be backed by a live ephemeral session, while a distinct named-session assignee must prove its own liveness. |

## Diff-owned test evidence

Each behavioral case below reported PASS in both the `cmd-gc-process` and integration-package `cmd/gc` paths, with no FAIL or SKIP:

- `TestFormatDeadAssigneeReopenedMessage/dead_named_session_distinct_from_route`
- `TestFormatDeadAssigneeReopenedMessage/bare_template_name_as_assignee`
- `TestFormatDeadAssigneeReopenedMessage/empty_assignee_never_reads_as_the_bare-template_case`
- `TestReleaseOrphanedPoolAssignments_TemplateAssigneeSkippedWhenSessionLive`
- `TestReleaseOrphanedPoolAssignments_TemplateAssigneeReleasedWhenNoLiveSession`
- `TestReleaseOrphanedPoolAssignments_DeadNamedAssigneeReleasedDespiteLiveTemplateSession`

`diff_tests_executed: 12 PASS, 0 FAIL, 0 SKIP (six cases, each selected twice)`

## Failure attribution

- `failure_attribution: TestSendReloadControlRequestInvalidConfig -> ga-vkhfnj | clause 1: failing test file is not diff-owned; clause 2: open tracker predates this run; clause 3 cross-change proof: this exact fixed-five-second initial-reconcile timeout is recorded on unrelated candidates, including consolidated ga-42hj7l and ga-crmtsh; clause 4: no path overlap`
- `failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-esyijp | clause 1: failing test is not diff-owned; clause 2: open tracker predates this run; clause 3 mechanism proof: external bd refused shared-server schema migration during fixture gc init before formula or candidate behavior; clause 4: no path overlap`
- `failure_attribution: TestCleanInstallTutorialPath -> ga-esyijp | clause 1: failing test is not diff-owned; clause 2: open tracker predates this run; clause 3 mechanism proof: external bd refused shared-server schema migration during fixture gc init before tutorial or candidate behavior; clause 4: no path overlap`
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause 1: failing test is not diff-owned; clause 2: open tracker predates this run; clause 3 mechanism proof: the tracker independently proves named-session retirement in buildDesiredStateWithSessionBeads/session_reconciler and the test has no routed work for this diff's orphan-release classifier to affect; clause 4: no path overlap`

Each sighting was appended to its tracker and read back after the run. The diff adds behavioral tests but does not change a test target, resource census, CI job, matrix, timeout, or required-check list. All four attributions use landed cross-change or mechanism proof, so the inconclusive guard is not invoked and no waiver is needed.

## Disposition

All seven criteria pass. Cut isolated branch `deploy/ga-29ijc9-gate` at the reviewed SHA, commit this checklist, push to the fork, open the deploy PR, publish `release-gate/deploy-clearance=success` on its exact head, and route the merge-request to mayor. The deployer does not merge.
