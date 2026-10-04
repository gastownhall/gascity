# Named/Direct Startup Kickoff Backstop Plan

Root bead: `ga-zogqc1.2`
Source: `source:actual-architect`
Priority: P2

## Goal

Turn the architect-approved named/direct first-turn delivery guarantee into
ordered work packages. The outcome is a restart-safe reconciler backstop for
named/direct sessions whose initial startup prompt was attempted but did not
lead to forward progress, while preserving the existing pool-managed nudge
behavior.

## Scope

Included:

- Regression coverage for the kickoff state machine before implementation.
- Session-bead kickoff metadata seeding for named/direct sessions.
- A shared grace/backoff/give-up engine reused by the existing pool path and
  the new named/direct startup path.
- Named/direct startup re-nudge behavior at the existing
  `CanReportActivity == false` reconcile gate.
- Durable `given_up` state and a registered typed event on retry exhaustion.
- Comment and documentation cleanup for `GC_STARTUP_PROMPT_DELIVERED` semantics.
- Final validator verification and quality gate reporting.

Out of scope:

- The separate GC_DIR workdir fix already split to `ga-zogqc1.1`.
- PR #3842 continuation-nudge behavior.
- Runtime provider interface changes.
- Any role-specific behavior or hardcoded role names.
- Dashboard/OpenAPI changes unless implementation discovers an actual API touch.

## Work Packages

1. `ga-zogqc1.2.1` - Author regression tests for named/direct startup kickoff backstop

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance:

   - Tests use existing test-time controls, not wall-clock sleeps.
   - Coverage includes due re-nudge, already-progressed no-op, and max-attempt
     exhaustion to `given_up` with a typed event expectation.
   - Existing pool-managed nudge behavior is protected.

2. `ga-zogqc1.2.2` - Seed named/direct startup kickoff metadata and progress binding

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance:

   - Named/direct session beads seed `startup_kickoff_state=pending`,
     `startup_kickoff_started_at`, `startup_kickoff_attempts=0`, and an empty
     or absent `startup_kickoff_last_nudge_at` at the same lifecycle point as
     `named_session` and `session_origin`.
   - Pool-managed session beads do not receive the named/direct kickoff state.
   - The implementation resolves the live-schema progress signal for bound
     work-step sessions.
   - If no clean signal exists for always-awake named sessions without a single
     bound step bead, file a follow-up and keep v1 scoped to bound-step sessions.

3. `ga-zogqc1.2.3` - Generalize pool idle-nudge pacing into a shared backstop engine

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance:

   - The shared engine reuses the current 90s grace, 3min backoff, 3 max
     attempts, and persisted metadata pacing pattern.
   - Pool claim predicate, `pool_managed` gating, and claim-nudge content remain
     behaviorally unchanged.
   - The engine supports separate outstanding-work predicates for pool claims
     and named/direct startup kickoff.

4. `ga-zogqc1.2.4` - Implement named/direct startup backstop and typed give-up event

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance:

   - The named/direct startup backstop runs beside the pool backstop under the
     existing `CanReportActivity == false` gate.
   - Outstanding and due named/direct startup kickoff is re-nudged through the
     existing nudge primitive.
   - Already-progressed bound work is a no-op.
   - Exhaustion persists `startup_kickoff_state=given_up` and emits a typed
     event with a registered payload.
   - Pool-managed sessions are excluded from the named/direct path.

5. `ga-zogqc1.2.5` - Correct startup delivery flag and backstop comments

   Routing: `ready-to-build`, `gc.routed_to=gascity/builder`

   Acceptance:

   - Comments no longer describe `nudgeStalledPoolClaims` as the sole delivery
     backstop.
   - `GC_STARTUP_PROMPT_DELIVERED` is documented as delivery configured or
     attempted, not confirmed receipt.
   - No behavior change is made to the flag or SessionStart hook suppression.

6. `ga-zogqc1.2.6` - Verify named/direct startup guarantee end to end

   Routing: `needs-tests`, `gc.routed_to=gascity/validator`

   Acceptance:

   - Targeted tests confirm re-nudge, no double-nudge after progress, pool-path
     exclusion, and give-up event behavior.
   - Existing pool-path tests still pass.
   - `go test ./...` and `go vet ./...` are run or exact blockers are recorded.
   - Dashboard/OpenAPI checks are confirmed not applicable unless API or schema
     files are touched.

## Dependency Graph

- `ga-zogqc1.2.1` blocks `ga-zogqc1.2.2`, `ga-zogqc1.2.3`, and
  `ga-zogqc1.2.6`.
- `ga-zogqc1.2.2` and `ga-zogqc1.2.3` block `ga-zogqc1.2.4`.
- `ga-zogqc1.2.4` blocks `ga-zogqc1.2.5`.
- `ga-zogqc1.2.2`, `ga-zogqc1.2.3`, `ga-zogqc1.2.4`, and `ga-zogqc1.2.5`
  block final verification in `ga-zogqc1.2.6`.

## Risks

- The forward-progress signal for always-awake named sessions without a single
  bound step bead is intentionally unresolved. Builder must resolve against the
  live schema or file a follow-up and scope v1 to bound-step sessions.
- Shared pacing code must not alter current pool-claim semantics. Validator
  coverage is first in the dependency graph to make that regression visible.
- The typed give-up event must be registered with a payload. This is a CI
  invariant, not optional cleanup.

## Handoff State

The child beads are already created with `source:actual-pm`, routing labels,
`gc.routed_to` metadata, and dependency edges. Validator has started
`ga-zogqc1.2.1`; `ga-zogqc1.2.6` is correctly blocked until implementation and
comment cleanup complete.
