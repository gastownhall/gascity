# Dolt Compact Doctor Backstop

Date: 2026-06-17

PM intake source:
- Root bead: ga-qc5chv
- Source: mayor investigation from 2026-06-17

## Goal

Make `gc doctor` surface stale Dolt compact markers and oversized managed
stores so compaction failures are visible even if an alert path is missed.
This is an independent backstop to the operator-alert work in ga-f342m1.

Out of scope:
- Emitting mail/events from the compact script, tracked by ga-f342m1.
- Auto-clearing false-positive quarantines, tracked by ga-3pg1y6.

## Work Packages

### Regression coverage

- ga-qc5chv.1 -> validator: Cover Dolt compact state diagnostics.

Acceptance focus:
- Stale fixtures under `compact-quarantine`, `compact-pending-gc`, and
  `compact-pending-push` produce WARN/FAIL results with db name, marker
  type/path, reason, timestamp or age context, and non-empty `FixHint`.
- A clean fixture reports OK.
- The managed-store size heuristic is covered above and below the
  maintenance-overdue threshold.
- Tests use isolated fixtures and do not depend on live city stores.

### Doctor implementation

- ga-qc5chv.2 -> builder: Report Dolt compact markers and oversized stores.

Acceptance focus:
- `gc doctor` registers a Dolt compact-state check alongside the existing Dolt
  checks.
- The check reports stale markers under `compact-quarantine`,
  `compact-pending-gc`, and `compact-pending-push`.
- The check reports managed `.dolt` store bloat using the size-to-live-row
  heuristic from the root bead.
- The check reports OK when clean and follows existing Dolt-check skip behavior
  when managed Dolt is intentionally unavailable.

Dependency:
- ga-qc5chv.2 depends on ga-qc5chv.1.

### CLI verification

- ga-qc5chv.3 -> validator: Verify compact-state signals in `gc doctor`.

Acceptance focus:
- Synthetic stale quarantine, pending-gc, and pending-push markers are visible
  in user-facing `gc doctor` output.
- Synthetic oversized managed stores are visible with DB and size condition.
- Clean synthetic layout is green.
- Validation records whether the root's current stale pending-push examples
  would be caught by the fixture logic without mutating live stores.

Dependency:
- ga-qc5chv.3 depends on ga-qc5chv.2.

## Handoff Targets

Validator:
- ga-qc5chv.1
- ga-qc5chv.3

Builder:
- ga-qc5chv.2

All child beads carry `source:actual-pm` plus exactly one routing label
(`needs-tests` or `ready-to-build`) and `gc.routed_to` metadata set to the
target agent.
