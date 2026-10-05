# Golangci Cache Key Clean Branch Recovery

Source PM bead: `ga-b1jf1r`
Related work: `ga-tkg45j`, PR #3595
Owner: `gascity/pm`
Created: 2026-06-19
Priority: P2

## Goal

Ship the golangci-lint cache-key fix as an independent release unit, without
bundling it with the separate review workflow timeout branch that already backs
PR #3595.

## Context

Reviewer passed commit `4cc7902b5` on `builder/ga-omnkls` for the one-line CI
cache-key change that removes `Makefile` from the golangci-lint cache key. The
deploy gate failed branch-scope criterion 7 because that reviewed commit is
stacked on unrelated timeout work. As of 2026-06-19, PR #3595 remains open from
`builder/ga-omnkls` at `dbd97dea5` for the timeout change.

The cache-key fix can ship independently. The recovery path is to prepare a
fresh origin/main-based branch containing only the reviewed cache-key behavior,
then run the deploy gate on that clean branch.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-b1jf1r.1` | ready-to-build: prepare clean branch for golangci-lint cache-key fix | `gascity/builder` | none |
| `ga-b1jf1r.2` | needs-deploy: gate clean golangci-lint cache-key fix branch | `gascity/deployer` | `ga-b1jf1r.1` |

## Acceptance Focus

`ga-b1jf1r.1` is complete when builder records a clean branch based on current
`origin/main`, with a diff limited to `.github/workflows/ci.yml` and no
`test/integration/review_formula_test.go` timeout change.

`ga-b1jf1r.2` is complete when deployer gates the builder-provided clean branch,
opens a scoped PR on PASS, and routes merge authority to mayor/mpr. On FAIL,
deployer records the exact failed criterion and routes the bead back to PM.

## Dependency Graph

`ga-b1jf1r.1` -> `ga-b1jf1r.2`

The deploy retry waits for the clean branch because the previous gate failure
was caused by release-unit contamination, not by the product requirement or the
reviewed CI change.

## Out Of Scope

- Waiting for PR #3595 to merge before recovering the cache-key fix.
- Reusing `builder/ga-omnkls` for the cache-key deploy.
- Shipping the review workflow timeout change as part of this cache-key PR.
- Merging any PR directly from an agent session.
