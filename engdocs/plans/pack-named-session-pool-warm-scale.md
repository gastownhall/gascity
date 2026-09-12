# Pack Named-Session Pool Warm/Scale Plan

Root bead: `ga-3prlpb`
Source: mayor report, folded into architecture bead `ga-o3ko1j`
Date: 2026-07-23
Priority: P1

## Goal

Turn the confirmed pack named-session pool warm/scale gap into ordered work
packages. The outcome is that rig-patched `min_active_sessions` and
`max_active_sessions` on pack-provided `on_demand` named sessions materialize
real pool instances, and those instance identities can be targeted by
`gc session wake` and `gc sling`.

## Scope

Included:

- Backend architecture for pooled instance identity allocation and registration.
- Regression coverage for warm-to-min, instance-N wake, instance-N sling, and
  combined max ceiling behavior.
- Builder implementation for materialized instance identity registration.
- Builder implementation for proactive warm-to-min convergence.
- Final validator verification against both known production repro classes.

Out of scope:

- Read-side `gc.routed_to` normalization, already split to `ga-o3ko1j.3`.
- Dead-assignee demand fallback and fail-loud stranded demand, split to
  `ga-o3ko1j.4`.
- UI, dashboard, or user-facing design work.
- Any hardcoded role behavior in Go.

## Work Packages

1. `ga-3prlpb.1` - Specify pack named-session pool identity materialization

   Routing: `needs-architecture`, `gc.routed_to=gascity/architect`

   Acceptance focus:

   - Define how rig-patched pool min/max applies to pack-provided
     `on_demand` named-session templates.
   - Define allocation, persistence or derivation, and registration for
     instance identities such as `<rig>/<template>-1`.
   - Align with `session_identity.go` and `session_level_converge.go`.
   - Define one combined `max_active_sessions` ceiling across resume-tier and
     new-spawn identities.
   - Cover both gascity builder and cairn reviewer incidents without role
     hardcoding.

2. `ga-3prlpb.2` - Write pool warm-to-min regression coverage

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance focus:

   - Tests reproduce a pack-provided `on_demand` named-session template with
     rig-patched `min_active_sessions=2` and `max_active_sessions=3`.
   - Tests fail before implementation when only the base template session
     exists.
   - Tests cover instance-N wakeability, slingability, and the combined max
     ceiling.
   - Tests keep `readyExcludeTypes` and blocking-dependency behavior out of
     scope.

3. `ga-3prlpb.3` - Register materialized pack pool instance identities

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance focus:

   - Materialized instance identities become valid session identities.
   - `gc session wake` and `gc sling` can target valid materialized instance
     identities.
   - Invalid or out-of-range identities fail clearly.
   - The implementation follows the architecture-approved convergence path and
     introduces no role-specific Go logic.

4. `ga-3prlpb.4` - Warm pack named-session pools to configured minimum

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance focus:

   - A rig patch raising `min_active_sessions` converges the pack-provided pool
     to that minimum.
   - `max_active_sessions` is honored across resume-tier sessions and new
     spawned demand.
   - The behavior does not require duplicating pack agents in `city.toml`.
   - Existing singleton named-session behavior stays unchanged unless the
     template is configured as a pool.

5. `ga-3prlpb.5` - Verify pack pool warm-to-min end to end

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance focus:

   - Focused tests confirm warm-up, max ceiling, instance-N wake, and instance-N
     sling.
   - The gascity builder and cairn reviewer repros are represented by generic
     fixtures where production code is concerned.
   - Interaction with the dead-assignee/fail-loud policy work is checked.
   - `go test ./...` and `go vet ./...` pass, or exact blockers are recorded.

## Dependency Graph

```text
ga-3prlpb.1
  -> ga-3prlpb.2
  -> ga-3prlpb.3

ga-3prlpb.1, ga-3prlpb.2, ga-3prlpb.3
  -> ga-3prlpb.4

ga-3prlpb.2, ga-3prlpb.3, ga-3prlpb.4
  -> ga-3prlpb.5

ga-3prlpb.1
  -> ga-o3ko1j.4.1
```

## Risks

- The identity materialization contract touches recently refactored session
  convergence code. The architecture bead must resolve ownership before
  builder starts.
- The current live symptom is role-named, but the implementation must be
  generic. Acceptance explicitly forbids role-name-specific Go logic.
- `max_active_sessions` must be one ceiling across resume and spawn paths.
  Treating each tier independently would recreate the operational risk.

## Handoff State

All child beads are created with `source:actual-pm`, routing labels, and
`gc.routed_to` metadata. Tracker import was a no-op because no
`tracker-to-beads` skill or CLI is installed in this worktree.
