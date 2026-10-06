# Release Gate: session hang-detector budgets

Deploy bead: `ga-ovbpc9`  
Review bead: `ga-ws8nu7`  
Build bead: `ga-42mt5x.7`  
Reviewed source: `53343c8aed79086a51463142d55fdc766d290dc1`  
Base: `origin/main@7a36644ade7e2d21043dac51cc00937e238c2100`  
Gate date: 2026-08-20

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-ws8nu7` is closed PASS for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | The session tests define goroutine and exec hang budgets as six times their central testutil floors. All four pure hang-detector waits use the appropriate derived budget without feeding the value into production behavior or negative assertions. |
| 3 | Tests pass | PASS | The focused session suite reports 626 PASS groups, 0 FAIL, 0 SKIP under `-race`; all 147 runnable test functions in the modified test files pass by name. The required 40-job union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. All six top-level failures are tracked and structurally unreachable from this test-only diff; the two beads#4566 failures are preserved below as FAIL — WAIVED. `make test-ci-policy`, `go vet ./...`, and gofmt pass. |
| 4 | No high-severity review findings open | PASS | The reviewer independently confirmed all four sites are pure hang detectors and found no security, correctness, or style issue. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; `git diff --check origin/main...53343c8a...` passes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 53343c8a...` succeeded against `origin/main@7a36644a...`, producing `3cee374c78afabeabdacf6f19b614c9d9d0d04ed`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. |
| 7 | Single feature theme | PASS | One commit changes three `internal/session` test files to apply one package-level hang-budget convention to four coupled wait sites. |

## Acceptance Evidence

- `goroutineHangBudget` and `execHangBudget` derive from the relevant central
  testutil floors and document why they are hang ceilings rather than latency
  assertions.
- The two manager synchronization helpers and the product-metrics child file
  poll use the derived budgets.
- `TestProductMetricsDirectChildEnvSessionSubmitPoller` passes under `-race`
  in 0.01s; passing behavior does not approach the larger ceiling.
- No production file, wire type, configuration, persistence path, or runtime
  behavior changes.

## Test Evidence

```text
test_cmd: go test -race -v -count=1 -timeout 300s ./internal/session/...
test_counts: 626 PASS groups, 0 FAIL, 0 SKIP (1,065 test/subtest starts)
diff_tests_executed: 147 PASS, 0 FAIL, 0 SKIP (every runnable test function in manager_test.go and productmetrics_child_env_test.go resolved by name; hangbudget_test.go contains constants/helpers only)
waiver_ref: none for diff-owned tests
focused_log: /var/tmp/ga-ovbpc9-session-focused.log

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.bJtQ8D

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
format_lane: gofmt check on all three changed Go files — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. None is diff-owned.
Candidate changes are confined to `internal/session/*_test.go`; they cannot
execute in `cmd/gc`, `internal/bdflags`, `internal/runtime/tmux`, or
`test/integration`. The candidate-owned session package and product-metrics
hook both passed in the required union.

- FAIL — WAIVED: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
  and `TestGCLiveContract_BeadsAndEvents` failed during fixture initialization
  with the exact `gastownhall/beads#4566` dirty-table migration signature.
  Root tracker: `ga-lpfjhc`; standing authorization: `ga-6bnc42`. Both new
  occurrences were recorded and read back.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The installed
  `bd` surface is ahead of the checked-in manifest. This occurrence was
  recorded and read back.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. The host
  tmux default key table is empty. Both occurrences were recorded and read
  back.
- FAIL — attributed: `TestCleanInstallTutorialPath` -> `ga-hrdd3h`. Beads
  circuit-breaker cleanup diagnostics contaminated the expected
  `issue_prefix` stdout. This occurrence was recorded and read back.

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix + TestGCLiveContract_BeadsAndEvents -> ga-lpfjhc + standing authorization ga-6bnc42 + exact beads#4566 signature + test-only no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + separate-package no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-package no-mechanism proof
failure_attribution: TestCleanInstallTutorialPath -> ga-hrdd3h + separate-package no-mechanism proof
```

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the reviewed source after the
final `origin/main` refresh. The target has not already merged or been
superseded through a PR, so normal isolated-branch deployment applies. The
builder branch's later rebased tip was treated as provenance only.

## Commands

```text
git fetch origin
git merge-tree --write-tree origin/main 53343c8aed79086a51463142d55fdc766d290dc1
assert_deploy_ancestry_scope origin/main 53343c8aed79086a51463142d55fdc766d290dc1 ga-ovbpc9 ga-ws8nu7 ga-42mt5x.7
git diff --check origin/main...53343c8aed79086a51463142d55fdc766d290dc1
go test -race -v -count=1 -timeout 300s ./internal/session/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go vet ./...
```
