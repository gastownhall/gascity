# Worktree Reaper Typed Events Plan

Root bead: `ga-plhh3l`
Architecture source: `ga-uq92xy`
Design source: `gascity/designer`, completed 2026-06-02

## Goal

Add typed event constants and payload registrations for closed-bead worktree
reaper outcomes. The events provide observability for successful reaps and
safety-gate skips without adding hand-written JSON wire paths or role-specific
logic.

## Child Beads

| Order | Bead | Route | Title |
| --- | --- | --- | --- |
| 1 | `ga-plhh3l.1` | `gascity/builder` | As an operator, worktree reaper outcomes emit typed registered events |
| 2 | `ga-plhh3l.2` | `gascity/validator` | Validate typed worktree reaper events and payload registration |

Dependency graph:

- `ga-plhh3l.2` depends on `ga-plhh3l.1`
- `ga-xxsd7k.1` depends on `ga-plhh3l.1` because runtime wiring must match the
  final reaper event-emission signature.

## Acceptance Summary

The work is complete when:

- `BeadWorktreeReaped` has wire value `bead.worktree.reaped`.
- `BeadWorktreeReapSkipped` has wire value `bead.worktree.reap_skipped`.
- Both constants are included in `events.KnownEventTypes`.
- Typed payload structs live in `internal/events`, implement `IsEventPayload`,
  and self-register with `events.RegisterPayload`.
- Mandatory JSON fields match the design exactly.
- `TestEveryKnownEventTypeHasRegisteredPayload` passes.

## Out Of Scope

- No registration move into `internal/api/event_payloads.go` unless existing
  tests force it and the reason is documented.
- No new SDK decision logic based on event payload contents.
- No hardcoded agent role names.

## Risks

- Adding constants without payload registration fails CI.
- Parallel work on the runtime call site can drift if it starts before the
  event-emission signature is settled.
