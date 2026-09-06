# Named-Session Cold-Wake Clean Deploy Recovery

Owner: `gascity/pm`
Created: 2026-07-25
Source beads: `ga-k3jb5n`, `ga-d5au8t`
Priority: P3

## Goal

Recover the blocked deployment of the named-session `on_demand` cold-wake fix
without shipping the unrelated worktree-liveness change that precedes it in the
reviewed branch.

The deploy gate rejected `8a8564edcf44deafc8685f352bb6c1b5663fcffe`
because its effective diff from `origin/main` includes both product themes. The
prior review remains useful evidence about behavior, but it does not authorize a
different commit for deployment.

Tracker import was checked during intake. The `tracker-to-beads` skill is not
materialized in this session, so the import step was a no-op.

## Work Package

| Bead | Route | Outcome |
| --- | --- | --- |
| `ga-k3jb5n.1` | `gascity/builder` | Produce a current-main, single-theme commit for the named-session cold custom-`scale_check` wake fix, prove both the routed-demand wake and `always`-mode suppression boundaries, push it, and request fresh review of its exact SHA. |

This recovery uses one work package because branch isolation, regression
coverage, gate evidence, and exact-SHA reviewer handoff are one release
contract. Splitting them across independently authored candidates would recreate
the provenance ambiguity that stopped the original deployment.

## Acceptance Boundary

The replacement candidate must:

- start from current `origin/main`;
- contain only the named-session cold custom-`scale_check` wake behavior and
  its regression coverage;
- exclude every change introduced by unrelated parent
  `7eb9f2d7e3d07b2ec7ab175b6897531c3b56c6c5`;
- prove that routed demand wakes one ephemeral target when the named-session
  pool is cold;
- prove that `namedSessionMode == "always"` does not create a numbered phantom
  target;
- record the base SHA, head SHA, changed-file list, commit list, and applicable
  quality-gate results; and
- route the exact pushed head SHA through a new reviewer handoff.

The work package must not deploy, open a PR, or merge. A reviewer may create a
replacement deploy bead only after the clean head SHA passes review.

## Dependency and Handoff

`ga-k3jb5n.1` retains the original implementation bead `ga-d5au8t` and reviewed
commit `8a8564edcf44deafc8685f352bb6c1b5663fcffe` as provenance. They are evidence
for reconstructing and testing the intended behavior, not deploy sources.

The invalid deploy bead `ga-k3jb5n` is retired after this plan is committed and
verified. The builder work package then flows through a fresh exact-SHA review.
Only that review can create a new deploy bead for the isolated candidate.

## Risks and Controls

- **Scope contamination:** Require an `origin/main...HEAD` changed-file and
  commit inventory before review.
- **Review transfer:** Treat the prior PASS as non-transferable; review the new
  head SHA independently.
- **Suppression regression:** Require explicit coverage of the `always`-mode
  boundary, in addition to the cold routed-demand wake.
- **Premature release:** Prohibit PR creation and deployment from the builder
  bead.
