# Release gate: feedback-survey modal nudge delivery (`ga-m59u2z`)

- Overall verdict: **PASS with attributed raw test failures**
- Evaluated: 2026-09-01 PDT / 2026-09-01 UTC
- Deploy mode: `remote`; push remote: `origin`
- Reviewed deploy source: `23f7db0ba705c9358b964c4802d778e8b520dd5d`
- Source branch: `builder/ga-did5ck` (provenance only)
- Base evaluated: `origin/main@5c9e7810f0f8efb8bdb0aae61e9b89567ab8d979`
- Scoped re-review bead: `ga-hkf9hm`
- Original feature review bead: `ga-t7geya`
- Rebase-remediation bead: `ga-did5ck`

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this gate
uses the seven release criteria embedded in `mol-deployer-gate` and the
deployer prompt. The pre-flight query found no pull request associated with
the reviewed source; the normal deploy path applies.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The scoped re-review `ga-hkf9hm` is closed `pass` on the exact reviewed commit and records no finding. The original substantive review `ga-t7geya` is also closed `pass`. |
| 2 | Acceptance criteria met | **PASS** | Independent source inspection confirms the single-line survey matcher recognizes both Claude Code variants, the tmux delivery path clears the composer before dismissing with `0` plus Enter, rechecks and retries once, and performs dismissal before literal nudge paste. `WaitForIdle` and `Provider.Nudge` are unchanged. The real-tmux end-to-end test passes. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented complete matrix, `make test-local-full-parallel`, scheduled all 40 jobs and completed **37 PASS / 3 FAIL / 0 SKIP jobs**. Three non-diff-owned failures are retained and attributed below; no candidate-owned failure remains. All six `cmd/gc` process shards and all three `internal/runtime/tmux` shards passed. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is attributed to `ga-f0uceo`; `TestGraphWorkflowSuccessPath` and `TestE2E_SuspendResume_City` are attributed to predating consolidated host-contention tracker `ga-vkhfnj`. Each exact sighting and its reachability proof was appended to the tracker. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, fresh-cache affected-package lint, changed-file formatting, `go vet ./...`, and diff whitespace checks passed. Affected lint reported `0 issues`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, job matrix, timeout, required-check list, runner policy, or CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Both reviews are closed `pass`. The original review records two non-blocking observations but no high-severity or request-changes finding; the scoped baseline re-review records no finding. |
| 5 | Final branch clean | **PASS** | The detached reviewed-source worktree remained clean after build, full-matrix, focused, and static checks. This checklist is the deployer's only change and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 23f7db0ba705c9358b964c4802d778e8b520dd5d` exited 0 against current `origin/main` and produced tree `e01710f49f17917fd7934d884e588f0c90afca38`. The candidate is two commits behind and two commits ahead of the evaluated base; no bounded self-rebase is needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits add one feedback-survey modal detector/dismissal path, its unit and real-tmux tests, and the required manifest/resource-census baseline updates. All ten changed files serve that one fix. |

## Build and smoke evidence

- `make build`: **PASS**; built `bin/gc` from the reviewed commit.
- `./bin/gc version`: **PASS** (`dev`).
- `./bin/gc --help`: **PASS**.
- `make check-schema`: **PASS**; generated schema/reference files remained clean.
- `make check-docs`: **PASS**.

## Criterion 3 evidence

Environment established before the full run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- `EXTRA_TEST_ENV` passed both values through the repository's scrubbed test wrapper
- rootless Podman 5.8.4 was reachable; the rig has no cached `dolt-tests-via-podman` cairn entry and the source contains no pinned `dolthub/dolt*` test-container tag

Test evidence:

- `test_cmd`: `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **37 PASS / 3 FAIL / 0 SKIP jobs**; the three red jobs contain three failing top-level test names
- `full_log`: `/var/tmp/ga-m59u2z-full-suite.out`
- `shard_logs`: `/var/tmp/gc-local-tests.1M8JFR`
- `skip_justification`: none; no `--- SKIP:` result was observed in any shard log
- `ci_lane_run`: n/a; no CI-config change
- `waiver_ref`: none

Diff-owned tests were re-run verbosely after the full matrix:

- `TestContainsFeedbackSurveyModal`: both survey variants and all negative cases — **6/6 top-level/subtest results PASS**.
- `TestFeedbackSurveyParkedPaneReadsIdle`: both variants — **3/3 top-level/subtest results PASS**.
- `TestNudgeSessionDismissesFeedbackSurveyBeforeDelivering`: real tmux with integration tag — **PASS** in 1.78 seconds.
- `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory` and `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce`: **2/2 PASS**.
- `diff_tests_executed`: **12/12 PASS, 0 FAIL, 0 SKIP**.
- Direct resource-baseline cross-check `TestRepositoryLedgerMatchesCensusAndDocumentation`: **PASS**.
- Focused log: `/var/tmp/ga-m59u2z-diff-tests.out` (**13 total PASS result lines** including the resource cross-check).

### Raw failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — attributed`
  - `integration-packages-core-4-of-4` reported the predating tracker's installed-`bd` flag-manifest drift, including the global CPU/database/memory/color flags and command-specific flags.
  - The candidate does not touch `internal/bdflags` or the installed CLI. `go list -deps ./internal/bdflags` contains neither changed production package, so the candidate cannot affect the manifest comparison; there is no path overlap.
  - Current sighting appended to the predating open tracker; raw log: `/var/tmp/gc-local-tests.1M8JFR/integration-packages-core-4-of-4.log`.

- `failure_attribution: TestGraphWorkflowSuccessPath -> ga-vkhfnj | clause 3(a), mechanism — attributed`
  - `integration-rest-smoke-2-of-2` completed the workflow but observed the convoy's `work_dir` metadata before cleanup had cleared it.
  - The unchanged fixture explicitly configures `[session] provider = "subprocess"`; the candidate's production behavior is confined to `internal/runtime/tmux` nudge delivery. That code is unreachable from this test, and the failure is at a post-workflow metadata assertion with no path overlap.
  - The consolidated tracker predates this run and already lists `TestGraphWorkflowSuccessPath` through `ga-g0w518`; the new exact signature was appended. Raw log: `/var/tmp/gc-local-tests.1M8JFR/integration-rest-smoke-2-of-2.log`.

- `failure_attribution: TestE2E_SuspendResume_City -> ga-vkhfnj | clause 3(a), mechanism — attributed`
  - `integration-rest-full-2-of-8` timed out after 93.54 seconds waiting for `citysus.report`, matching the consolidated `ga-yc0e3a` whole-suite signature.
  - The unchanged test performs start, suspend, session kill, and resume; it performs no nudge operation. The candidate's only tmux behavior change is inside `NudgeSession`, so the changed branch is not exercised and there is no failing-test path overlap.
  - Current sighting appended to the predating open tracker; raw log: `/var/tmp/gc-local-tests.1M8JFR/integration-rest-full-2-of-8.log`.

The candidate does add one real-tmux integration test and updates its declared
resource census. That does not weaken the attributions above: each uses a
direct mechanism/reachability proof rather than the protocol's inconclusive
host-load path, and the new test itself ran and passed in both the full tmux
shards and the focused integration run.

## Policy and static evidence

- `make test-ci-policy`: **PASS** (runner policy, CI suite coverage, `scripts/cipolicy`, `scripts/prwatchdog`, and static-scope contracts).
- `GOLANGCI_LINT_CACHE=/var/tmp/ga-m59u2z-golangci-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=3d317457dd7a7ff68f1c0333f7eb5fb399b2016c make lint-affected`: **PASS**, `0 issues`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=3d317457dd7a7ff68f1c0333f7eb5fb399b2016c make fmt-check-changed`: **PASS**.
- `go vet ./...`: **PASS**.
- `git diff --check origin/main...HEAD`: **PASS**.
- Static log: `/var/tmp/ga-m59u2z-static.out`.

## Disposition

Gate PASS. Cut `deploy/ga-m59u2z-gate` from the exact reviewed source, commit
this checklist there, push the isolated branch, and open a pull request. Merge
authority remains with mayor/mpr; the deployer does not merge.
