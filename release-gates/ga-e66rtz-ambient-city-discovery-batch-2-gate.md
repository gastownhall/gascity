# Release Gate: Explicit city paths for command tests, batch 2

- Deploy bead: `ga-e66rtz`
- Source review bead: `ga-do76zk`
- Reviewed commit: `0230167ae9cdec48254c84ece95ad8063cd76a91`
- Gated commit after bounded self-rebase: `7868a0148515db3e59e635dd0e2ec5e909876386`
- Base: `origin/main@9efcfb5411be1056e2a824bacebc65421a98f982`
- Deploy branch: `deploy/ga-e66rtz-gate`
- Gate date: 2026-07-25
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this
  checkout, so this checklist uses the active deployer criteria and `TESTING.md`.

## Gate Results

Criterion 6 was evaluated first as required.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-do76zk` is closed with `REVIEW VERDICT: PASS`. Its independent review covered the complete 17-line diff, build, vet, focused tests, security, coverage, and CI-integrity concerns. |
| 2 | Acceptance criteria met | PASS | The patch adds `t.Setenv("GC_CITY_PATH", dir)` immediately after the existing `t.Chdir(dir)` at 17 sites in the four specified `cmd/gc` test files. The focused smoke passed all 17 affected tests. No production code changed. |
| 3 | Tests pass | PASS | On gated commit `7868a0148`, `go build ./...` and `go vet ./...` passed; the 17 affected tests passed; and `make test-fast-parallel` passed all nine jobs (`unit-core`, six `unit-cmd-gc` shards, `fsys-darwin-compile`, and `push-gate-lock-selftest`). |
| 4 | No high-severity review findings open | PASS | The source review records no security, coverage, CI-integrity, or other unresolved HIGH finding. Deployer inspection found no additional high-severity concern in this test-only change. |
| 5 | Final branch is clean | PASS | Before this checklist was added, `git status --porcelain=v1` produced no output and `git diff --check origin/main...HEAD` passed. This checklist is committed as the isolated deploy branch tip. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed commit did not contain current main, so the canonical bounded helper replayed it from `0230167ae9` to `7868a01485` on `origin/main@9efcfb5411`. `git range-diff` reported the patch as identical, the lease-protected push to the isolated deploy branch succeeded, and `git ls-remote` verified the remote at `7868a0148515db3e59e635dd0e2ec5e909876386`. |
| 7 | Single feature theme | PASS | The commit changes four `cmd/gc` test files only, with one repeated purpose: make city selection explicit instead of relying on ambient upward discovery. |

## Acceptance Checks

- PASS: The event-emit fallback test pins its temporary city explicitly.
- PASS: Six formula-show tutorial tests pin their temporary city explicitly.
- PASS: Eight formula version-check tests pin their temporary city explicitly.
- PASS: Two order show/history tests pin their temporary city explicitly.
- PASS: The change remains test-only: 17 insertions in four files.
- PASS: Parent bead `ga-klo4gz` remains sequenced after the full migration; this
  batch does not clear the ambient-discovery guard to land.

## Commands

```text
git range-diff 2c4c43b270d72365a0080530b0c3e3503f898e7d..0230167ae9cdec48254c84ece95ad8063cd76a91 9efcfb5411be1056e2a824bacebc65421a98f982..7868a0148515db3e59e635dd0e2ec5e909876386
git diff --check origin/main...HEAD
go build ./...
go vet ./...
go test -count=1 ./cmd/gc -run '^(TestEventPayloadForEmitFallsBackToStoreBead|TestFormulaShowTutorialStepCountMatchesRenderedSteps|TestFormulaShowTutorialConditionUsesDefaultVars|TestFormulaShowDoesNotRejectRequiredVars|TestFormulaShowHighlightsRequiredVars|TestFormulaShowWithPartialVarsStillShowsRequiredVars|TestFormulaShowValidatesProvidedVarsWithoutRequiringMissingVars|TestFormulaVersionCheck_MatchExitsZero|TestFormulaVersionCheck_DivergeReturnsErrExit|TestFormulaVersionCheck_DivergeShowsFormulaPath|TestFormulaVersionCheck_JSONOutput|TestFormulaVersionCheck_MissingFormulaHashErrors|TestFormulaVersionCheck_MissingRefErrors|TestFormulaVersionCheck_BeadNotFoundErrors|TestFormulaVersionCheck_FormulaNotOnDiskErrors|TestCmdOrderShowIncludesOverrideDisabledOrder|TestCmdOrderHistoryIncludesOverrideDisabledOrder)$' -v
make test-fast-parallel
```
