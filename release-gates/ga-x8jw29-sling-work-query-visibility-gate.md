# Release Gate: `gc sling` work-query visibility postcondition

- Deploy bead: `ga-x8jw29`
- Source bead: `ga-wvtgnv`
- Reviewed commit: `84d5244c700ff764813d8ff241d8c55ae2914fed`
- Compared with: `origin/main@8a6809e8fa6dc71faaa04e6a5ee4822dce130275`
- Result: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the
seven release criteria from the active deployer instructions, the source bead's
acceptance criteria, and `TESTING.md`.

## Release criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The source bead records `REVIEW (gascity/reviewer): PASS` for the exact reviewed commit. The reviewer independently ran build, vet, focused `internal/sling` tests, and the complete `cmd/gc` sling test surface. |
| 2 | Acceptance criteria met | PASS | `SlingDeps.WorkQueryProbe` is injected at the CLI boundary; `cliWorkQueryProbe` reuses `hookQueryEnv` and `shellWorkQueryWithEnv`; `finalize` returns an actionable error when the routed bead's exact JSON `id` is absent; dry-run and force-mode semantics are preserved; formulas v2 keep their separate launch-visibility mechanism. The failure and success cases are covered by `TestDoSlingFailsWhenBeadNotVisibleToTargetWorkQuery` and `TestDoSlingSucceedsWhenBeadVisibleToTargetWorkQuery`. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all eight jobs. Uncached focused runs also passed: `go test -count=1 ./internal/sling/...` and `go test -count=1 ./cmd/gc/... -run 'Sling'`. `go build ./...` and `go vet ./...` exited 0. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no blocking issues and only one non-blocking comment-precision nit. Unresolved high-severity findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty before creating this gate artifact; the isolated deploy branch was rechecked clean after committing it. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main 84d5244c700ff764813d8ff241d8c55ae2914fed` exited 0 and produced tree `f391259380c1b2189bd7dc64a85891a530110ab4`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two-commit set touches only `cmd/gc` and `internal/sling` routing/probe code and their tests. Every change supports the single post-routing visibility guarantee. |

## Acceptance evidence

- A routed bead absent from the target's work-query output produces a non-nil
  error naming the bead, target, and `work_query`; the CLI's existing error
  path converts it to a non-zero exit.
- The membership check decodes JSON arrays or objects and compares the exact
  `id` field, avoiding substring false positives.
- The passing case returns success when the routed bead is visible.
- `DoSling` returns before routing on dry-run. `--force` skips the
  postcondition because it explicitly permits dispatch when the local
  bead/query cannot resolve the bead.
- Standalone and attached v1 formulas verify the claimable root or source bead
  through the shared finalizer. Formulas v2 continue through their existing
  workflow-launch visibility check.
- `internal/sling` does not import `cmd/gc`; the shell execution, environment,
  working-directory, and timeout behavior remains owned by the CLI boundary.
- `gofmt -l` returned no changed files and `git diff --check` passed.

## Commands

```text
git fetch origin main
git merge-tree --write-tree origin/main 84d5244c700ff764813d8ff241d8c55ae2914fed
git diff --check origin/main...84d5244c700ff764813d8ff241d8c55ae2914fed
gofmt -l cmd/gc/cmd_sling.go cmd/gc/cmd_sling_test.go cmd/gc/sling_seam_test.go internal/sling/sling.go internal/sling/sling_core.go internal/sling/sling_test.go
go test -count=1 ./internal/sling/...
go test -count=1 ./cmd/gc/... -run 'Sling'
make test-fast-parallel
go build ./...
go vet ./...
git status --porcelain=v1
```
