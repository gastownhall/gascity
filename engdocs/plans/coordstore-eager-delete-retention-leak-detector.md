# Coordstore Eager-Delete Retention Plan

Source bead: `ga-7a0x3`
Design bead: `ga-jb78m`
Gate bead: `ga-gjnt7`
Priority: P3

## Goal

Turn the completed eager-delete design into builder-ready work packages while
keeping implementation blocked until the gctest HQStore soak gate graduates.
The feature reduces closed-record accumulation by deleting no-reader wisps
immediately, retaining short-lived post-close readers only for explicit
windows, and surfacing abnormal backstop reclaim as an operator event.

## Work Packages

1. `ga-7a0x3.1` - Builder: eager-delete archived mail wisps
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-gjnt7`
   - Acceptance: Archive deletes the target mail bead immediately;
     ArchiveMany deletes every requested bead without `CloseAll`; the
     already-archived caller contract is preserved or intentionally updated
     across callers and tests; tests cover immediate delete, batch delete
     partial results, and a second Archive call.

2. `ga-7a0x3.2` - Builder: add HQStore retention queue and backstop primitives
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.1`
   - Acceptance: HQStore exposes the retention and backstop behavior from
     `ga-jb78m`; retention entries delete only after `deleteAfter`; closed
     records have a reliable closure timestamp for age checks; options exist
     for main-tier TTL, ephemeral TTL, and leak-detector wiring; unit tests
     cover drain, preservation, closed-before-delete, and cutoff behavior.

3. `ga-7a0x3.3` - Builder: eager-delete convergence wisps and retain roots
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.2`
   - Acceptance: terminal convergence wisps are deleted; convergence roots
     use a 60-second retention window; the store boundary supports retention
     without unnecessary API churn; tests cover terminal wisp deletion and
     root readability during the retention window followed by deletion.

4. `ga-7a0x3.4` - Builder: retain closed convoy beads before deletion
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.2`
   - Acceptance: ConvoyClose enrolls closed convoy beads in a 60-second
     retention window; post-close convoy reads still work during retention;
     retention drain removes the bead after expiry; tests cover enrollment
     and deletion after expiry.

5. `ga-7a0x3.5` - Builder: retain closed session beads for reconciler restarts
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.2`
   - Acceptance: session CloseDetailed and Prune paths use a 5-minute
     retention window; reconciler reads with `IncludeClosed=true` keep
     working during the window; retained session beads are deleted after
     expiry; tests cover close/prune retention and the restart-reader window.

6. `ga-7a0x3.6` - Builder: run three-pass sweeper and emit leak events
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.1`, `ga-7a0x3.3`, `ga-7a0x3.4`, `ga-7a0x3.5`
   - Acceptance: the TTL sweeper runs PurgeExpired, DrainRetentionQueue, and
     PurgeBackstop in order; `StoreBackstopLeakDetected` is registered with a
     typed payload; events fire only above threshold and include main and
     ephemeral counts; logs distinguish normal reclaim from leak detection;
     tests cover above-threshold, below-threshold, and main-tier orphan cases.

7. `ga-7a0x3.7` - Builder: add soak assertions for bounded backstop reclaim
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-7a0x3.6`
   - Acceptance: the soak or benchmark harness records backstop reclaim after
     warm-up; assertions verify reclaim stays at or below the configured leak
     threshold per tick; failure output identifies ephemeral versus main-tier
     excess; assertions run in the coordstore soak or benchmark target without
     requiring production infrastructure.

## Dependency Graph

- `ga-gjnt7` blocks `ga-7a0x3.1`.
- `ga-7a0x3.1` blocks `ga-7a0x3.2` and `ga-7a0x3.6`.
- `ga-7a0x3.2` blocks `ga-7a0x3.3`, `ga-7a0x3.4`, and `ga-7a0x3.5`.
- `ga-7a0x3.3`, `ga-7a0x3.4`, and `ga-7a0x3.5` block `ga-7a0x3.6`.
- `ga-7a0x3.6` blocks `ga-7a0x3.7`.

## Guardrails

- Do not start implementation before `ga-gjnt7` closes.
- Keep role behavior configuration-driven; add no hardcoded role names.
- Read `specs/architecture.md` before touching `internal/events`,
  `internal/api`, generated OpenAPI, SSE, or CLI wire projections.
- Preserve typed event payload registration for every new event constant.
- Do not conflate eager-delete retention windows with the existing main-tier
  closed-task retention policy.
- Builder should record resolutions for the three open design questions from
  `ga-jb78m` before closing the affected beads.
