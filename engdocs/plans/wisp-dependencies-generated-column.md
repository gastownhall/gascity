# Wisp Dependencies Generated Column Plan

Source bead: `ga-3agym`
Architecture parent: `ga-ghfv2`
Target rig: `beads`
Priority: P2

## Goal

Stop the bd CLI wisp-sync path from inserting explicit values into
`wisp_dependencies.depends_on_id` after that column became GENERATED
STORED in the split target schema.

## Work Packages

1. `be-rkwj2` — Tests: wisp_dependencies sync omits generated depends_on_id
   - Route: `beads/validator`
   - Label: `needs-tests`
   - Source metadata: `ga-3agym`
   - Acceptance: reproduce the split-schema failure, exercise infra issue
     dependency mirroring, and assert generated `depends_on_id` derives
     from the split target columns.

2. `be-hewzr` — As a bd operator, wisp dependency mirroring no longer writes generated depends_on_id
   - Route: `beads/builder`
   - Label: `ready-to-build`
   - Depends on: `be-rkwj2`
   - Acceptance: remove `depends_on_id` from the sync INSERT column list
     and SELECT projection while preserving the split target columns.

3. `be-8m5vh` — Verify: wisp_dependencies errno 1105 no longer appears during bd reconcile
   - Route: `beads/validator`
   - Label: `needs-tests`
   - Depends on: `be-hewzr`
   - Acceptance: run dependency-aware bd operations after the fix and
     confirm `dolt.log` no longer records errno 1105 for
     `wisp_dependencies` generated-column writes.

## Dependency Graph

`be-rkwj2` blocks `be-hewzr`, which blocks `be-8m5vh`.

## Guardrails

- Do not modify the legacy `0035_migrate_infra_to_wisps.up.sql` branch
  where `depends_on_id` was still a regular column.
- Keep `depends_on_issue_id`, `depends_on_wisp_id`, and
  `depends_on_external` in the sync INSERT and SELECT.
- Keep the executable work in the `beads` rig database so routing to
  `beads/builder` and `beads/validator` is valid.
