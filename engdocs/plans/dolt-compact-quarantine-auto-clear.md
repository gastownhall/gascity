# Dolt Compact Quarantine Auto-Clear

Date: 2026-06-17

PM intake source:
- Root bead: ga-3pg1y6
- Source: mayor investigation from 2026-06-17

## Goal

Allow the Dolt compact path to self-heal a known false-positive quarantine on
the next compact pass when current HEAD proves preservation. The target incident
class is the same-count table-hash drift race from the 2026-06-10 hq blackout.

Out of scope:
- Initial operator alert plumbing, tracked by ga-f342m1.
- `gc doctor` marker and store-size diagnostics, tracked by ga-qc5chv.

## Work Packages

### Regression coverage

- ga-3pg1y6.1 -> validator: Cover compact quarantine false-positive
  auto-clear.

Acceptance focus:
- A synthetic same-count table-hash-drift marker with a known race-class
  reason is cleared on the next compact only when current-HEAD preservation
  probes pass.
- A HEAD-advanced case is covered using the root bead's accepted condition.
- Probe failure, table-list change, decrease-without-proven-writer, and
  unknown marker reasons continue to hard-block.
- Auto-clear and hard-block paths assert use of the P1 alert mechanism once it
  is available.
- Fixtures are synthetic and isolated.

### Auto-clear implementation

- ga-3pg1y6.2 -> builder: Auto-clear proven false-positive compact
  quarantines.

Acceptance focus:
- The existing-marker check in `flatten_database` re-runs the current-HEAD
  preservation probes before deciding whether to keep blocking.
- Known same-count table-hash drift race markers clear only when preservation
  is proven or the accepted HEAD-advanced condition applies.
- Cleared markers emit the P1 operator alert path.
- Unprovable classes keep hard-blocking and emit the P1 alert path.
- Scope stays in the Dolt compact pack script/proof helper surface.

Dependencies:
- ga-3pg1y6.2 depends on ga-3pg1y6.1.
- ga-3pg1y6.2 depends on ga-f342m1.3, the P1 alert delivery validation.

### Synthetic verification

- ga-3pg1y6.3 -> validator: Prove quarantine auto-clear and hard-block
  behavior.

Acceptance focus:
- Synthetic same-count table-hash-drift marker is auto-cleared and compact
  proceeds when preservation is proven.
- Synthetic hard-block marker remains blocked and is not removed.
- Both paths include expected operator alert evidence.
- Evidence records marker before/after state, compact exit behavior, alert
  recipient, and event/mail signals.
- Results do not claim the separate `gc doctor` behavior.

Dependency:
- ga-3pg1y6.3 depends on ga-3pg1y6.2.

## Handoff Targets

Validator:
- ga-3pg1y6.1
- ga-3pg1y6.3

Builder:
- ga-3pg1y6.2

All child beads carry `source:actual-pm` plus exactly one routing label
(`needs-tests` or `ready-to-build`) and `gc.routed_to` metadata set to the
target agent. The builder package is explicitly blocked on P1 alert validation
so auto-clear does not land before operator alert delivery is proven.
