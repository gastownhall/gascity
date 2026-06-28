# Release Gate: Formula Extension Pass-Through

Decision: PASS

## Scope

- Deploy bead: ga-cch93d
- Source review bead: ga-negyxm
- Branch: builder/ga-8ymdco-formula-ext-fix
- Candidate commit: 4417eb5edc68066d79f173cd79044ed5ec81e2ec
- Base: origin/main 4c3b612b7fc0cbfff01ae527a9dcd4a1e9ee5741
- Shape: single-bead PR

This release fixes formula name resolution when callers pass a formula file
name with an existing known extension, such as `loop-flow.toml`,
`loop-flow.formula.toml`, or `loop-flow.formula.json`. The change strips the
known extension before the resolver probes the configured extension order, so
callers no longer end up probing doubled names like `loop-flow.toml.toml`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-negyxm is closed with `REVIEW VERDICT: PASS` for commit 4417eb5edc68066d79f173cd79044ed5ec81e2ec. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `internal/formula/resolve.go` and `internal/formula/resolve_test.go`. `ResolveWithSource` now strips known caller-supplied formula extensions before probing. `TestResolve_ExtensionPassThrough` covers `.toml`, `.formula.toml`, and `.formula.json`. CLI smoke verified `gc formula show loop-flow` and `gc formula show loop-flow.toml` render identical output in a scratch city. |
| 3 | Tests pass | PASS | `go test ./internal/formula` passed. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. `go build -o /tmp/gc-ga-cch93d ./cmd/gc` passed. |
| 4 | No high-severity review findings open | PASS | Review bead findings are all `[PASS]`; no unresolved HIGH or CRITICAL findings appear in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. The only deployer-authored change is this release gate checklist. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed on the candidate branch; the feature commit is directly on current `origin/main`. |
| 7 | Single feature theme | PASS | Commit set is one formula resolver fix plus focused tests in `internal/formula`; no unrelated packages or user-facing themes are included. |

## Commands Run

```text
go test ./internal/formula
make test-fast-parallel
go vet ./...
go build -o /tmp/gc-ga-cch93d ./cmd/gc
timeout 20 /tmp/gc-ga-cch93d --city <scratch-city> formula show loop-flow
timeout 20 /tmp/gc-ga-cch93d --city <scratch-city> formula show loop-flow.toml
```

The scratch `formula cook loop-flow.toml` runtime smoke was attempted but not
used as release evidence because the scratch city lacked a fully initialized
native bead-store identity for cooking. That setup failure is outside the
candidate diff; the resolver path used by show and cook is covered by
`TestResolve_ExtensionPassThrough` and the passing fast suite.
