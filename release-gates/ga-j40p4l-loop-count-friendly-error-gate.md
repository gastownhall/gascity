# Release Gate: Loop Count Friendly Error

Decision: PASS

## Scope

- Deploy bead: ga-j40p4l
- Source review bead: ga-acfcqc
- Reviewed commit: 0a2b1e7edac3c1c9bf8d716652965d5d10266bfb
- Actual source branch: builder/ga-sdv68f-loop-count-friendly-error
- Release branch: release/ga-j40p4l-loop-count-friendly-error-clean
- Base: origin/main 4c3b612b7fc0cbfff01ae527a9dcd4a1e9ee5741
- Shape: single-bead PR

The deploy bead listed the branch as `main`, but the reviewed commit is present
on `origin/builder/ga-sdv68f-loop-count-friendly-error` and not on
`origin/main`. This gate uses a clean release branch cut from the reviewed
commit so the PR has a valid feature head and does not depend on a dirty local
builder worktree.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-acfcqc is closed with `Review Verdict: PASS` for commit 0a2b1e7edac3c1c9bf8d716652965d5d10266bfb. |
| 2 | Acceptance criteria met | PASS | Branch delta adds a pre-check in `internal/formula/types.go` so string-valued `loop.count` fails with a friendly message that points users to `range = "1..{n}"` plus `var = "n"`. `TestCompile_LoopCountStringParseError` covers the parse error. `docs/tutorials/05-formulas.md` documents that `count` accepts only integer literals and points variable-driven loops to `range`. |
| 3 | Tests pass | PASS | `make check-docs`, `go test ./internal/formula -run TestCompile_LoopCountStringParseError`, `go test ./internal/formula`, `make test-fast-parallel`, `go vet ./...`, and `go build -o /tmp/gc-ga-j40p4l ./cmd/gc` all passed. |
| 4 | No high-severity review findings open | PASS | Review findings are PASS/LOW only; no unresolved HIGH or CRITICAL findings appear in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | The gate ran in a dedicated clean worktree on `release/ga-j40p4l-loop-count-friendly-error-clean`; `git status --short --branch` was clean before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed on the reviewed commit; the release branch is a straight descendant of current `origin/main`. |
| 7 | Single feature theme | PASS | Commit set is one formula parser UX fix, one focused regression test, and one tutorial note for the same `loop.count` behavior. |

## Commands Run

```text
make check-docs
go test ./internal/formula -run TestCompile_LoopCountStringParseError
go test ./internal/formula
make test-fast-parallel
go vet ./...
go build -o /tmp/gc-ga-j40p4l ./cmd/gc
```
