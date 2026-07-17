# Release Gate: ga-9ajrc0 docgen tracked directory scan

Evaluated: 2026-07-17T05:18:42Z

- Deploy bead: `ga-9ajrc0`
- Source bead: `ga-vfurlv`
- Review bead: `ga-bla86o`
- Branch: `builder/ga-vfurlv-docgen-tracked-dir-scan`
- Candidate commit: `dcaa53067d71440dd677409996ce7cec81e1e084`
- Base: `origin/main` at `d5cb9125fc9a20a4a720037aec387d76cca2cc60`
- Release criteria source: deployer gate prompt. `docs/PROJECT_MANIFEST.md` is not present at this commit.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of the candidate (`git merge-base --is-ancestor origin/main HEAD` rc 0). `git rev-list --left-right --count origin/main...HEAD` reported `0 3`. |
| 1 | Review PASS present | PASS | `ga-bla86o` is closed with close reason `pass`; notes contain `Reviewer verdict: PASS` and no blocking findings. Deploy bead `ga-9ajrc0` records reviewer PASS evidence. |
| 2 | Acceptance criteria met | PASS | The source bug `ga-vfurlv` required bounding `internal/docgen` schema comment scanning so stray root directories cannot multiply parser work. `internal/docgen/schema.go` now scopes visible top-level directories through `gitTrackedTopLevelDirs` using `git ls-tree -d --name-only HEAD`, with fallback to the previous walk-all behavior outside a usable git repo. `internal/docgen/schema_test.go` adds `TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs`, covering tracked and untracked top-level directories. Resource census ledger changes are mirrored in `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and `TESTING.md`. |
| 3 | Tests pass | PASS | `gofmt -l internal/docgen/schema.go internal/docgen/schema_test.go internal/testpolicy/resourcecensus/census.go` produced no output. `go vet ./internal/docgen/... ./internal/testpolicy/resourcecensus/...` passed. `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...` passed (`internal/docgen` 21.058s, `internal/testpolicy/resourcecensus` 1.591s). `make test-fast-parallel` passed all eight fast jobs. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-bla86o` say "No findings requiring changes." No unresolved HIGH findings were found in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short` in the scratch worktree was empty. This gate file is committed as the branch tip before push. |
| 7 | Single feature theme | PASS | The commit set is one release theme: bound docgen's schema comment scan to tracked top-level directories, plus the required resource-census ledger mirror updates for the new git-backed test fixture. Diff scope is `internal/docgen`, `internal/testpolicy/resourcecensus`, `test/test-resources.toml`, and `TESTING.md`. |

## Commit Set

| Commit | Summary |
|--------|---------|
| `ca1a6ce6a` | `fix(docgen): bound schema doc-gen walk to git-tracked top-level dirs` |
| `3c15e92c8` | `test(resourcecensus): bump subprocess ledger for gitTrackedTopLevelDirs` |
| `dcaa53067` | `fix(resourcecensus): rebase ledger bump onto origin/main post-#4211` |

## Test Output Summary

- `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...`: PASS
- `make test-fast-parallel`: PASS, all fast jobs passed
- `go vet ./...`: PASS
