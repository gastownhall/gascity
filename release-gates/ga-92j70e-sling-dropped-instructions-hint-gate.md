# Release Gate: sling dropped-instructions hint

Deploy bead: `ga-92j70e`  
Review bead: `ga-7tyvef`  
Reviewed source: `aa7173b9f9e06ec2f3125cff45999bb87adbfe8c`  
Base: `origin/main@8070d4741f6fd9fd010c9c3170f5a7fc3861387c`  
Gate date: 2026-08-20

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test commands in `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-7tyvef` is closed with a PASS reason for the exact reviewed source; duplicate review bead `ga-jhva9w` was closed without changing that verdict. |
| 2 | Acceptance criteria met | PASS | The advisory now recognizes the legacy sling path's automatic `gc.var.issue = beadID` stamp, still fires when the caller explicitly clears `issue`, and remains suppressed when `context_path` or `requirements_path` carries instructions. The four behavior regressions pass under the race detector. |
| 3 | Tests pass | PASS | The focused sling suite reports 212 top-level PASS, 0 FAIL, 0 SKIP under `-race`; all 155 runnable tests in the added/modified test files pass by name. The required 40-job union reports 33 PASS jobs, 7 FAIL jobs, 0 job-level SKIP. All eight top-level failures are tracked, not diff-owned, and structurally unreachable from the changed advisory formatter; the two beads#4566 occurrences are preserved below as FAIL — WAIVED under mayor's standing authorization. `make test-ci-policy` and `go vet ./...` pass. |
| 4 | No high-severity review findings open | PASS | Reviewer reports no style, security, or wire-path findings and no blocking issues. The narrower graph.v2 advisory limitation is explicitly non-blocking and outside this legacy-path fix. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed source before adding this gate record; `git diff --check origin/main...aa7173b9...` passed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main aa7173b9...` succeeded against `origin/main@8070d474...`, producing `8c84ef760710660c3d19647d26deb1dda478e344`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. |
| 7 | Single feature theme | PASS | One commit changes only three `internal/sling` files for the attached-bead dropped-instructions advisory and its directly coupled tests. |

## Acceptance Evidence

- `TestAttachedHintSuppressedWhenIssueVarCarriesBead`: PASS.
- `TestAttachedHintFiresWhenIssueVarNotStamped`: PASS.
- `TestAttachedHintSuppressedWhenContextOrRequirementsPathSupplied`: PASS.
- `TestSlingAttachFormulaWarnsWhenBeadDescriptionDropped`: PASS.
- Direct inspection confirms `BuildSlingFormulaVars` stamps `issue=beadID` on
  the legacy route unless the caller explicitly overrides it.
- The diff changes no wire type, API, dependency, config schema, persistence,
  or dispatch behavior; it controls only whether an advisory string is shown.

## Test Evidence

```text
test_cmd: go test -race -v -count=1 -timeout 300s ./internal/sling/...
test_counts: 212 top-level PASS, 0 FAIL, 0 SKIP
diff_tests_executed: 155 PASS, 0 FAIL, 0 SKIP (all runnable tests in the added/modified test files resolved by name)
waiver_ref: none for diff-owned tests
focused_log: /var/tmp/ga-92j70e-sling-focused.log

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 33 PASS jobs, 7 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.AAn5nv

policy_lane: make test-ci-policy — PASS
static_lane: go vet ./... — PASS
```

### Raw failures and attribution

The failures below remain failures in the raw output. They are not rewritten
as green. None is in a file added or modified by this diff.

- FAIL — WAIVED: `TestCleanInstallTutorialPath` and
  `TestHumaBinary_CityCreateAsync` failed during fixture initialization with
  the exact `gastownhall/beads#4566` dirty-table schema-migration signature.
  Root tracker: `ga-lpfjhc`. This candidate cannot reach Dolt schema migration
  or store bootstrap. Both occurrences were logged on `ga-lpfjhc`, satisfying
  the standing authorization recorded by mayor on `ga-6bnc42`; the raw results
  are preserved here as FAIL — WAIVED.
- FAIL — attributed: `TestSQLiteLegacySnapshotSIGKILLAtBoundaries` ->
  `ga-11wvsd`. The timeout is in `internal/storebinding/sqlite`, which the
  changed advisory formatter cannot reach.
- FAIL — attributed: `TestExportMirrorPublishesTheUserTurnBeforeTheReply` ->
  `ga-8ehfod`. That tracker was filed from this run for visibility, not used as
  independent pre-existing proof; structural separation carries attribution:
  the zcode adapter test does not invoke sling attachment or the changed pure
  formatter.
- FAIL — attributed: `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The candidate
  cannot alter the installed `bd` binary or `internal/bdflags` manifest.
- FAIL — attributed: `TestConditionChecksRunConcurrentlyWithinTheirCap` ->
  `ga-e7cxrg`. The failure is in order condition-check concurrency; the changed
  pure post-attach advisory formatter cannot alter order scheduling.
- FAIL — attributed: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. They run
  in the separate tmux provider and match the tracked empty-host-keytable
  signature.

```text
failure_attribution: TestCleanInstallTutorialPath + TestHumaBinary_CityCreateAsync -> ga-lpfjhc + standing authorization ga-6bnc42; exact beads#4566 signature; no schema/bootstrap mechanism
failure_attribution: TestSQLiteLegacySnapshotSIGKILLAtBoundaries -> ga-11wvsd + separate-package no-mechanism proof
failure_attribution: TestExportMirrorPublishesTheUserTurnBeforeTheReply -> ga-8ehfod + structural call-path proof (tracker is same-run visibility only)
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + structural no-mechanism proof
failure_attribution: TestConditionChecksRunConcurrentlyWithinTheirCap -> ga-e7cxrg + pure-formatter/order-scheduler separation
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-provider path proof
```

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the reviewed source. The target
has not already merged or been superseded through a PR, so normal isolated-
branch deployment applies.

## Commands

```text
git fetch origin main
git merge-tree --write-tree origin/main aa7173b9f9e06ec2f3125cff45999bb87adbfe8c
assert_deploy_ancestry_scope origin/main aa7173b9f9e06ec2f3125cff45999bb87adbfe8c ga-92j70e ga-7tyvef ga-tj5jbm
git diff --check origin/main...aa7173b9f9e06ec2f3125cff45999bb87adbfe8c
go test -race -v -count=1 -timeout 300s ./internal/sling/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go vet ./...
```
