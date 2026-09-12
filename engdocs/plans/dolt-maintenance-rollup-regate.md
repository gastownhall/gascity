# Dolt Maintenance Rollup Re-Gate

Owner: `gascity/pm`
Created: 2026-06-17
Source bead: `ga-hxy2j3`
Related PR: https://github.com/gastownhall/gascity/pull/3579

## Goal

Recover the failed Dolt Storage Maintenance runbook re-gate without opening a
second overlapping PR. The current candidate must be treated as a reviewed
rollup, then used to update or supersede PR #3579 only after that review and
the deploy gate pass.

## Context

The deploy gate for `ga-hxy2j3` failed before PR creation. The gate artifact
in the deployer worktree records that `builder/ga-84xwd5.1` at
`7c3db2116252f91e0be4f61b9d332f5448da0f62` is not a clean reviewed deploy
unit yet. It contains the open PR #3579 lineage plus four newer commits:

- `159e75ca7` docs(runbooks): add Dolt Storage Maintenance operator runbook
- `016871fd6` docs(runbooks): fix link format and promote Observability subheadings
- `989b5de45` test(gastown): accept hook-claim polecat startup
- `7c3db2116` chore(packs): bump gastown to 0.1.10, verify gascity at 0.1.6

The runbook commits were reviewed through `ga-wexskk` at `016871fd6`, but the
current head also includes unreviewed Gastown conformance and pack-pin changes.
PR #3579 is already open for the Dolt maintenance retirement lineage and must
not be duplicated by a second overlapping PR.

Tracker import was a no-op for this PM pass because no `tracker-to-beads`
command or skill path was present in the worktree or rig path.

## Decision

Use the current candidate as a rollup candidate, not as an automatically
deployable unit. The rollup needs one reviewer pass over the full current head.
If review passes and explicitly accepts the rollup scope, deployer should
re-run the gate and update or supersede PR #3579 rather than opening another
overlapping PR.

If review rejects the rollup scope, the work returns to PM with the requested
split. PM will then choose between waiting for PR #3579 to land and rebasing
the runbook/pack changes, or creating separate review and deploy lanes.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-hxy2j3.1` | `gascity/reviewer` | `needs-review` | Review the full rollup head `7c3db2116`, including the runbook, Gastown test, and pack-pin changes. |
| `ga-hxy2j3.2` | `gascity/deployer` | `needs-deploy` | After reviewer PASS, re-run the deploy gate and update or supersede PR #3579 instead of opening an overlapping PR. |

## Dependency Graph

`ga-hxy2j3.2` depends on `ga-hxy2j3.1`.

The deployer must not act until the reviewer records PASS or CHANGES for the
full current head. The existing `ga-hxy2j3` PM bead is closed once these child
beads are created and routed because the PM sequencing decision is complete.

## Acceptance Details

Reviewer acceptance:

- Review `builder/ga-84xwd5.1` at
  `7c3db2116252f91e0be4f61b9d332f5448da0f62` or a newer head explicitly
  handed off by builder.
- Cover the full rollup scope: PR #3579 lineage, runbook commits
  `159e75ca7` and `016871fd6`, Gastown test commit `989b5de45`, and pack-pin
  commit `7c3db2116`.
- Explicitly state whether the combined scope is acceptable as one reviewed
  release unit for updating or superseding PR #3579.
- Record PASS or CHANGES, exact branch, base SHA, head SHA, diff scope, checks
  reviewed or run, and any high-severity findings.
- If the rollup should be split, route back to PM with the required split and
  do not pass the deploy bead.

Deployer acceptance:

- Wait for `ga-hxy2j3.1` reviewer PASS for the same head being gated.
- Rerun the standard deploy gate from current `origin/main` using the
  reviewer-approved head only.
- Do not open a duplicate overlapping PR. If PR #3579 is still open, update its
  branch or mark it superseded by the reviewed rollup path per deployer
  convention. If PR #3579 has already merged or closed, route back to PM before
  creating any new PR.
- Gate evidence must include review coverage for the current head, acceptance
  coverage for the runbook plus Gastown pack/test changes, passing tests
  including `scripts/update-bundled-gastown-pack --check`, branch cleanliness,
  merge cleanliness, and single-theme justification under the reviewed rollup.
- On PASS, record the PR URL and route the merge request to mayor/mpr only. On
  FAIL, route back to PM with the gate artifact and failed criteria.

## Out Of Scope

- PM-authored implementation, test, or branch surgery.
- Direct merge of PR #3579 by any rig agent.
- Opening a second PR that overlaps the still-open #3579 lineage.
