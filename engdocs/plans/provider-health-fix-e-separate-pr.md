# Provider Health Fix E Separate PR

Owner: `gascity/pm`
Created: 2026-06-23
Source beads: `ga-5x9zuh`, `ga-fpbptk`, `ga-4qbgqf.5`
Related PR: https://github.com/gastownhall/gascity/pull/3687

## Goal

Recover the failed deploy gate for provider-health Fix E by moving it into its
own release lane. The work must not be added to the existing A/B/C partial-demand
PR unless a maintainer explicitly authorizes a combined release.

## Context

Reviewer bead `ga-fpbptk` passed commit `d4c0fa80b` on
`builder/ga-4qbgqf.2-partial-demand-create-gate`. Deployer triage on
`ga-5x9zuh` then failed the gate for acceptance/scope reasons: Fix E was present
on the same branch already backing PR #3687, whose body still describes only the
A/B/C partial-demand work.

Source bead `ga-4qbgqf.5` acceptance criterion 4 requires Fix E to land in a PR
separate from A/B/C and separate from Fix D unless the maintainer explicitly
combines them. No explicit maintainer combine authorization is recorded in
`ga-5x9zuh`, `ga-fpbptk`, `ga-4qbgqf.5`, or PR #3687.

## Decision

Split Fix E into a separate release lane. Do not update PR #3687 with Fix E and
do not treat the current mixed branch as deployable for Fix E.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-5x9zuh.1` | `gascity/builder` | `ready-to-build` | Produce an isolated Fix E candidate branch that contains only the provider-health create-gate scope from `ga-4qbgqf.5`; record branch, commit, diff scope, and test evidence. |
| `ga-5x9zuh.3` | `gascity/reviewer` | `needs-review` | Review the isolated Fix E candidate for the `ga-4qbgqf.5` acceptance criteria and confirm it is separate from A/B/C and Fix D. |
| `ga-5x9zuh.2` | `gascity/deployer` | `needs-deploy` | Run the standard deploy gate on the reviewed isolated branch, open a separate Fix E PR on PASS, and route the merge request to mayor/mpr without merging from an agent session. |

## Dependency Graph

`ga-5x9zuh.1` -> `ga-5x9zuh.3` -> `ga-5x9zuh.2`.

The deploy task must not begin until reviewer records PASS on the isolated Fix E
candidate. If isolation proves impossible without combining with A/B/C, route
back to PM with the exact blocker rather than updating PR #3687.

## Out Of Scope

- Requesting maintainer combine authorization as the default path.
- Adding Fix E to PR #3687.
- Merging any PR directly from an agent session.
- Reopening A/B/C partial-demand implementation scope beyond what is necessary
  to keep Fix E isolated.

## Handoff

All child beads carry `source:actual-pm` plus `gc.routed_to` metadata for their
target agent. Downstream agents must record exact branch, commit, PR URL, and
gate evidence on their beads. Any failed gate returns to PM with the failed
criteria and artifact path.
