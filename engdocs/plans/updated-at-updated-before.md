# UpdatedAt and UpdatedBefore Plan

Source bead: `ga-uicyd3`
Architecture source: `source:actual-architect`
Design source: `source:actual-designer`
Target agent: `gascity/builder`
Priority: P2

## Goal

Implement first-class last-mutation timestamps for beads so retention and
operator-facing views use the correct activity time instead of approximating
with creation time. The work adds `Bead.UpdatedAt`, adds
`ListQuery.UpdatedBefore`, wires the existing `bd list --updated-before` CLI
surface, exposes `updated_at` through the API, and displays it in the dashboard
detail panel.

Tracker import: no tracker skill is installed in this rig, so there were no
external tracker issues to materialize.

## Work Packages

1. `ga-uicyd3.1` - Builder: add UpdatedAt and UpdatedBefore to the beads query contract
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `beads.Bead` exposes `UpdatedAt` with
     `json:"updated_at,omitempty"`; `beads.ListQuery` exposes
     `UpdatedBefore`; `HasFilter()` recognizes it; `Matches()` uses UpdatedAt
     with CreatedAt fallback and excludes records at or after the cutoff;
     tests cover filters, boundaries, fallback, and zero-query behavior.

2. `ga-uicyd3.2` - Builder: stamp UpdatedAt across bead stores
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-uicyd3.1`
   - Acceptance: HQStore initializes UpdatedAt on create and stamps Update,
     Close, Reopen, CloseAll, and SetMetadataBatch; MemStore stamps equivalent
     mutations; BBoltStore write paths are audited and stamped where reachable;
     tests verify initialization, mutation advancement, and old snapshot
     compatibility.

3. `ga-uicyd3.3` - Builder: wire BdStore and terminal retention to UpdatedBefore
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-uicyd3.1`, `ga-uicyd3.2`
   - Acceptance: BdStore passes `--updated-before` with RFC3339Nano formatting
     and parses `updated_at`; the hqstore coordstore adapter's PurgeTerminal
     uses `UpdatedBefore` instead of `CreatedBefore`; CachingStore retention
     calls use `Live: true`; tests cover CLI wiring, JSON parsing, and the
     retention case where a recently closed old bead is not purged.

4. `ga-uicyd3.4` - Builder: expose updated_at through the API schema and generated clients
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-uicyd3.1`
   - Acceptance: API Bead responses include optional `updated_at` date-time
     and omit it when zero; Huma remains the generated OpenAPI source;
     `internal/api/openapi.json`, `docs/schema/openapi.json`, and generated
     dashboard TypeScript types are in sync; API tests cover populated and zero
     timestamps.

5. `ga-uicyd3.5` - Builder: display Updated timestamp in dashboard issue detail
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-uicyd3.4`
   - Acceptance: the issue detail panel renders `Updated:` immediately after
     `Created:` only when `updated_at` is present and differs from
     `created_at` by more than one second; the line uses existing timestamp
     formatting, `<time datetime>`, and the same secondary metadata color;
     tests cover display, hide, threshold, and semantic datetime behavior.

## Dependency Graph

- `ga-uicyd3.1` blocks all implementation surfaces that consume the new field.
- `ga-uicyd3.2` depends on `ga-uicyd3.1`.
- `ga-uicyd3.3` depends on `ga-uicyd3.1` and `ga-uicyd3.2`.
- `ga-uicyd3.4` depends on `ga-uicyd3.1`.
- `ga-uicyd3.5` depends on `ga-uicyd3.4`.

## Guardrails

- Do not add `UpdatedAfter`, `SortUpdatedAsc`, or `SortUpdatedDesc`.
- Do not add a new HQStore index for `UpdatedBefore`.
- Do not remove or reinterpret `gc.hqstore.closed_at`; that retention path is
  separate from the coordstore benchmark adapter's PurgeTerminal contract.
- Do not use metadata as the source of truth for UpdatedAt.
- Dashboard production styling must not use the blue wireframe highlight.
- If `issue-detail-created` lacks semantic `<time datetime>`, fix Created and
  Updated together in the dashboard bead.

## Future Follow-Up

The dashboard list table's current Age column appears to represent creation
age. Switching that column to last-activity age may be useful, but it is out of
scope for this implementation and should get separate product/design review.
