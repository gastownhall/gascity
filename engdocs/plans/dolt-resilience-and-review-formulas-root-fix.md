# Plan: Dolt resilience and review-formulas root-fix sequencing

> Owner: `gascity/pm` - Created: 2026-06-17
> Sources: mayor mail `gm-wisp-t6sgjbe`; P0 root `ga-pqfk8t`; CI root `ga-jtjaiy`

## Goal

Keep the current cycle focused on the P0 Dolt data-plane resilience work
while still making the review-formulas root-fix explicit and trackable.

The mayor sequencing call is:

- `ga-pqfk8t` takes priority.
- `ga-pqfk8t` implementation must wait for architect scoping and explicit
  operator approval because the Dolt data-plane blast radius includes
  data-loss and recovery behavior.
- `ga-jtjaiy` is a load flake under busy-runner conditions, not a product
  regression. Its follow-up must target the gc-hook-under-load root cause,
  not a `reviewWorkflowTimeout` timeout bump.

## Work breakdown

| Bead | Title | Priority | Routes to | Gate |
|------|-------|----------|-----------|------|
| `ga-pqfk8t.1` | Architect scope: Dolt data-plane resilience for upstream #3176 | P0 | `gascity/architect` | `needs-architecture` |
| `ga-jtjaiy.1` | Architect scope: review-formulas gc-hook-under-load root fix | P1 | `gascity/architect` | `needs-architecture` |

No builder or validator implementation bead is created yet. Both work
items need an architecture handoff before PM decomposes build/test work.

## Dependency graph

```text
ga-pqfk8t.1
  -> blocks ga-jtjaiy.1

ga-jtjaiy.1
  -> tracked as child context under ga-jtjaiy
```

This makes the review-formulas root-fix visible without letting it preempt
the P0 Dolt resilience scoping.

## Acceptance summary

### `ga-pqfk8t.1`

1. The architecture handoff covers all three upstream #3176 risk areas:
   bounded Dolt journal growth, complete/fresh backup coverage for every
   managed DB, and recovery from corrupted journal startup failures.
2. The handoff separates SDK infrastructure changes from operator-run
   recovery/maintenance steps, with implementation gated on explicit
   operator approval.
3. The handoff preserves Gas City invariants: controller-driven SDK
   infrastructure, no hardcoded roles, no stale status files, and no
   consumer-pack assumptions.
4. The handoff calls out compatibility with gascity local-only Beads/Dolt
   policy and does not require `bd dolt push`, `bd dolt pull`, or
   `gc dolt sync` for this rig.
5. The handoff defines downstream builder/validator work packages or asks
   PM to decompose again after architecture.

### `ga-jtjaiy.1`

1. The architecture handoff treats review-formulas as a load flake caused
   by gc-hook/bd probing under busy-runner load, not as a product
   regression.
2. The handoff rejects `reviewWorkflowTimeout` increases as the primary fix
   unless separately justified as temporary mitigation and explicitly linked
   to the root-fix bead.
3. The handoff evaluates the root-fix paths called out by the mayor and
   builder analysis: shorter test-city `bdProbeTimeout`, faster
   `scale_check`/ready probing, and fan-out resilience when runners are
   busy.
4. If a green-main unblock is needed before the fix lands, the handoff
   requires an explicit known-flake marker linked to this root-fix bead
   rather than a silent permanent mask.
5. The handoff yields downstream builder/validator beads or asks PM to
   decompose again after the architecture decision.

## Risks and constraints

- Do not route either bead directly to builder before architecture closes.
- Do not let the review-formulas work turn into a timeout-only workaround.
- Do not run gascity Beads/Dolt push, pull, or sync commands; this rig's
  Beads/Dolt store is local-only.
- Do not encode role names or recovery judgments in Go implementation work;
  architecture must preserve the SDK role-configuration boundary.
