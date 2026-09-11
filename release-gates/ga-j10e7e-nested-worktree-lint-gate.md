# Release Gate: nested worktree lint dirwalk detection

Gate date: 2026-07-03

Deploy bead: ga-j10e7e
Review bead: ga-sd6q36
Source bead: ga-dt41jn
Candidate branch: builder/ga-sd6q36-detect-nested-worktrees-structurally
Reviewed commit: aeb8197e168bf8444d90c016d5e2294e59f96e0b
Candidate tip before this gate: aeb8197e168bf8444d90c016d5e2294e59f96e0b
Base checked: origin/main @ 1dfce8f962d31d7df683f91dd003f20429be2b5b

Note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in
this checkout. This gate uses the deployer release criteria from the active
role prompt plus the repository testing guidance in `TESTING.md`, matching
prior gates in this repository.

## Summary

This release teaches the repository lint dirwalk tests to recognize linked
git worktrees by structure rather than by directory name. Nested worktrees have
a `.git` file instead of a `.git` directory, so the lint walks can now skip
bead-slug-named worktree roots without requiring those directories to be named
`worktrees` or `worktree-*`.

The final release diff is one feature theme:

| Path | Change |
|---|---|
| `internal/testenv/lint_test.go` | Skips nested linked worktree roots while preserving the current worktree root scan with `path != root`. |
| `test/docsync/docsync_test.go` | Skips nested linked worktree roots when checking top-level doc directory coverage. |

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-sd6q36` records `REVIEWER VERDICT (round 2): PASS` for `builder/ga-sd6q36-detect-nested-worktrees-structurally` at `aeb8197e1`. |
| 2 | Acceptance criteria met | PASS | The branch contains the structural `.git` file check requested by `ga-dt41jn` and applies it to both failing repo lint dirwalks. `git diff --name-only origin/main...HEAD` is limited to `internal/testenv/lint_test.go` and `test/docsync/docsync_test.go`. |
| 3 | Tests pass | PASS | `gofmt -l` on changed files produced no output. `git diff --check origin/main...HEAD` produced no output. Focused regression tests passed. `go vet ./internal/testenv/... ./test/docsync/...` passed. `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel` passed. |
| 4 | No high-severity review findings open | PASS | The round-1 deployability finding was remediated by cherry-picking the reviewed diff onto the clean pushed branch; round-2 review records no outstanding blockers. Unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## HEAD (no branch)` with no worktree changes. This gate file is the only deployer-authored change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `67db99ee14c4b05adf2dd3a95843584e589fb84c`; no merge conflicts with current `origin/main`. |
| 7 | Single feature theme | PASS | The commit set is confined to structural nested-worktree skipping in repo lint/doc coverage tests. No production code, docs, generated artifacts, or unrelated release-gate files are included in the reviewed diff. |

## Acceptance Checklist

- [x] Gate evaluated the reviewed pushed branch, not the original contaminated branch from round 1.
- [x] Release diff is limited to nested linked worktree detection in the two lint/doc dirwalk tests.
- [x] The current worktree root remains scanned because both walks keep the `path != root` guard.
- [x] Candidate branch has no untracked worktree files or unrelated release-gate artifacts.
- [x] Candidate branch merges cleanly with current `origin/main`.
- [x] Required focused tests, build, vet, and fast unit baseline passed.
- [x] Deployer will open a PR and route merge authority to mayor/mpr, not merge directly.

## Test Log

```text
gofmt -l internal/testenv/lint_test.go test/docsync/docsync_test.go
# no output

git diff --check origin/main...HEAD
# no output

git merge-tree --write-tree origin/main HEAD
67db99ee14c4b05adf2dd3a95843584e589fb84c

go test ./internal/testenv/... ./test/docsync/... -run "TestRequiresDedicatedTestenvImportFile|TestDocDirCoverage|TestNoLeakVectorReadsAtPackageInit" -count=1
ok  	github.com/gastownhall/gascity/internal/testenv	0.371s
ok  	github.com/gastownhall/gascity/test/docsync	0.021s

go vet ./internal/testenv/... ./test/docsync/...
# no output

go build ./...
# no output

go vet ./...
# no output

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-core] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-2-of-6] ok
All fast jobs passed
```
