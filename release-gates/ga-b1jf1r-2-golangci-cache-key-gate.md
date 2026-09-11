# Release Gate: Golangci-Lint Cache Key Clean Branch

Bead: `ga-b1jf1r.2`

Parent deploy bead: `ga-b1jf1r`

Source review bead: `ga-tkg45j`

Clean branch source bead: `ga-b1jf1r.1`

Branch under review: `fix/golangci-cache-key-clean`

Local deploy branch: `deploy/ga-b1jf1r-2-golangci-cache-key`

Candidate head: `34e48ee36616836b1e321bca5df4a74b69fd9ec6`

Current base checked: `origin/main` at `c1a5f331a396a9699bf9ef5db8d8bff4b751e34a`

Merge base: `c1a5f331a396a9699bf9ef5db8d8bff4b751e34a`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Parent review bead `ga-tkg45j` is closed with close reason `pass`; notes contain `PASS (gascity/reviewer)` and record the reviewed cache-key fix. Clean-branch bead `ga-b1jf1r.1` is closed and records branch `fix/golangci-cache-key-clean` at `34e48ee36616836b1e321bca5df4a74b69fd9ec6`. |
| 2 | Acceptance criteria met | PASS | The effective diff from `origin/main` is scoped to `.github/workflows/ci.yml`, with one line removing `Makefile` from the golangci-lint `hashFiles(...)` cache key. `test/integration/review_formula_test.go` is absent from the diff. |
| 3 | Tests pass | PASS | `make build` passed. `go vet ./...` passed. `make lint-full` passed with `0 issues`. `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Source review notes list all findings as PASS and record no blockers or HIGH findings. |
| 5 | Final branch is clean | PASS | Before writing this gate artifact, `git status --short --branch` showed a clean branch at `34e48ee36616836b1e321bca5df4a74b69fd9ec6`. This artifact is the only deployer-authored file to be committed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `c1b8ae42f808a88ff55e28f0e88b3884fb071be6`; no merge conflicts. `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | The final branch is a single CI cache-key fix. It excludes the independent reviewWorkflowTimeout change from PR #3595. |

## Scope Notes

The clean branch is based directly on current `origin/main`:

```text
HEAD       34e48ee36616836b1e321bca5df4a74b69fd9ec6
origin/main c1a5f331a396a9699bf9ef5db8d8bff4b751e34a
merge-base  c1a5f331a396a9699bf9ef5db8d8bff4b751e34a
```

The effective diff is:

```text
.github/workflows/ci.yml | 2 +-
1 file changed, 1 insertion(+), 1 deletion(-)
```

The exact change removes `Makefile` from the golangci-lint cache key:

```diff
-          key: ${{ runner.os }}-golangci-lint-${{ hashFiles('go.sum', '.golangci.yml', 'Makefile') }}
+          key: ${{ runner.os }}-golangci-lint-${{ hashFiles('go.sum', '.golangci.yml') }}
```

`git diff --name-only origin/main...HEAD` reports only:

```text
.github/workflows/ci.yml
```

## Commands Run

```bash
gh auth status
git fetch origin main fix/golangci-cache-key-clean:refs/remotes/origin/fix/golangci-cache-key-clean
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
make build
go vet ./...
make lint-full
make test-fast-parallel
```

## Decision

Gate PASS. Push `fix/golangci-cache-key-clean`, open a scoped PR to
`gastownhall/gascity:main`, then route the merge-request to mayor/mpr. Do not
merge from the deployer session.
