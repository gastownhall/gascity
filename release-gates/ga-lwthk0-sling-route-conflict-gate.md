# Release Gate: `gc sling` conflicting route refusal

- Deploy bead: `ga-lwthk0`
- Source bead: `ga-3afq3j`
- Reviewed commit: `ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596`
- Deploy branch: `deploy/ga-lwthk0-gate`
- Evaluated: 2026-07-25
- Gate source: deployer prompt release-gate table. `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Summary

PASS. `gc sling` now hard-refuses to overwrite an existing conflicting `gc.routed_to` value unless `--force` is used, and `--dry-run` renders the same routing conflict instead of previewing a misleading successful route.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git fetch origin main`; `git merge-tree --write-tree origin/main ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596` returned tree `551f1cefefbd287ba1c3f8461827c4f3f08e6419`; `git diff --check origin/main...ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596` produced no output. |
| 1 | Review PASS present | PASS | Deploy bead `ga-lwthk0` records reviewer PASS for source bead `ga-3afq3j`; source notes contain `Review verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | Commit set is the expected red/green pair: `93aebd64a` and `ba4b3d1b7`. Diff covers the sling conflict result, attachment check, preflight propagation, CLI dry-run rendering, and regression tests. `--force`, custom SlingQuery, and assignee-only cases remain outside the hard-conflict path as specified. |
| 3 | Tests pass | PASS | Focused sling regression run passed for `TestRoutedStateWarnings`, `TestCheckBeadStateWithOptions_RoutingConflict`, `TestDoSlingRefusesConflictingRoute`, `TestDoSlingForceOverridesConflictingRoute`, and `TestDryRunRoutingConflict`. `go test ./internal/sling -count=1` passed. `go build ./...` passed. `go vet ./...` passed. Initial `make test-fast-parallel` failed on unrelated `internal/productmetrics` `TestConcurrentGreaterEpochResumeAndDisableHaveOneCASWinner`; isolated retry of that test passed, and bounded full-suite rerun of `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0 | rg -i -- 'ga-lwthk0|ga-3afq3j|HIGH|request-changes|security'` returned only sling helper beads `ga-9xqgt7`, `ga-epfzr0`, and `ga-ykkkyq`; no open HIGH/request-changes finding was found. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` returned only `## deploy/ga-lwthk0-gate`. The gate file is committed as the final branch tip before push. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: `gc sling` route-conflict handling and its tests. The sibling multi-line inline-text fix is intentionally separate. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596
git diff --check origin/main...ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596
git log --oneline --reverse 80e5166473033b9f2807dad048ddcb70dfc3b86e..ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596
git diff --stat 80e5166473033b9f2807dad048ddcb70dfc3b86e..ba4b3d1b7c90b8d14adecf3c666390bcb9c5d596
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./internal/sling ./cmd/gc -run 'TestRoutedStateWarnings|TestCheckBeadStateWithOptions_RoutingConflict|TestDoSlingRefusesConflictingRoute|TestDoSlingForceOverridesConflictingRoute|TestDryRunRoutingConflict' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./internal/sling -count=1
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go build ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go vet ./...
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE go test ./internal/productmetrics -run '^TestConcurrentGreaterEpochResumeAndDisableHaveOneCASWinner$' -count=1 -v
TMPDIR=/var/tmp env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE make test-fast-parallel
bd list --status open --limit 0 | rg -i -- 'ga-lwthk0|ga-3afq3j|HIGH|request-changes|security'
```
