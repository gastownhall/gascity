# Dolt Compact Quarantine Alerts

Date: 2026-06-17

PM intake source:
- Root bead: ga-f342m1
- Source: mayor investigation from 2026-06-17

## Goal

Turn Dolt compact quarantine and stale pending-marker failures from silent
stderr-only incidents into same-day operator signals. This package covers only
notification: one operator mail plus one `dolt.compact.quarantine` event for
fresh quarantine writes, existing quarantine markers, and stale pending-push
markers.

Out of scope:
- Auto-clearing false-positive quarantines, tracked separately by ga-3pg1y6.
- `gc doctor` stale-marker and oversized-store checks, tracked separately by
  ga-qc5chv.

## Work Packages

### Regression coverage

- ga-f342m1.1 -> validator: Cover compact quarantine operator-alert cases.

Acceptance focus:
- A freshly written compact-quarantine marker emits one operator mail and one
  `dolt.compact.quarantine` event.
- Existing compact-quarantine markers in both `flatten_database` and
  `bare_gc_database` emit the same operator signals before hard-blocking.
- A stale compact-pending-push marker emits the same operator signals.
- `GC_DOLT_COMPACT_ALERT_TO` override and default recipient behavior are
  covered.
- Synthetic fixtures do not mutate real city stores.

### Alert implementation

- ga-f342m1.2 -> builder: Emit compact quarantine operator alerts.

Acceptance focus:
- `examples/bd/dolt/commands/compact/run.sh` sends one `gc event emit
  dolt.compact.quarantine` and one `gc mail send` for every required marker
  path.
- Alerts include db name, marker path/type, reason, recipient, and enough
  timing context to distinguish fresh from stale markers.
- Recipient resolution honors `GC_DOLT_COMPACT_ALERT_TO` and otherwise follows
  the existing operator-recipient convention from the stale-db escalation
  pattern.
- Quarantine and pending-marker control flow remains notification-only.

Dependency:
- ga-f342m1.2 depends on ga-f342m1.1.

### Synthetic verification

- ga-f342m1.3 -> validator: Prove compact alert delivery on synthetic markers.

Acceptance focus:
- Synthetic verification proves fresh quarantine, existing quarantine, and
  stale pending-push cases each send one operator mail plus one
  `dolt.compact.quarantine` event.
- Evidence records both default recipient behavior and
  `GC_DOLT_COMPACT_ALERT_TO` override behavior.
- Failures include command, fixture path, and missing signal so builder can
  reproduce quickly.
- Results explicitly do not claim the separate P2 auto-clear behavior.

Dependency:
- ga-f342m1.3 depends on ga-f342m1.2.

## Handoff Targets

Validator:
- ga-f342m1.1
- ga-f342m1.3

Builder:
- ga-f342m1.2

All child beads carry `source:actual-pm` plus exactly one routing label
(`needs-tests` or `ready-to-build`) and `gc.routed_to` metadata set to the
target agent.
