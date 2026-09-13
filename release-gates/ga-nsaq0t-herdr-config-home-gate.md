# Release Gate: herdr config-home socket resolution

Deploy bead: `ga-nsaq0t`  
Review bead: `ga-nqlb8q`  
Reviewed source: `4ca2c3158e1c2065b898ba29eaaa1a02eaca5931`  
Base: `origin/main@7e92a87f18f323575fa6cce98070a4a42b02bdfb`  
Gate date: 2026-08-20

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-nqlb8q` is closed with reason `pass`; its notes record `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | `socketPath` now uses `os.UserConfigDir`, so the client follows herdr's `XDG_CONFIG_HOME` precedence. The two new config-home tests pass, the test socket listeners share the production resolver, and both real-binary regressions (`TestProviderLive` and `TestProviderLiveClaudeKindPath`) pass. |
| 3 | Tests pass | PASS | The focused herdr suite reports 80 top-level PASS, 0 FAIL, 0 SKIP; all 28 test functions in added/modified test files pass by name. The required 40-job union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. All six failures are tracked, not diff-owned, and structurally unreachable from this herdr-only diff; the beads#4566 occurrence is preserved below as FAIL — WAIVED under the mayor's standing authorization. `make test-ci-policy` and `go vet ./...` pass. |
| 4 | No high-severity review findings open | PASS | Reviewer reports no style or security blockers and no uncovered acceptance criteria; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed source before adding this gate record; `git diff --check origin/main...4ca2c315...` also passed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 4ca2c315...` succeeded against `origin/main@7e92a87f...`, producing `b1bf09a26a79a91b41333f2d1f6e10a2e9cf15a2`. `assert_deploy_ancestry_scope` passed for `ga-nsaq0t` and review bead `ga-nqlb8q`. |
| 7 | Single feature theme | PASS | The two reviewed commits touch only `internal/runtime/herdr`: XDG-aware socket discovery plus the directly coupled test-helper and regression updates. |

## Acceptance Evidence

- `TestSocketPathHonorsXDGConfigHomeOverHome`: PASS.
- `TestSocketPathFallsBackToHomeConfigWhenXDGUnset`: PASS.
- `TestProviderLive`: PASS in 1.35s with the real `herdr` binary.
- `TestProviderLiveClaudeKindPath`: PASS in 4.17s with real `herdr` and
  `claude` binaries.
- All 28 test functions in the four diff-owned test files report PASS by name:
  2 config-home tests, 13 pane-binding tests, 2 stale-server tests, and 11
  startup-delivery tests.
- Diff contains no dependency, API, configuration-schema, or migration change.

## Test Evidence

```text
test_cmd: go test -v -count=1 -timeout 300s ./internal/runtime/herdr/...
test_counts: 80 top-level PASS, 0 FAIL, 0 SKIP
diff_tests_executed: 28 PASS, 0 FAIL, 0 SKIP (every test function in the four touched *_test.go files resolved by name)
waiver_ref: none for diff-owned tests
focused_log: /var/tmp/ga-nsaq0t-herdr-focused.log

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.WpoCur

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. They are not rewritten
as green. None is in a test file added or modified by this diff.

- FAIL — WAIVED: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
  failed during fixture initialization with the exact
  `gastownhall/beads#4566` dirty-issues schema-migration signature. Tracked by
  `ga-qyh9cb` and root tracker `ga-lpfjhc`. The candidate changes only herdr
  socket discovery and cannot reach Dolt schema migration or store bootstrap.
  This occurrence was logged on `ga-lpfjhc`, satisfying the standing
  authorization recorded by mayor on `ga-6bnc42`; the raw result is preserved
  here as FAIL — WAIVED.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The candidate
  cannot alter the bdflags manifest.
- FAIL — attributed: `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` ->
  `ga-vbyn8v` (fix awaiting deployment). The candidate cannot reach
  `examples/gastown` or `internal/doltorphan`.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. They run
  in the separate tmux provider and reproduce the tracked empty-default-table
  host signature.
- FAIL — attributed: `TestCleanInstallTutorialPath` -> `ga-hrdd3h`. The
  failure is the tracked beads circuit-breaker stdout-contamination signature;
  the candidate cannot affect that logging or tutorial capture.

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-qyh9cb + ga-lpfjhc + standing authorization ga-6bnc42; exact beads#4566 signature; no schema/bootstrap mechanism
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + structural no-mechanism proof
failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-vbyn8v + structural no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-provider path proof
failure_attribution: TestCleanInstallTutorialPath -> ga-hrdd3h + structural no-mechanism proof
```

## Pre-flight

Team-authored PR [#5435](https://github.com/gastownhall/gascity/pull/5435)
is OPEN at the exact reviewed source, mergeable, and has no comments or reviews.
It has not merged, and no external contributor has engaged it. The deploy source
remains the recorded SHA; the provenance branch is not a deploy push target.

## Commands

```text
git fetch origin main
git merge-tree --write-tree origin/main 4ca2c3158e1c2065b898ba29eaaa1a02eaca5931
assert_deploy_ancestry_scope origin/main 4ca2c3158e1c2065b898ba29eaaa1a02eaca5931 ga-nsaq0t ga-nqlb8q
git diff --check origin/main...4ca2c3158e1c2065b898ba29eaaa1a02eaca5931
go test -v -count=1 -timeout 300s ./internal/runtime/herdr/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go vet ./...
```
