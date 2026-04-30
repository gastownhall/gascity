# Release gate — ga-2k9v (mol-dog-stale-db formula + nightly cron)

Deployer: gascity/deployer
Date: 2026-04-30
Bead: ga-2k9v (review of ga-evjp)
Source commit: `e49a912f` on `gc-builder-1-01561d4fb9ea`
Cherry-picked SHA on release branch: `f70a4abe`
Branch: `release/ga-2k9v-mol-dog-stale-db-cron` (off origin/main `7b6c5406`)

## Verdict: PASS

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | claude reviewer verdict `PASS` recorded in bead notes (commit `e49a912f`); gemini second-pass disabled per current policy. |
| 2 | Acceptance criteria met | PASS | Spec compliance against AD-04 §4.1 / Wireframe 5: stage events `mol-dog-stale-db.{scan,escalate,drop,purge,reap,done}`; two-phase dry-run → threshold gate → optional `--force`; `gc dolt-cleanup --json` envelope `gc.dolt.cleanup.v1` paths align; cron `0 3 * * *`; `__gc_probe` mention preserved (formula Safety section). All confirmed by reviewer. |
| 3 | Tests pass | PASS | On assembled branch: `go vet ./...` clean; `go build ./...` clean; `go test -short ./internal/orders/... ./internal/formula/... ./internal/molecule/...` all PASS; `go test -short -run 'TestEmbed\|TestBuiltin\|TestMaterializeBuiltinPacks' ./cmd/gc/` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded `request-changes (none)`. Four informational items, none blocking: `[vars]` block in order is non-functional but defaults match (doc drift, not behavior); cron syntax validated; bash-state persistence consistent with sibling dog formulas; mail-send call form supported. |
| 5 | Final branch is clean | PASS | `git status` reports working tree clean after gate commit. |
| 6 | Branch diverges cleanly from main | PASS | Cherry-pick of `e49a912f` onto fresh branch off `origin/main` applied without conflict. Commit only touches `examples/dolt/formulas/mol-dog-stale-db.toml` and `examples/dolt/orders/mol-dog-stale-db.toml`; both files identical between commit's parent and origin/main. |

## Notes

The builder authored on shared branch `gc-builder-1-01561d4fb9ea` (stack of
20+ unrelated commits across multiple beads). The deploy unit is the bead, so
the release branch is a fresh cut off `origin/main` containing only the
ga-evjp commit. PR is single-commit by design.

## Pre-existing issues (not introduced)

- `TestCmdOrderHistoryUsesProviderAwareCityStore` and two siblings fail on
  baseline (rig-registration test isolation, unrelated). Confirmed by builder
  and reviewer. Not part of this PR's surface.
