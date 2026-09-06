# PM Plan: gc doctor named-always-min-conflict

Root bead: ga-ihrikr
Source: gascity/designer handoff for architecture bead ga-4mubc9
Date: 2026-05-29

## Goal

Warn operators when a non-suspended agent both backs a `[[named_session]]`
with `mode="always"` and has `min_active_sessions > 0`, because that valid
configuration can create an unexpected duplicate pool session.

The check is advisory. It must not change reconciler behavior, startup
behavior, session spawning, or any user-configured role behavior.

## Resolved Inputs

- File placement: create `internal/doctor/checks_named_session.go`.
- Severity: `StatusWarning` with `SeverityAdvisory`.
- Documentation scope: Go comments only, no new user-facing docs page.
- Warm-up scope: not part of `gc start` warm-up checks.

## Work Packages

### ga-ihrikr.1 - Doctor Check And Unit Coverage

Route: `gascity/builder`
Label: `ready-to-build`

Acceptance:

- A named-always/min_active_sessions doctor check exists for the design in
  ga-ihrikr.
- OK cases cover no named sessions, `mode="on_demand"` with
  `min_active_sessions > 0`, `mode="always"` with `min_active_sessions=0`,
  and suspended agents.
- Warning cases cover `min_active_sessions=1`, `min_active_sessions=2`, and
  two simultaneous conflicting agents.
- Warning details include the qualified agent, min value, named session, and
  corrective instruction.
- The implementation preserves zero hardcoded roles and does not change
  reconciler/session spawning behavior.

### ga-ihrikr.2 - Doctor Lifecycle Wiring

Route: `gascity/builder`
Label: `ready-to-build`
Blocked by: ga-ihrikr.1

Acceptance:

- `gc doctor` registers `named-always-min-conflict` only when city config
  loads successfully.
- `WarmupEligible()` returns false using the established doctor pattern.
- Existing doctor checks keep their current order except for the new check in
  the configured city-check block from ga-ihrikr.

### ga-ihrikr.3 - Config Comment Scope

Route: `gascity/builder`
Label: `ready-to-build`

Acceptance:

- `internal/config/config.go` explains that pool minimums and
  `[[named_session]] mode="always"` are independent.
- The comment update is limited to the `MinActiveSessions` and named-session
  `Mode` fields.
- No separate user-facing docs page is added.

### ga-ihrikr.4 - Final Verification

Route: `gascity/builder`
Label: `ready-to-build`
Blocked by: ga-ihrikr.1, ga-ihrikr.2, ga-ihrikr.3

Acceptance:

- Focused doctor package tests pass.
- `go test ./...` passes.
- `go vet ./...` passes.
- The completed change still has no role-name-specific Go logic.

## Dependency Graph

```text
ga-ihrikr.1
  -> ga-ihrikr.2
ga-ihrikr.3
ga-ihrikr.1, ga-ihrikr.2, ga-ihrikr.3
  -> ga-ihrikr.4
```

## Handoff Notes

All child beads are builder-ready because the designer resolved the remaining
design questions. No additional UX/design loop is needed. Tracker import was a
no-op in this worktree because no tracker conversion skill was installed.
