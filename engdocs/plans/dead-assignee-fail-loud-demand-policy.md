# Dead-Assignee And Fail-Loud Demand Policy Plan

Root bead: `ga-o3ko1j.4`
Source: architect decision `ga-o3ko1j`, designer reroute as backend-only work
Date: 2026-07-23
Priority: P2

## Goal

Turn the scale-from-zero demand policy gap into ordered work packages. The
outcome is that work assigned to a confirmed-dead session can wake the owning
template, and routed demand that cannot wake a `min=0` target is surfaced
loudly instead of silently stranding.

## Scope

Included:

- Backend architecture for confirmed-dead assignee demand fallback.
- Backend architecture for fail-loud stranded routed demand, including staged
  rollout from warning-only to hard failure.
- Regression coverage before implementation.
- Builder implementation for the dead-assignee fallback.
- Builder implementation for the fail-loud routed-demand policy.
- Final validation against GH issue #3872 / bead `ga-f4tu7c`, PR #3885 scope,
  and sibling read-side normalization bead `ga-o3ko1j.3`.

Out of scope:

- Pack named-session pool identity materialization, split to `ga-3prlpb`.
- Read-side `gc.routed_to` normalization, split to `ga-o3ko1j.3`.
- Changes to `readyExcludeTypes` or the blocking-dependency gate.
- UI, dashboard, or API work unless architecture explicitly requires a typed
  wire surface.
- Any hardcoded role behavior in Go.

## Work Packages

1. `ga-o3ko1j.4.1` - Specify dead-assignee demand fallback and fail-loud rollout

   Routing: `needs-architecture`, `gc.routed_to=gascity/architect`

   Acceptance focus:

   - Define the mechanical confirmed-dead signal and exclude idle, asleep, and
     uncertain sessions.
   - Define how a dead assignee maps back to the target template.
   - Define the fail-loud surface for sling, direct metadata writes, and order
     dispatch.
   - Define the rollout flag: default OFF warning period, later default ON hard
     failure.
   - Preserve `readyExcludeTypes` and blocking-dependency exclusions.
   - Cross-check `ga-o3ko1j.3`, `ga-3prlpb.1`, GH #3872 / `ga-f4tu7c`, and
     PR #3885.

2. `ga-o3ko1j.4.2` - Write dead-assignee and fail-loud regression coverage

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance focus:

   - Tests cover confirmed-dead assignee demand and no fallback for live, idle,
     asleep, or uncertain identities.
   - Tests cover stranded routed demand from sling, direct metadata writes, and
     order-dispatch style paths.
   - Tests cover default-OFF warning-only behavior and flag-ON hard failure.
   - Tests include the graph.v2 drain-unit-member repro from GH #3872 /
     `ga-f4tu7c` or a faithful fixture.
   - Tests prove excluded bead types and blocked work remain excluded.

3. `ga-o3ko1j.4.3` - Implement dead-assignee pool-wake fallback

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance focus:

   - Confirmed-dead assignee identities count as demand for the correct
     template.
   - Live, idle, asleep, and unknown identities do not trigger the fallback.
   - Demand computation stays object-model driven and mechanical.
   - `max_active_sessions` remains respected when resume-tier and new-spawn
     demand coexist.
   - `readyExcludeTypes` and blocking-dependency gates are unchanged.

4. `ga-o3ko1j.4.4` - Implement fail-loud stranded routed demand policy

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance focus:

   - Routed-but-unwakeable `min=0` demand surfaces through the
     architecture-approved warning or failure mechanism.
   - The rollout flag defaults OFF and supports the later ON hard-failure
     state.
   - The policy applies to all `gc.routed_to` writers, not only order dispatch.
   - Event/API surfaces remain typed if touched.
   - Operator-facing context is recorded without swallowing errors.

5. `ga-o3ko1j.4.5` - Verify scale-from-zero demand policy end to end

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance focus:

   - Focused tests pass for dead-assignee fallback, warning-only rollout,
     hard-failure rollout, direct metadata routing, sling routing, and
     order-dispatch routing.
   - GH #3872 / `ga-f4tu7c` incident #5 is fixed or a separate root-cause
     follow-up is filed.
   - Interaction with `ga-o3ko1j.3` read-side normalization is verified.
   - `readyExcludeTypes` and blocking-dependency exclusions are rechecked.
   - `go test ./...` and `go vet ./...` pass, or exact blockers are recorded.

## Dependency Graph

```text
ga-3prlpb.1
  -> ga-o3ko1j.4.1

ga-o3ko1j.4.1
  -> ga-o3ko1j.4.2
  -> ga-o3ko1j.4.3
  -> ga-o3ko1j.4.4

ga-o3ko1j.4.2
  -> ga-o3ko1j.4.3
  -> ga-o3ko1j.4.4

ga-o3ko1j.4.2, ga-o3ko1j.4.3, ga-o3ko1j.4.4, ga-o3ko1j.3
  -> ga-o3ko1j.4.5
```

## Risks

- The fail-loud default flip may reveal existing strandings across the fleet.
  The architecture bead must define the warning bake period clearly.
- The dead-assignee fallback depends on a precise dead-vs-idle distinction.
  Ambiguous or read-error states must fail safe.
- `ga-o3ko1j.3` can mask or reveal demand matching symptoms, so final
  verification depends on that sibling normalization work.

## Handoff State

All child beads are created with `source:actual-pm`, routing labels, and
`gc.routed_to` metadata. Tracker import was a no-op because no
`tracker-to-beads` skill or CLI is installed in this worktree.
