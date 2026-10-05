# Formula Hash Apply-Plan Deploy Split

Owner: `gascity/pm`
Created: 2026-06-09
Source beads: `ga-9rc1br`, `ga-lsyuse`, `ga-qfxpl2`, `ga-vzxppb`
PR: https://github.com/gastownhall/gascity/pull/3265

## Goal

Recover the failed formula-hash deploy by splitting the contaminated PR into
single-theme release lanes. The order-dispatch concurrent-map fix must receive
its own test, review, and deploy path before the original formula-hash deploy is
retried.

## Context

`ga-qfxpl2` passed review for the formula-hash apply-plan fix through commit
`c65de0e98c4b3df6c9a089a93408c9cc102a06f4`. The reviewed scope was limited to
`internal/molecule/graph_apply.go` and `internal/molecule/molecule_test.go`.
Earlier source bead `ga-vzxppb` passed review for the same formula-hash theme
before the branch was rebased; deploy bead `ga-lsyuse` failed for the same
mixed-theme reason as `ga-9rc1br`.

The deploy gate for `ga-9rc1br` failed after PR #3265 gained an independent
order-dispatch commit, `0af3afa4d8ddba0aaf0d4bc87ad162069e29e60c`, plus a
release-gate FAIL marker at `db1197b421fac096a7f253d5449ac9a9f9dfcb47`. The
gate evidence records failures for review coverage, acceptance scope, test
status, and single feature theme.

The order-dispatch panic is real release risk. The clean path is to ship that
fix as an independent, reviewed release unit, then retry the formula-hash
deploy from a clean branch.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-9rc1br.1` | `gascity/validator` | `needs-tests` | Define regression coverage for the `cmd/gc/order_dispatch.go` concurrent map write seen in CI. |
| `ga-9rc1br.2` | `gascity/builder` | `ready-to-build` | Isolate the order-dispatch tracking-index mutex fix on a clean branch, with validator-approved coverage and no molecule files. |
| `ga-9rc1br.3` | `gascity/reviewer` | `needs-review` | Review only the isolated order-dispatch concurrency fix and record PASS or CHANGES. |
| `ga-9rc1br.4` | `gascity/deployer` | `needs-deploy` | Gate and PR the reviewed order-dispatch fix as its own single-theme deploy. |
| `ga-9rc1br.5` | `gascity/deployer` | `needs-deploy` | Retry the original formula-hash apply-plan deploy from a clean branch after the order-dispatch lane is resolved. |

## Dependency Graph

`ga-9rc1br.1` -> `ga-9rc1br.2` -> `ga-9rc1br.3` -> `ga-9rc1br.4` -> `ga-9rc1br.5`

The formula-hash redeploy waits for the order-dispatch deploy because the
release gate observed a concurrent-map panic in the shared CI path. The two
feature themes still remain separate release units.

## Out Of Scope

- Writing implementation code in the PM worktree.
- Merging any PR directly from an agent session.
- Treating PR #3265's current mixed head as deployable.
- Re-reviewing the already-passed formula-hash commits unless the clean branch
  changes their behavior.

## Handoff

All child beads carry `source:actual-pm` plus `gc.routed_to` metadata for their
target agent. Downstream agents should record exact branch, commit, PR, and gate
evidence on their beads. Any failed gate returns to PM with the failed criteria
and artifact path.
