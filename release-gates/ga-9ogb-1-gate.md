# Release Gate: ga-9ogb.1 — layout-version stamping + migration error in ValidateAgents

**Deploy bead:** ga-zzhk
**Originating bead:** ga-9ogb.1
**Branch:** `builder/ga-9ogb-1` (fork: `quad341/gascity`)
**Commits (own work):** d5b5f2d3, ace7e871
**Stacked on:** ga-tpfc.1 (PR #1583, branch `builder/ga-mol-bq54`)
**Verdict:** PASS

## Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `gascity/reviewer-1` PASS verdict in ga-zzhk notes; layout-pair matrix (3×3), headline byte-pinning, fallback-suppression, and patch/override propagation all confirmed. |
| 2 | Acceptance criteria met | PASS | 13 new tests covering: bug repro, headline pinning, all 9 matrix cells (2 migration + 7 generic), fallback=true suppresses, patch/override preserve layout, city-inline edge case, field_sync_test exclusion, pool tombstone. |
| 3 | Tests pass | PASS | `go build ./...` clean, `go vet ./internal/config/...` clean, `go test ./internal/config/` PASS, `go test ./cmd/gc/ -run 'TestDeepCopyAgent\|TestAgentFieldSync'` PASS. |
| 4 | No high-severity review findings open | PASS | Zero blockers. Migration URL pinned to `docs/packv2/migration.mdx`, which is present in this branch and covered by `TestMigrationGuideDocPathExists`. |
| 5 | Final branch is clean | PASS | `git status` clean (untracked `.gitkeep` only). |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main HEAD` writes merge tree without conflicts. Five commits ahead of origin/main (3 from ga-tpfc.1 parent branch + 2 own). |

## Coordination

- **Stacking dependency.** This branch is based on `builder/ga-mol-bq54` (ga-tpfc.1, PR #1583). The two PRs share the `formatDuplicateAgentError` helper — ga-tpfc.1 introduces it, ga-9ogb.1 extends it with the (V1Inline, V2Convention) migration variant. The diff against `main` here therefore includes ga-tpfc.1's 3 commits until #1583 merges; afterwards, the diff collapses to just the 2 ga-9ogb.1 commits.
- **Merge order.** PR #1583 (ga-tpfc.1) must merge first. Reviewer-1's mail explicitly stated "Land ga-tpfc.1 first (parent)".
