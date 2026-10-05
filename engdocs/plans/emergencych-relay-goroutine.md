# EmergencyCh Relay Goroutine Plan

Root bead: `ga-3xpjd`

Source design: `ga-3xpjd`, derived from architecture bead `ga-c8po6`.

## Goal

Emergency records delivered to the running controller over the existing socket path should be relayed from `controllerState.emergencyCh` into the controller's event recorder, so live SSE/event consumers see the controller-sequenced `emergency.signaled` event path without changing the emergency spool, socket protocol, HTTP API, OpenAPI schema, or dashboard types.

## Scope

The design is backend-only. The builder should add the relay consumer and one `runController` call site. The validator should establish focused tests first and verify the finished behavior after implementation.

Already provided by the prior builder branch:

| Existing piece | Location |
| --- | --- |
| Emergency record type and `RecordSignaled` helper | `internal/emergency/emergency.go` |
| `events.EmergencySignaled` registration path | `internal/events/`, `internal/api/event_payloads.go` |
| Controller emergency channel field and socket producer | `cmd/gc/api_state.go`, `cmd/gc/controller.go` |
| Existing emergency socket fanout regression coverage | `cmd/gc/cmd_emergency_controller_test.go` |

Out of scope:

- New API endpoints, OpenAPI changes, dashboard generated type changes, or socket protocol changes.
- Rewriting the emergency producer path.
- Any `WriteSpool` call inside the controller relay.
- Any `pokeCh` coupling for emergency records.

## Work Packages

| Bead | Route | Purpose | Acceptance summary |
| --- | --- | --- | --- |
| `ga-3xpjd.1` | `gascity/validator` | Define relay behavior with tests | Successful relay records `events.EmergencySignaled`; cancellation exits cleanly; nil `eventProv` is no-op; optional nil `emergencyCh` no-op. |
| `ga-3xpjd.2` | `gascity/builder` | Implement the controller relay | Add `controllerState.startEmergencyEventRelay(ctx)` and call it from `runController` after `cs.startBeadEventWatcher(ctx)`. Preserve no-spool, no-poke, no-schema-change invariants. |
| `ga-3xpjd.3` | `gascity/validator` | Verify regression and API stability | Run targeted relay and existing fanout tests, confirm no schema/dashboard generated drift, and record validation results. |

## Dependency Graph

`ga-3xpjd.1` blocks `ga-3xpjd.2`.

`ga-3xpjd.2` blocks `ga-3xpjd.3`.

This preserves TDD sequencing: tests first, implementation second, verification last.

## Handoff Notes

The builder should treat `ga-3xpjd` as the pinned technical design. The expected implementation is intentionally small:

1. Add `startEmergencyEventRelay(ctx context.Context)` on `controllerState`.
2. Return early when `emergencyCh` or `eventProv` is nil.
3. In the goroutine, select on `ctx.Done()` and `cs.emergencyCh`.
4. For each record, call `emergency.RecordSignaled(cs.eventProv, rec)` and log errors without stopping the relay.
5. Call the method from `runController` after `cs.startBeadEventWatcher(ctx)`.

The post-build validator should confirm the duplicate-online event path remains NDI-compliant because the emergency spool ID is the dedupe key.
