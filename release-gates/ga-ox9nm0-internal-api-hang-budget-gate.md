# Release gate: internal/api hang-budget consolidation

- Deploy bead: `ga-ox9nm0`
- Build bead: `ga-42mt5x.5`
- Review bead: `ga-i9pvzc`
- Reviewed commit: `093b95a7b0a770706c4879cfb43d59cbd42dd637`
- Base: `origin/main@16f2f3c8466a0f240f10ddaaf38e86d22e54f222`
- Deploy mode: remote
- Evaluated: 2026-08-22
- Verdict: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-i9pvzc` is closed with an explicit `Review PASSED` verdict at the reviewed commit. The reviewer rebuilt, vetted, and tested the exact SHA with 0 FAIL and 0 SKIP. |
| 2 | Acceptance criteria met | **PASS** | The diff adds one package-local `hangBudget = 6 * testutil.GoroutineRaceTimeout` and converts exactly ten pure hang detectors: three in `handler_session_stream_promotion_test.go` and seven in `session_structured_providers_test.go`. The two context deadlines are last-resort streaming backstops whose handlers treat cancellation and deadline expiry identically. The change touches no production file, shared `testutil` constant, scenario-input timeout, negative-assertion window, `cmd/gc`, or `dashboardbff`. |
| 3 | Tests pass | **PASS (3 attributed failures)** | With rootless Podman enabled, the documented CI-equivalent `LOCAL_TEST_JOBS=4 make test-local-full-parallel` completed all 40 jobs: **37 PASS, 3 FAIL, 0 SKIP**. Every product lane outside the three attributed host checks passed, including all six process shards, product metrics, review formulas, bd-store, both REST-smoke shards, and all eight REST-full shards. The four changed API tests passed by exact name: **4 PASS, 0 FAIL, 0 SKIP**. `diff_tests_executed`: `TestStructuredPeekEmitsSameRequestPendingUpdates` PASS; `TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect` PASS; `TestHandleSessionStreamStructuredResumesFromPaginatedRESTSnapshot` PASS; `TestHandleSessionStreamStructuredResumesFromEmptyPaginatedRESTSnapshot` PASS. `waiver_ref`: the mayor ruling in `ga-ox9nm0` dated 2026-08-22 07:22 PDT is scoped to this exact SHA, but none of its five covered failures recurred, so this run does not consume it. |
| 3a | Pre-existing failures attributed | **PASS** | All four clauses hold for each failure: the diff touches only `internal/api/*_test.go`; each failure has an open tracker; the identical signature reproduced with `-tags=integration -count=1` on current `origin/main@16f2f3c8466a0f240f10ddaaf38e86d22e54f222`; and neither failing package overlaps the diff. `failure_attribution`: `TestBdFlagManifestCurrent` -> `ga-f0uceo` + current-base repro FAIL with the same missing installed-`bd` flags; `TestGetKeyBinding_CapturesDefaultBinding` -> `ga-afqddr` + current-base repro FAIL with empty default `prefix-n`; `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-k3fxvj` + current-base repro FAIL with empty `choose-tree`. |
| 3b | Policy, static, and dashboard lanes | **PASS** | `make test-ci-policy` PASS; merge-base-scoped `make lint-affected` PASS with 0 issues across `./cmd/gc ./cmd/gen-client ./cmd/genspec ./internal/api`; `make fmt-check-changed` PASS; `go vet ./...` PASS; `make dashboard-ci` PASS. An initial local lint attempt against `origin/main` selected a false full-repository scope because this checkout is the reviewed commit rather than GitHub's synthetic merge; it surfaced unrelated generated and stale-worktree diagnostics and is not counted. Re-running against the exact merge base selected the same candidate paths as PR CI and passed with an isolated linter cache. |
| 4 | No high-severity review findings open | **PASS** | The review verdict records no security findings, no blockers, and no open high-severity findings. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at the reviewed commit after all test, static, and dashboard gates and before this checklist was written. Generated dashboard and client checks left no drift. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final fetch, `git merge-tree --write-tree origin/main 093b95a7b0a770706c4879cfb43d59cbd42dd637` produced tree `00a2bff884b6d32cd68e233ad26649a270e6e785` without conflict. `assert_deploy_ancestry_scope origin/main <reviewed-commit> ga-ox9nm0 ga-42mt5x.5` returned 0. No self-rebase was needed, preserving the reviewed and waiver-scoped SHA. |
| 7 | Single feature theme | **PASS** | One test-reliability theme in one subsystem: consolidating pure hang-detector ceilings in `internal/api` tests. The commit set contains one feature commit and no independent behavior. |

## Test evidence

- Environment: rootless Podman at `unix:///run/user/1000/podman/podman.sock` with `TESTCONTAINERS_RYUK_DISABLED=true`; repository-pinned Dolt and testcontainers images were present, including `dolthub/dolt:2.1.7` and `dolthub/dolt-sql-server:2.1.7`.
- Required union: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 make test-local-full-parallel` — **37 PASS, 3 FAIL, 0 SKIP jobs**. Candidate log: `/var/tmp/ga-ox9nm0-full-regate-20260822.log`; job logs: `/var/tmp/gc-local-tests.n8JTJT/`.
- Diff-owned tests: exact-name `go test -count=1 -v ./internal/api -run '^(TestStructuredPeekEmitsSameRequestPendingUpdates|TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect|TestHandleSessionStreamStructuredResumesFromPaginatedRESTSnapshot|TestHandleSessionStreamStructuredResumesFromEmptyPaginatedRESTSnapshot)$'` — **4 PASS, 0 FAIL, 0 SKIP**.
- Current-base attribution: `go test -tags=integration -count=1 -v ./internal/bdflags -run '^TestBdFlagManifestCurrent$'` and the two exact tmux binding tests reproduced all three candidate signatures on `origin/main@16f2f3c8466a0f240f10ddaaf38e86d22e54f222`.
- Policy/static: `make test-ci-policy`, merge-base-scoped `make lint-affected`, `make fmt-check-changed`, and `go vet ./...` all passed.
- API/dashboard: `make dashboard-ci` passed the production SPA build, shared/frontend type checks, E2E type check, dashboard Go tests, and generated-client refresh without worktree drift.
- `skip_justification`: none; the broad runner logs contain 0 SKIP lines and every diff-owned test executed.

## Disposition

All seven criteria pass. The three red runner jobs are fully attributed to current-base host/tool failures outside the diff, and all candidate-owned evidence is green. Cut the isolated `deploy/ga-ox9nm0-gate` branch at the reviewed SHA, commit this checklist, push, open the PR, publish deploy clearance on the gated head, and route the merge-request to the mayor.
