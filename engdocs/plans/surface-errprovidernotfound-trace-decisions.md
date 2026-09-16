# Plan: surface ErrProviderNotFound skips as trace decisions

> **Status:** decomposed - 2026-05-31
> **Root bead:** `ga-2jnm5r` - Surface ErrProviderNotFound skips as trace decisions in buildDesiredState
> **Architecture parent:** `ga-lp4ciu` (closed)
> **Designer handoff:** `gm-wisp-vb6te9` (2026-05-31)
> **Decomposed into:** 3 child beads - `ga-2jnm5r.1`, `ga-2jnm5r.2`, `ga-2jnm5r.3`

## Context

The reconciler currently writes stderr skip lines when provider resolution
fails with `config.ErrProviderNotFound`, but those skips are invisible in the
session reconciler trace. Operators investigating why a session was omitted
from desired state need a typed trace decision for the three known skip paths.

The designer handoff confirmed the architect spec is implementable and added
one correction: `applySessionBeadDesiredOverlay` also needs a trace parameter
because the originally identified call site is inside that helper, not directly
inside `buildDesiredStateWithSessionBeads`.

No additional design work is needed. This plan routes validator coverage first,
then the builder implementation slices.

## Plan

| Bead | Route | Scope |
|---|---|---|
| `ga-2jnm5r.1` | `gascity/validator` | Regression coverage for provider-not-found desired-state skips and trace decisions. |
| `ga-2jnm5r.2` | `gascity/builder` | Thread trace through `discoverSessionBeadsWithRoots` and `applySessionBeadDesiredOverlay`. |
| `ga-2jnm5r.3` | `gascity/builder` | Record `build_desired_state.unresolvable_provider` decisions at the three skip sites. |

## Acceptance Summary

### `ga-2jnm5r.1` - validator coverage

- Exercises an `ErrProviderNotFound` desired-state skip and proves the skipped
  session is absent from desired state.
- Asserts trace decision fields: site code
  `build_desired_state.unresolvable_provider`, reason `provider_not_found`,
  outcome `skipped`, and populated provider/template/session values.
- Covers the session-bead/template path that requires trace threading, or
  records a concrete blocker if that harness is unavailable.
- Confirms nil-trace behavior remains safe and preserves existing stderr skip
  behavior.

### `ga-2jnm5r.2` - trace plumbing

- Adds `trace *sessionReconcilerTraceCycle` before `stderr io.Writer` on
  `discoverSessionBeadsWithRoots`.
- Adds the same trace parameter to `applySessionBeadDesiredOverlay` and passes
  it through to discovery.
- Passes `trace` from `buildDesiredStateWithSessionBeads`.
- Passes `nil` from `refreshDesiredStateWithSessionBeads` and the
  `discoverSessionBeads` wrapper.
- Leaves nil trace behavior unchanged.

### `ga-2jnm5r.3` - provider skip trace decisions

- Pool no-store, named session, and session-bead/template skip paths each call
  `trace.recordDecision` only when `errors.Is(err, config.ErrProviderNotFound)`
  and trace is non-nil.
- Uses site code `build_desired_state.unresolvable_provider`, reason
  `provider_not_found`, and outcome `skipped`.
- Populates template/session/provider as specified in the designer handoff; the
  session-bead path also includes `bead_id`.
- Preserves all existing stderr skip lines and continue behavior.
- Does not record generic template resolution failures.
- Does not add new event types, event payloads, OpenAPI/dashboard generated
  types, or config resolver behavior.

## Dependencies

- `ga-2jnm5r.2` depends on `ga-2jnm5r.1`.
- `ga-2jnm5r.3` depends on `ga-2jnm5r.1` and `ga-2jnm5r.2`.

This keeps the work TDD-shaped: validator coverage lands first, trace plumbing
unblocks the call path, and decision recording lands last.

## Routing

- `ga-2jnm5r.1` carries label `needs-tests` and
  `gc.routed_to=gascity/validator`.
- `ga-2jnm5r.2` carries label `ready-to-build` and
  `gc.routed_to=gascity/builder`.
- `ga-2jnm5r.3` carries label `ready-to-build` and
  `gc.routed_to=gascity/builder`.

After the root closes, sling all three child beads so validator and builder
sessions wake immediately. Builder beads remain dependency-gated by bd.

## Risks And Non-Goals

- The session-bead path is the most important coverage target because it is the
  path affected by the trace-threading correction.
- This work intentionally does not add typed events or dashboard/API surfaces.
- This work intentionally does not alter `internal/config/resolve.go`; the
  existing `config.ErrProviderNotFound` sentinel is the contract.
- This work intentionally does not trace every generic template resolution
  error in `build_desired_state.go`.
