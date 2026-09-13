# Release Gate: ga-7kav6u

Feature: shard `cmd/gc` in `make test-cover` and restore 10m timeout
PR: https://github.com/gastownhall/gascity/pull/3553
Branch: builder/ga-92tpq3
Reviewed commit: 0ee4951ebdb21a9dc5fad70be667f552e01913dc
Gate worktree: /tmp/gascity-deploy-ga-7kav6u-1781628712
Gate date: 2026-06-16

## Gate Inputs

- Deploy bead: ga-7kav6u
- Source review bead: ga-qux1ih
- Source implementation bead: ga-92tpq3
- Review verdict: PASS in ga-qux1ih notes.
- Manifest note: docs/PROJECT_MANIFEST.md is not present in this checkout, so this gate uses the deployer release criteria table from the agent instructions.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-qux1ih is closed with close reason `pass`; notes contain `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | Commit 0ee4951ebdb21a9dc5fad70be667f552e01913dc changes only `Makefile`. It adds `CMD_GC_COVER_TOTAL ?= 6`, excludes `cmd/gc` from the non-sharded coverage package list, runs `cmd/gc` coverage through `scripts/test-go-test-shard`, restores a 10m coverage timeout, and merges profiles with `scripts/merge-coverprofiles`. |
| 3 | Tests pass | PASS | `go build ./cmd/gc` passed. `go vet ./...` passed. `make test-fast-parallel` passed: all fast jobs passed. `make test-cover` passed and merged `coverage.noncmdgc.txt` plus six `coverage.cmdgc.*.txt` profiles into `coverage.txt`; total coverage was 70.9%. `cmd/gc` coverage shard runtimes were 48.628s, 50.423s, 54.604s, 66.928s, 74.080s, and 70.909s. |
| 4 | No high-severity review findings open | PASS | ga-qux1ih review notes contain one INFO finding (`UNIT_COVER_PKGS` is now a dead variable) and no HIGH findings. |
| 5 | Final branch is clean | PASS | Clean gate worktree before gate file; generated coverage artifacts removed after `make test-cover`; final cleanliness rechecked after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | Rebased cleanly onto main (2026-06-16). Current `origin/main` at e6c2df1fa3f0a66ed9c09b4edc4b2aa8563c6ea7; merge-base e6c2df1fa3f0a66ed9c09b4edc4b2aa8563c6ea7. |
| 7 | Single feature theme | PASS | Single Makefile-only test-infrastructure change for coverage execution; no production code, config, API, schema, or runtime behavior changes. |

## Diff Scope

```text
M	Makefile
```

## Test Commands

```text
go build ./cmd/gc
go vet ./...
make test-fast-parallel
make test-cover
go tool cover -func=coverage.txt
```
