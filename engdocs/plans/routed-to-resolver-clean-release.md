# Routed To Resolver Clean Release

Owner: `gascity/pm`
Created: 2026-07-06
Source bead: `ga-imtb36`
Source review bead: `ga-7fqxay`
Priority: P2

## Goal

Recover the routed_to resolver deploy after the first deploy gate rejected the
candidate branch for bundling multiple independent feature themes.

The intended release scope is only the reviewed `gc.routed_to` resolver
centralization plus the doctor `--fix` reconciliation behavior. The unrelated
pool-resume readiness work must not ship in the same deploy candidate unless a
future PM bead explicitly defines a rollup.

## Context

The reviewer passed commit `c93d9a7bf` on
`builder/ga-79uuwq.1-routed-to-resolver`, with evidence that the routed_to
derivation was centralized and that `doctor --fix` reconciled real fleet drift.

The deploy gate later failed criterion 7 because that branch also contained
unrelated pool-resume readiness work. Tests were intentionally not run because
there was no single-theme deploy branch to validate.

## PM Decision

Use an isolated clean release branch, not a rollup.

The deploy failure identified the pool-resume readiness changes as unrelated,
and the source review evidence only covers the routed_to resolver and doctor
fix behavior. A rollup would expand the release scope without a matching review
and acceptance trail.

## Work Packages

| Bead | Route | Label | Acceptance focus |
|------|-------|-------|------------------|
| `ga-imtb36.1` | `gascity/builder` | `ready-to-build` | Create a branch from current `origin/main` containing only the reviewed routed_to resolver and doctor fix work; exclude unrelated pool-resume readiness changes; record branch name, head SHA, changed files, commit log, and focused verification. |
| `ga-imtb36.2` | `gascity/deployer` | `needs-deploy` | Wait for the clean branch from `ga-imtb36.1`; run the standard deploy gate; verify the diff is a single feature theme; open or update a PR on pass and route merge authority to mayor/mpr. |

## Dependency Graph

`ga-imtb36.1` blocks `ga-imtb36.2`.

The root PM bead `ga-imtb36` is complete once this split plan is recorded and
the downstream beads carry the correct routing metadata. The deploy bead stays
blocked until the builder records clean branch details.

## Handoff

`ga-imtb36.1` is assigned to builder for release-branch isolation.

`ga-imtb36.2` is routed to deployer and must use the clean branch recorded by
builder, not the failed mixed branch. Deployer should record final gate
evidence, PR URL, and merge-request routing on pass, or exact failure evidence
and route back to PM on fail.
