# Release Gate: ga-9ajrc0 docgen tracked directory scan

Evaluated: 2026-07-18T15:36:08Z

- Deploy bead: `ga-9ajrc0`
- Source bead: `ga-vfurlv`
- Review bead: `ga-bla86o`
- Branch: `builder/ga-vfurlv-docgen-tracked-dir-scan`
- Candidate commit: `eaf0174f97097159ea8c54babbcacadf6728b2f8`
- Base: `origin/main` at `e9f266c8d1f652a88c15a5dc185c04e58bb2a5dd`
- Release criteria source: deployer gate prompt. `docs/PROJECT_MANIFEST.md` is not present at this commit.
- Rebase note (bead `ga-zv2oi4`): this supersedes the prior evaluation recorded at this same path against base `d5cb9125fc9a20a4a720037aec387d76cca2cc60`. The branch was rebased onto current `origin/main` to resolve PR #4377's needs-rebase state. The former `fix(resourcecensus): rebase ledger bump onto origin/main post-#4211` commit (`dcaa53067d71440dd677409996ce7cec81e1e084`) became an empty commit under the new base — its `cmd/gc`+`untagged`/`environment` ledger values now match `origin/main` exactly — and was dropped by the rebase sequencer. The three census-ledger mirror files (`internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, `TESTING.md`) had conflicting `environment`-resource rows during the rebase, resolved by keeping `origin/main`'s newer baseline/reported values, since this branch's own changes make no `cmd/gc` environment calls; the branch's own `subprocess`-resource bumps carried forward unchanged.
- Rebase note (bead `ga-ugoi7u`): this supersedes the prior evaluation recorded at this same path against base `5f9f6cee2aafaf68113381f398c80360b82a4594`. The automated `hourly pr-audit` order re-flagged PR #4377 as needing rebase after `origin/main` advanced 37 more commits (to `e9f266c8d`), reintroducing conflicts. The branch was rebased again onto current `origin/main`. The former `test(resourcecensus): bump subprocess ledger for gitTrackedTopLevelDirs` commit (`df66905926b5168866e668ddd51282054a4a9376`) became an empty commit under the new base — its three `subprocess`-resource rows conflicted with `origin/main`'s own newer baseline bumps for the same rows, and resolving to `origin/main`'s (higher) values made the commit's entire diff empty against the new base — and it was dropped by the rebase sequencer, the same mechanic as the `dcaa53067`/`environment` drop from the first rebase cycle above. A new commit, `eaf0174f9`, re-derives the correct post-rebase `subprocess` baseline by running `TestRepositoryLedgerMatchesCensusAndDocumentation`'s live census scan (not hand-transcribed) and applying the exact reported drift on top of `origin/main`'s baseline: `all/subprocess` calls 532→533, files 157→158; `untagged/subprocess` source-debt row calls 404→405, files 110→111; `untagged/subprocess` small-debt row calls 402→403, files 109→110.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of the candidate (`git merge-base --is-ancestor origin/main eaf0174f9` rc 0). `git rev-list --left-right --count origin/main...eaf0174f9` reported `0 4`. |
| 1 | Review PASS present | PASS | `ga-bla86o` is closed with close reason `pass`; notes contain `Reviewer verdict: PASS` and no blocking findings. Deploy bead `ga-9ajrc0` records reviewer PASS evidence. Rebasing carries no logic changes on either cycle, so no fresh review pass was required. |
| 2 | Acceptance criteria met | PASS | Unchanged from the first evaluation: `internal/docgen/schema.go` scopes visible top-level directories through `gitTrackedTopLevelDirs` using `git ls-tree -d --name-only HEAD`, with fallback to the previous walk-all behavior outside a usable git repo. `internal/docgen/schema_test.go`'s `TestAddGoCommentsFilteredSkipsUntrackedTopLevelDirs` carried through both rebases unchanged. Resource census ledger mirrors were re-derived for the new base as described in the rebase note above. |
| 3 | Tests pass | PASS | `gofmt -l internal/docgen/schema.go internal/docgen/schema_test.go internal/testpolicy/resourcecensus/census.go` produced no output. `go build ./...` passed. `go vet ./...` passed. `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...` passed. `make test-fast-parallel` ran 8 fast jobs: 7 passed; 1 `unit-cmd-gc` shard failure was root-caused as pre-existing and unrelated to this diff — see Test Output Summary below. |
| 4 | No high-severity review findings open | PASS | Unchanged: reviewer notes for `ga-bla86o` say "No findings requiring changes." No unresolved HIGH findings in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Before writing this gate refresh, `git status --short` in the worktree was empty. This gate file is committed as the branch tip before push. |
| 7 | Single feature theme | PASS | Unchanged: one release theme (bound docgen's schema comment scan to tracked top-level directories, plus the resource-census ledger mirror updates the fixture requires). This rebase cycle's additions (`eaf0174f9` and this gate refresh) are mechanical rebase upkeep, not new theme scope. |

## Commit Set

| Commit | Summary |
|--------|---------|
| `288c2da81` | `fix(docgen): bound schema doc-gen walk to git-tracked top-level dirs` |
| `2b12a90b3` | `chore: release gate PASS for ga-9ajrc0-docgen-tracked-dir-scan` |
| `93a9c2a8d` | `chore(release-gate): refresh ga-9ajrc0 evidence after rebase onto main` |
| `eaf0174f9` | `test(resourcecensus): re-derive subprocess ledger bump on second rebase` |

The former `fb8a69489`/`df6690592` pair (first-rebase-cycle hashes for the docgen fix and its ledger bump) were superseded by this second rebase: `fb8a69489`'s content replayed cleanly as `288c2da81` (same 2-file, 124-line diff; new hash from the new parent), while `df6690592` became empty and was dropped — see rebase note above. `dcaa53067`, dropped in the first rebase cycle, remains dropped.

## Test Output Summary

- `go build ./...`: PASS
- `go test ./internal/docgen/... ./internal/testpolicy/resourcecensus/...`: PASS
- `go vet ./...`: PASS
- `make test-fast-parallel`: 7/8 fast jobs passed (`fsys-darwin-compile`, `unit-core`, `unit-cmd-gc-1-of-6`, `unit-cmd-gc-2-of-6`, `unit-cmd-gc-3-of-6`, `unit-cmd-gc-5-of-6`, `unit-cmd-gc-6-of-6`). 1 job failed, root-caused as pre-existing and independent of this diff:
  - `unit-cmd-gc-4-of-6`: `TestProductMetricsServiceChildEnvSupervisorStart` fails with `HOME override "/home/jaword/james-claude" differs from the user home "/home/jaword"; platform supervisor requires the real HOME` — the identical sandbox `HOME`-override guard rail documented in the first rebase cycle above. `cmd/gc/productmetrics_service_child_env_test.go` is byte-identical to `origin/main` (`git diff origin/main -- cmd/gc/productmetrics_service_child_env_test.go` is empty), and this branch's full diffstat vs `origin/main` touches only `internal/docgen`, `internal/testpolicy/resourcecensus`, `test/test-resources.toml`, `TESTING.md`, and this gate file — nothing on the supervisor path. Not a regression.
  - The second flake documented in the first rebase cycle, `TestProductMetricsLifecycleCommandPathMatrixAttemptsOnce/jsonl_failure`, ran this cycle (inside `unit-cmd-gc-2-of-6`) and passed, consistent with its documented sensitivity to ambient city state rather than a deterministic failure.
