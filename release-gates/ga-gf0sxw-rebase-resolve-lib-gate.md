# Release Gate: deployer bounded self-rebase helper

- Bead: `ga-gf0sxw`
- Source review bead: `ga-ohb1ru`
- Source branch: `origin/builder/ga-yvrg05.1-rebase-resolve-lib-port`
- Final deploy branch: `deploy/ga-gf0sxw-rebase-resolve-lib-port-20260715083438`
- Original gate base: `origin/main` at `3cb8d2d4bf17ac007cd56e48bafa79d4acee5e96`
- Rebased candidate head before gate file: `150afca8a983b1f81aed026d1960e95caae96da2`
- Current deploy follow-up bead: `ga-dfhdvu`
- Current base: `origin/main` at `081efc705c661905d7bf095052f30af6c7354e8e`
- Current PR head before this refresh: `5f3ee5e25f9f25ac46bcc49d9d639f3441a0310b`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate applies the active deployer prompt release criteria plus the repo guidance in `TESTING.md`.

## Current Refresh

PASS on 2026-07-15 for deploy follow-up `ga-dfhdvu`.

Evidence from `/var/tmp/gascity-deployer-ga-dfhdvu-gate-20260715051447`:

- `git rev-parse HEAD`: `5f3ee5e25f9f25ac46bcc49d9d639f3441a0310b`
- `git rev-parse origin/main`: `081efc705c661905d7bf095052f30af6c7354e8e`
- `git rev-list --left-right --count origin/main...HEAD`: `0 3`
- `git merge-tree --write-tree origin/main HEAD`: `fc42b2f92581c2c9ab4c83670d654179fd7e49cc`
- `make test-fast-parallel`: PASS (`All fast jobs passed`)
- `bash scripts/test-rebase-resolve.sh`: PASS (`pass=22 fail=0`)
- `go test ./scripts/... -run RebaseResolve -v`: PASS
- `go test ./internal/testpolicy/resourcecensus/...`: PASS
- `shellcheck scripts/rebase-resolve-lib.sh scripts/test-rebase-resolve.sh`: clean
- `gofmt -l scripts/rebase_resolve_lib_test.go`: clean
- `go vet ./...`: clean
- `go build ./...`: clean

## Scope

This PR ports the deployer's bounded self-rebase helper into the Gas City repo so deployer gate criterion 6 can self-heal provably trivial branch staleness. The change adds:

- `scripts/rebase-resolve-lib.sh`
- `scripts/test-rebase-resolve.sh`
- `scripts/rebase_resolve_lib_test.go`
- resource-census ledger updates in `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and `TESTING.md`

## Criterion 6: Branch Diverges Cleanly From Main

PASS.

Evidence:

- Original reviewed source branch was stale against current `origin/main` (`origin/main...origin/builder/ga-yvrg05.1-rebase-resolve-lib-port` was `2 2`) but conflict-free by `git merge-tree --write-tree origin/main origin/builder/ga-yvrg05.1-rebase-resolve-lib-port`.
- The original builder worktree was not used because it had unrelated untracked scaffold residue. A clean deployer-owned branch was cut from the reviewed source branch.
- `scripts/rebase-resolve-lib.sh` was sourced from the candidate branch and `attempt_bounded_self_rebase deploy/ga-gf0sxw-rebase-resolve-lib-port-20260715083438 main` returned `0`.
- Self-rebase audit: `BEFORE_SHA=d9c61bbb68458c1908961198fba0ae13500bb2dd`, `AFTER_SHA=150afca8a983b1f81aed026d1960e95caae96da2`.
- The helper pushed with `--force-with-lease`; the push returned 0.
- `git rev-list --left-right --count HEAD...origin/main` after rebase: `2 0`.
- `git merge-tree --write-tree origin/main HEAD` after rebase returned tree `4ba0e24723d58c10e96221fbd7367b4d92d31175` with exit 0.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-ohb1ru` is closed with close reason `pass` and notes contain `Reviewer verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | Library port is byte-identical to `/home/jaword/projects/gc-management/packs/actual/deployer/scripts/rebase-resolve-lib.sh` (`cmp -s` exit 0). Shell test diff against the source pack is exactly the expected two-line path-layout adjustment from `PACK_DIR` + `LIB` to sibling `LIB="$TEST_DIR/rebase-resolve-lib.sh"`. The caller rationale was independently reviewed in `ga-ohb1ru`: deployer formula sources `scripts/rebase-resolve-lib.sh` from each target rig checkout. |
| 3 | Tests pass | PASS | `attempt_bounded_self_rebase` push triggered `.githooks/pre-push`; for this new Go-changing branch the hook runs `make test-fast-parallel`, and the push returned 0. Additional explicit checks: `bash scripts/test-rebase-resolve.sh` passed `pass=22 fail=0` with a valid `TMPDIR`; `go test ./scripts/... -run RebaseResolve -v` passed; `go test ./internal/testpolicy/resourcecensus/...` passed; `shellcheck scripts/rebase-resolve-lib.sh scripts/test-rebase-resolve.sh` passed; `gofmt -l scripts/rebase_resolve_lib_test.go` returned empty; `go vet ./...` passed; `go build ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list exactly two LOW severity, non-blocking comment-only findings about stale path comments. No HIGH or CRITICAL findings are present in `ga-ohb1ru`. |
| 5 | Final branch is clean | PASS | `git status --short --branch` before writing this gate file showed no working-tree changes. After this gate file is committed, deployer will re-check a clean tree before push/PR. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first; see dedicated section above. |
| 7 | Single feature theme | PASS | Commit set touches one feature theme: deployer bounded self-rebase helper and its direct tests/resource-census bookkeeping. No independent user-facing feature is bundled. |

## Test Log Summary

- `bash scripts/test-rebase-resolve.sh`: `pass=22 fail=0`
- `go test ./scripts/... -run RebaseResolve -v`: `ok github.com/gastownhall/gascity/scripts`
- `go test ./internal/testpolicy/resourcecensus/...`: `ok github.com/gastownhall/gascity/internal/testpolicy/resourcecensus`
- `shellcheck scripts/rebase-resolve-lib.sh scripts/test-rebase-resolve.sh`: clean
- `gofmt -l scripts/rebase_resolve_lib_test.go`: no output
- `go vet ./...`: clean
- `go build ./...`: clean
