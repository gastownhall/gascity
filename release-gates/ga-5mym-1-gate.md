# Release Gate: ga-5mym.1 — drop slow bd config set in gc-beads-bd op_init + lint guard

**Deploy bead:** ga-ss3v
**Originating bead:** ga-5mym.1
**Branch:** `builder/ga-5mym-1` (fork: `quad341/gascity`)
**Commits:** 886874a2, db51865c
**Verdict:** PASS

## Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `gascity/reviewer-1` PASS verdict in ga-ss3v notes; spec compliance, security, and shape-divergence justification all confirmed. |
| 2 | Acceptance criteria met | PASS | TestGcBeadsBdNoBdConfigSet (untagged Go-level lint), TestGcBeadsBdInitFastPath, TestGcBeadsBdInitPinsManagedDoltEnvForBdSubcommands all pass. Three dependent fake-bd tests in beads_provider_lifecycle_test.go updated to drop config.env/config-db.log assertions. |
| 3 | Tests pass | PASS | `go build ./...` clean, `go vet ./cmd/gc/...` clean, focused test suite PASS. |
| 4 | No high-severity review findings open | PASS | Zero blockers. Reviewer noted shape divergence from original architect design as acceptable: main has SUPERSEDED the YAML-fallback approach with ensure_bd_runtime_config_value (writes directly via SQL), so the fix is just removing the redundant slow run_bd_pinned ... config set calls and adding the CI guard. |
| 5 | Final branch is clean | PASS | `git status` clean (untracked `.gitkeep` only). |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main HEAD` writes merge tree without conflicts. Two commits ahead of origin/main. |

## Pre-existing failures (NOT introduced)

- TestDocDirCoverage (worktrees/ga-mol-bq54/ has markdown not in docTreeDirs) — unrelated, baseline-reproducible per builder.
- TestInitBeadsForDirBdMaterializedScript* and TestGcBeadsBdInitRetriesRootStoreVerification — fail with 'connection refused' when no test dolt is running; environmental, unrelated.
