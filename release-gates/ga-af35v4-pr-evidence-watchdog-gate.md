# Release gate: PR evidence watchdog (`ga-af35v4`)

**Verdict: PASS**

- Deploy source: `804ca93aa4621f354515bb420c5429035822b664`
- Base checked: `origin/main@1807cf018045e9f225993d97cf6daea37e2ce6e9`
- Deploy mode: remote; push remote: `fork`
- Deploy branch: `deploy/ga-af35v4-gate`
- Build bead: `ga-oaz41a.2`
- Review bead: `ga-vq8kck`
- Existing-target preflight: no PR carries the deploy source

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The original review bead records PASS. After the builder rebased onto current main, the reviewer independently verified content identity, reran the scoped lanes, and recorded `VERDICT: PASS` pinned to the exact deploy source above. |
| 2 | Acceptance criteria met | PASS | Independent inspection and the focused suite cover the exact `pull_request_target` trigger set, read-only permissions, trusted-base-only checkout, explicit PR-head SHA lookups, pagination, bounded 25-minute observation, fail-closed state transitions, opt-in suite labels, duplicate-attempt precedence, and text-based summaries. The watchdog never dispatches suites or executes PR-head content. |
| 3 | Tests pass | PASS with attributed failures | The documented 40-job CI-equivalent sweep completed with 32 PASS / 8 FAIL / 0 SKIP jobs. All eight red jobs match pre-existing, non-diff-owned signatures with qualifying proof or an existing mayor standing authorization. The diff-owned focused suite completed 106 PASS / 0 FAIL / 0 SKIP. Details are below. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, fresh checkout-local-cache `make lint-affected`, `make fmt-check-changed`, `make vet`, `go build ./...`, and `git diff --check` all pass. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no unresolved HIGH finding. The trusted-boundary review found no PR-head execution, secret access, write permission, or mutation surface. |
| 5 | Final branch is clean | PASS | The reviewed source was clean before this checklist was updated; after the gate commit, `git status --short` is empty. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 804ca93...` returned 0 with tree `e17b8be92474751f890274316e58dbfa703ce849`. `assert_deploy_ancestry_scope origin/main 804ca93... ga-af35v4 ga-oaz41a.2` returned 0. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The workflow, `scripts/prwatchdog` implementation and tests, Makefile policy registration, and gate record form one cohesive fail-closed PR-evidence watchdog. |

## Criterion 3 evidence

### Required full suite

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 make test-local-full-parallel
test_counts: 32 PASS, 8 FAIL, 0 SKIP jobs (40 total)
container_evidence: rootless Podman available; pinned dolthub/dolt:2.1.7 image cached
logs: /var/tmp/gc-local-tests.HQ6JNT
```

The raw failures remain visible here; none is rewritten green:

- `failure_attribution: TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext -> ga-taq13x | clause 3b cross-branch proof: ga-8pkpor independently hit and fixed the identical lingering embedded-Dolt writer versus TempDir cleanup race in this exact test; candidate paths do not overlap internal/doctor`.
- `failure_attribution: TestBdFlagManifestCurrent -> ga-gqxh5s | clause 3d/base and cross-PR proof: the installed-bd flag drift reproduces on clean main and many unrelated branches; candidate paths do not overlap internal/bdflags`.
- `failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg | clause 3b cross-PR proof: repeated unrelated branches fail with the same agent_pane_busy signature; also covered by waiver_ref mayor-2026-08-20-herdr-pane-standing; candidate paths do not overlap internal/runtime/herdr` — raw result **FAIL-WAIVED**.
- `failure_attribution: TestGetKeyBinding_CapturesDefaultBinding and TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-afqddr | clause 3a/3d proof: the host tmux 3.7b single-key query deterministically returns an empty default table and both exact tests reproduce on clean main; candidate paths do not overlap internal/runtime/tmux`.
- `TestAdoptPRFormulaCompileAndRun`, `TestHumaBinary_SessionMessageAsync`, and `TestGraphWorkflowFailureRunsCleanup` failed during fixture initialization with the exact gastownhall/beads#4566 dirty-table schema-migration signatures. They are **FAIL-WAIVED** under `ga-lpfjhc` / mayor standing authorization `ga-6bnc42`; the occurrence is logged on `ga-lpfjhc`. The candidate cannot alter Dolt schema migration or store bootstrap.

For every attributed failure: the test file is not diff-owned, its tracker predates this run and covers the exact test/signature, its package has no path overlap with the candidate, and a clause-3 proof landed. The inconclusive path and its added-test-load guard were not used.

### Diff-owned tests

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true go test -count=1 -json ./internal/testenv/... ./scripts/prwatchdog/...
test_counts: 106 PASS, 0 FAIL, 0 SKIP
diff_tests_executed: internal/testenv 56 PASS; scripts/prwatchdog 50 PASS
guard_test: TestRequiresDedicatedTestenvImportFile PASS
waiver_ref: none
```

The command package `scripts/prwatchdog/cmd/watchdog` correctly reports no test files; its behavior is exercised by the parent package tests. No diff-owned test skipped.

### Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS.
- `GOLANGCI_LINT_CACHE=$PWD/.gc-lint-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed` — PASS.
- `make vet` — PASS.
- `go build ./...` — PASS.
- `git diff --check origin/main...HEAD` — PASS.
- `.githooks` is the configured hooks path.

### Final-head pre-push gate

The mandatory fast pre-push gate completed 9 PASS / 1 FAIL jobs. The sole raw
failure was `TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails`:
it hit the tracked five-second stop timeout instead of reaching the bead-store
shutdown assertion.

`failure_attribution: TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails -> ga-tvgyen | clause 3b cross-branch proof: the tracker predates this run and records the exact timeout/assertion failure on unrelated ga-soa96t; the candidate has no cmd/gc path overlap`. The test is not diff-owned, all other fast jobs passed, and a clause-3 proof landed. Per the non-diff-owned pre-push protocol, this attribution was recorded before pushing the exact final head with `--no-verify`.

## Acceptance and trust-boundary audit

- Workflow events are exactly `opened`, `reopened`, `synchronize`, and `ready_for_review`; the required check is `Evidence / critical-path suites`.
- Permissions are read-only and limited to checks, pull requests, and contents. Checkout is pinned to `github.event.pull_request.base.sha` with credential persistence disabled.
- API queries use the explicit PR head SHA, exact check names, and pagination. Malformed input and API/auth/rate-limit/timeout errors fail closed.
- The 25-minute deadline is centralized and bounded; duplicate check attempts select the newest relevant run.
- `needs-mac` and `needs-review-formulas` are the only opt-in signals. Missing requested evidence fails; unrequested suites are reported as not requested, never as passed.
- The summary uses explicit text states, and the watchdog performs no dispatch, commenting, labeling, status mutation, or ruleset change.

## Disposition

Gate PASS. Push only the isolated `deploy/ga-af35v4-gate` branch, open a PR from the fork, publish deploy clearance on the exact gated head, and route the merge request to the mayor. The deployer does not merge.
