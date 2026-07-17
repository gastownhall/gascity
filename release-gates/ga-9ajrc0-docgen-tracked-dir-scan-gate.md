# Release Gate: ga-9ajrc0 docgen tracked directory scan

Evaluated: 2026-07-17T15:14:50Z

- Deploy bead: `ga-9ajrc0`
- Source bead: `ga-vfurlv`
- Review bead: `ga-bla86o`
- Branch: `builder/ga-vfurlv-docgen-tracked-dir-scan`
- Candidate commit: `df66905926b5168866e668ddd51282054a4a9376`
- Base: `origin/main` at `5f9f6cee2aafaf68113381f398c80360b82a4594`
- Release criteria source: deployer gate prompt. `docs/PROJECT_MANIFEST.md` is not present at this commit.
- Rebase note (bead `ga-zv2oi4`): this supersedes the prior evaluation recorded at this same path against base `d5cb9125fc9a20a4a720037aec387d76cca2cc60`. The branch was rebased onto current `origin/main` to resolve PR #4377's needs-rebase state. The former `fix(resourcecensus): rebase ledger bump onto origin/main post-#4211` commit (`dcaa53067d71440dd677409996ce7cec81e1e084`) became an empty commit under the new base — its `cmd/gc`+`untagged`/`environment` ledger values now match `origin/main` exactly — and was dropped by the rebase sequencer. The three census-ledger mirror files (`internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, `TESTING.md`) had conflicting `environment`-resource rows during the rebase, resolved by keeping `origin/main`'s newer baseline/reported values, since this branch's own changes make no `cmd/gc` environment calls; the branch's own `subprocess`-resource bumps carried forward unchanged.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of the candidate (`git merge-base --is-ancestor origin/main df6690592` rc 0). `git rev-list --left-right --count origin/main...df6690592` reported `0 2`. |
| 1 | Review PASS present | PASS | `ga-bla86o` is closed with close reason `pass`; notes contain `Reviewer verdict: PASS` and no blocking findings. Deploy bead `ga-9ajrc0` records reviewer PASS evidence. |
| 2 | Acceptance criteria met | PASS | The source bug `ga-vfurlv` required bounding `internal/docgen` schema comment scanning so stray root directories cannot multiply parser work. `internal/docgen/schema.go` now scopes visible top-level directories through `gitTrackedTopLevelDirs` using `git ls-tree -d --name-only HEAD`, with fallback to the previous walk-all behavior outside a usable git repo. `internal/docgen/schema_test.go` adds `TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs`, covering tracked and untracked top-level directories. Resource census ledger changes are mirrored in `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and `TESTING.md`. |
| 3 | Tests pass | PASS | `gofmt -l internal/docgen/schema.go internal/docgen/schema_test.go internal/testpolicy/resourcecensus/census.go` produced no output. `go build ./...` passed. `go vet ./...` passed. `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...` passed. `make test-fast-parallel` ran 8 fast jobs: 6 passed; 2 `unit-cmd-gc` shard failures were root-caused as pre-existing and unrelated to this diff (neither touches `internal/docgen`, `internal/testpolicy/resourcecensus`, or any file in this commit set) — see Test Output Summary below for the two-test breakdown and evidence. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-bla86o` say "No findings requiring changes." No unresolved HIGH findings were found in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short` in the scratch worktree was empty. This gate file is committed as the branch tip before push. |
| 7 | Single feature theme | PASS | The commit set is one release theme: bound docgen's schema comment scan to tracked top-level directories, plus the required resource-census ledger mirror updates for the new git-backed test fixture. Diff scope is `internal/docgen`, `internal/testpolicy/resourcecensus`, `test/test-resources.toml`, and `TESTING.md`. |

## Commit Set

| Commit | Summary |
|--------|---------|
| `fb8a69489` | `fix(docgen): bound schema doc-gen walk to git-tracked top-level dirs` |
| `df6690592` | `test(resourcecensus): bump subprocess ledger for gitTrackedTopLevelDirs` |

The former `dcaa53067` (`fix(resourcecensus): rebase ledger bump onto origin/main post-#4211`) was dropped by the rebase onto the new base — see rebase note above.

## Test Output Summary

- `go build ./...`: PASS
- `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...`: PASS (`internal/docgen` 41.018s including `TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs`; `internal/testpolicy/resourcecensus` 2.843s including `TestRepositoryLedgerMatchesCensusAndDocumentation`)
- `go vet ./...`: PASS
- `make test-fast-parallel`: 6/8 fast jobs passed (`fsys-darwin-compile`, `unit-core`, `unit-cmd-gc-1-of-6`, `unit-cmd-gc-2-of-6`, `unit-cmd-gc-3-of-6`, `unit-cmd-gc-4-of-6`). 2 jobs failed, both root-caused as pre-existing and independent of this diff:
  - `unit-cmd-gc-5-of-6`: `TestProductMetricsServiceChildEnvSupervisorStart` fails with `HOME override "/home/jaword/james-claude" differs from the user home "/home/jaword"; platform supervisor requires the real HOME` — a sandbox `HOME`-override guard rail. Reproduced identically running the same test against `origin/main` (`5f9f6cee2`) in an isolated worktree outside this branch, confirming it predates and is independent of this candidate.
  - `unit-cmd-gc-6-of-6`: `TestProductMetricsLifecycleCommandPathMatrixAttemptsOnce/jsonl_failure` expects `gc events --json` to exit 1 with no ambient city discoverable, but exits 0 against real event data. `cmd/gc/metrics_lifecycle_test.go` is untouched by this commit set (byte-identical to `origin/main`) and this subtest does not `os.Chdir` into an isolated fixture city the way sibling tests in the same file do, so it is sensitive to ambient city state reachable from the test binary's working directory. It passes when the same package is checked out outside a `.gc/worktrees/` tree (verified in the same isolated `origin/main` worktree above) and only fails when the checkout is nested inside this shared fleet's live `.gc/` tree, which is where this builder session's worktree for bead `ga-zv2oi4` is required to live. Not a regression introduced by rebasing or by this PR's content.
