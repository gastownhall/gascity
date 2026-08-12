# Release Gate: NativeDoltStore Ready status filter

Bead: `ga-nxmzl5`
Implementation bead: `ga-3mv5d3`
Branch: `builder/ga-3mv5d3-ready-status-filter`
PR: https://github.com/gastownhall/gascity/pull/4347
Reviewed commit: `2684ac560c730bf2e89092e669c31881b854d0c5`
Base: `origin/main` at `4fda5a28445f42d6e789fc7f5751645ac4fecd19`

The prompted `docs/PROJECT_MANIFEST.md` path is not present in this Gas City
checkout. No `PROJECT_MANIFEST.md` or `SOFTWARE_FACTORY_MANIFEST.md` was found
with `rg --files -g '*MANIFEST*.md' -g '!ga-*'`, so this gate uses the deployer
release criteria from the role prompt and the repository testing guidance in
`TESTING.md`.

## Diff Scope

`git diff --name-status origin/main...HEAD`:

```text
M	internal/beads/native_dolt_store.go
M	internal/beads/native_dolt_store_test.go
```

This is one release unit: `NativeDoltStore.Ready()` now queries only upstream
statuses that can legitimately become dispatch candidates, and the matching
regression test proves blocked/pinned/hooked/review/testing statuses do not
surface as ready work after bd-status normalization.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-nxmzl5` records `Reviewed-by: gascity/reviewer, verdict PASS`; the implementation bead `ga-3mv5d3` is closed with the fix summary and verification notes. |
| 2 | Acceptance criteria met | PASS | The regression test `TestNativeDoltStoreReadyOnlyIncludesOpenAndDeferredUpstreamStatuses` covers open, blocked, deferred, pinned, hooked, review, and testing upstream statuses. `nativeDoltOpenReadyStatuses` now contains only `beadslib.StatusOpen` and `beadslib.StatusDeferred`. The diff is limited to `internal/beads/native_dolt_store.go` and `internal/beads/native_dolt_store_test.go`, so the public `Bead.Status` wire enum, OpenAPI schema, and dashboard generated types are untouched. The implementation bead notes document the status-category investigation. |
| 3 | Tests pass | PASS | `go test ./internal/beads -run TestNativeDoltStoreReadyOnlyIncludesOpenAndDeferredUpstreamStatuses -count=1` passed. `go test ./internal/beads/...` passed. `gofmt -l internal/beads/native_dolt_store.go internal/beads/native_dolt_store_test.go` returned no files. `go vet ./...` passed. `make test-fast-parallel` passed all 8 fast jobs. `gh pr checks 4347` was green at reviewed commit `2684ac560c730bf2e89092e669c31881b854d0c5` before adding this gate file. |
| 4 | No high-severity review findings open | PASS | The deploy bead records reviewer PASS and no blocker/HIGH findings. PR #4347 has no comments or reviews from external contributors. |
| 5 | Final branch is clean | PASS | Detached gate worktree at `/var/tmp/gc-deployer-ga-nxmzl5-pass-20260716-4eYigT` started clean at `origin/builder/ga-3mv5d3-ready-status-filter`; `git status --short --branch` returned only `## HEAD (no branch)` before this gate file was added. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main origin/builder/ga-3mv5d3-ready-status-filter` passed. `git merge-base origin/main origin/builder/ga-3mv5d3-ready-status-filter` returned `4fda5a28445f42d6e789fc7f5751645ac4fecd19`. `git merge-tree --write-tree origin/main origin/builder/ga-3mv5d3-ready-status-filter` succeeded with tree `34b5ba7dbc8f8b7efe7df1a5f4496464634b15fc`. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: native Dolt-backed bead readiness filtering for non-dispatchable upstream statuses. |

Gate result: PASS.
