# Release Gate: docgen hidden directory schema walk fix

Date: 2026-06-08
Deployer: gascity/deployer
Deploy bead: ga-5emkl7
Source bug: ga-79qhoy
Review bead: ga-onhynn
PR: https://github.com/gastownhall/gascity/pull/3255
Feature branch: builder/ga-79qhoy
Reviewed commit: 72e838e125a49dc617ba0cfbe9bfda647c550e8e
Base checked: origin/main at 544bf47df80e63abff7b30553e1407f435d6e264
Post-push base rechecked: origin/main at 4111f8fb55fa24afec413719519b385014f15310

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the deployer release criteria from the active Gas City deployer prompt.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-onhynn` is closed with `Review Verdict: PASS` for commit `72e838e125a49dc617ba0cfbe9bfda647c550e8e`; deploy bead `ga-5emkl7` records reviewer PASS evidence. |
| 2 | Acceptance criteria met | PASS | Source bug `ga-79qhoy` required excluding `.gc/` from schema comment walks. The commit replaces repo-wide `AddGoComments(".", ...)` with `addGoCommentsFiltered`, which enumerates visible top-level directories only and skips names beginning with `.`. |
| 3 | Tests pass | PASS | Focused docgen regression passed; `make test` passed with observable log `/tmp/gascity-test.jsonl.ELhVPS`; `go vet ./...` passed; `go build ./cmd/gc` passed. |
| 4 | No high-severity review findings open | PASS | Review notes list PASS for style, design, security, and test coverage. No blockers or high-severity findings are listed. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Feature worktree `builder/worktrees/ga-79qhoy` was clean before writing this gate: `git status --short --branch` reported only `## builder/ga-79qhoy...origin/builder/ga-79qhoy`. Final cleanliness is rechecked after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main builder/ga-79qhoy) origin/main builder/ga-79qhoy` completed with a merged result and no conflict markers against initial base `544bf47df80e63abff7b30553e1407f435d6e264` and post-push base `4111f8fb55fa24afec413719519b385014f15310`. `git diff --check origin/main...builder/ga-79qhoy` produced no output. |
| 7 | Single feature theme | PASS | The commit set touches only `internal/docgen/schema.go` and `internal/docgen/schema_test.go`; one subsystem and one behavior: schema generation no longer walks transient hidden runtime directories. |

## Acceptance Evidence

- `newReflector` now calls `addGoCommentsFiltered` instead of walking the
  entire module root with `r.AddGoComments("github.com/gastownhall/gascity", ".")`.
- `addGoCommentsFiltered` reads the module root, processes visible top-level
  directories, and skips hidden directories such as `.gc/`, preventing schema
  generation from entering MPR checkout directories that can disappear mid-walk.
- `TestAddGoCommentsFilteredSkipsHiddenDirs` creates an unreadable hidden
  directory and verifies the filtered comment walk succeeds.
- Existing city and pack schema tests still pass.

## Test Evidence

```text
$ go test ./internal/docgen -run 'TestAddGoCommentsFilteredSkipsHiddenDirs|TestCitySchema|TestPackSchema|TestAddGoComments' -count=1
ok  	github.com/gastownhall/gascity/internal/docgen	6.846s

$ make test
observable go test: PASS log=/tmp/gascity-test.jsonl.ELhVPS

$ go vet ./...
PASS

$ go build ./cmd/gc
PASS
```

## Commands Run

```text
gh auth status
bd show ga-5emkl7
bd show ga-onhynn
bd show ga-79qhoy
git show --stat --name-only --oneline 72e838e12
git diff --name-only origin/main...builder/ga-79qhoy
git diff --check origin/main...builder/ga-79qhoy
git merge-tree $(git merge-base origin/main builder/ga-79qhoy) origin/main builder/ga-79qhoy
go test ./internal/docgen -run 'TestAddGoCommentsFilteredSkipsHiddenDirs|TestCitySchema|TestPackSchema|TestAddGoComments' -count=1
make test
go vet ./...
go build ./cmd/gc
```

## Files Reviewed

```text
internal/docgen/schema.go
internal/docgen/schema_test.go
```
