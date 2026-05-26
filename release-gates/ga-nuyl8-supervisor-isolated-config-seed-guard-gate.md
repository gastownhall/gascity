# Release Gate: ga-nuyl8 supervisor isolated config seed guard

Generated: 2026-05-26T14:04:53Z

Bead: ga-nuyl8 - Review: supervisor isolated config seed guard
Feature branch: builder/ga-zjxgr
Feature HEAD: be1bc75680b154ddb6f794429011946d53880f02
Base: origin/main

Note: docs/PROJECT_MANIFEST.md is not present in this checkout, so this gate uses the six deployer release criteria from the active prompt.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Bead notes contain `Reviewer Verdict: PASS` from `gascity/reviewer` for branch `builder/ga-zjxgr` at `be1bc7568`. |
| 2 | Acceptance criteria met | PASS | `shouldSeedIsolatedSupervisorConfig` now requires a test binary or `GC_ISOLATED=1`, non-empty `GC_HOME`, and `path == ConfigPath()`. Tests cover non-test binary without `GC_ISOLATED`, non-test binary with `GC_ISOLATED=1`, canonical default under symlinked HOME, and isolated test loading. |
| 3 | Tests pass | PASS | `go test ./internal/supervisor -count=1` passed; `make test-fast-parallel` passed all fast jobs; `go vet ./...` completed cleanly. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blocking findings and no HIGH findings. Only non-blocking observation is about future parallelization of tests that mutate `os.Args`. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before adding this checklist; deployer will re-check after committing the checklist before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main HEAD` produced a clean tree object (`a64fbcf0b5425e53250ba2e252860f9e039d2241`) with no conflict records. |

## Acceptance Evidence

- Production `gc` binaries no longer seed private supervisor configs merely because `GC_HOME` differs from the ambient user home.
- Test binaries keep the existing isolated-seeding behavior, preserving the test isolation guard.
- Non-test CI and development sandboxes can explicitly opt in with `GC_ISOLATED=1`.
- The seeded config path remains constrained to `ConfigPath()`.

## Commands Run

```text
go test ./internal/supervisor -count=1
make test-fast-parallel
go vet ./...
git merge-tree origin/main HEAD
git status --short
```
