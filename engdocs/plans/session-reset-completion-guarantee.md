# Session Reset Completion Guarantee Plan

Source bead: `ga-8x3f5g`
Architecture parent: `ga-doxjxg`
Design reviewer: `gascity/designer`
Priority: P2

## Goal

Make `gc session reset` of a live session complete reliably after the
intentional two-tick alias-race guard, and surface reset stalls with a
typed event, trace decision, and operator-readable stderr message.

## Work Packages

1. `ga-8x3f5g.1` - Builder: persist reset_committed_at on restart request
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `RestartRequestPatch` takes `now time.Time` and records
     `reset_committed_at` as UTC RFC3339 via one `resetCommittedAtKey`
     constant in `internal/session/lifecycle_transition.go`; callers pass
     `clk.Now()`; `lifecycle_transition.go` does not import
     `internal/clock`; `PreWakePatch` does not write, clear, or overwrite
     `reset_committed_at`; tests cover timestamp formatting and forensic
     preservation.

2. `ga-8x3f5g.2` - Builder: force reset-pending sessions into the awake set
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `compute_awake_set` adds sessions with
     `continuation_reset_pending` set and `restart_requested` empty to
     `desired` with reason `reset-pending`; the new block remains inside
     the existing `!WaitHold` guard; `on_demand` sessions with no pool
     demand are force-woken; `WaitHold` sessions are not force-woken; tests
     use configured agent names and no hardcoded role names.

3. `ga-8x3f5g.3` - Builder: register the SessionResetStalled event contract
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `events.SessionResetStalled` is added to
     `KnownEventTypes`; `SessionResetStalledPayload` lives in
     `internal/events/payloads.go` with `SessionName`, `Template`,
     `ResetCommittedAt`, and `ElapsedSeconds` fields;
     `internal/api/event_payloads.go` registers the payload immediately
     after `SessionStranded`; generated OpenAPI and dashboard TS types are
     updated via `make dashboard-check`;
     `TestEveryKnownEventTypeHasRegisteredPayload` passes.

4. `ga-8x3f5g.4` - Builder: emit reset stall watchdog diagnostics
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-8x3f5g.1`, `ga-8x3f5g.3`
   - Acceptance: the reconciler detects `continuation_reset_pending` with
     `reset_committed_at` older than `startup_timeout` while the target is
     not alive; it emits a `SessionResetStalled` typed event, trace
     decision, and stderr line; `Event.Message` matches stderr and includes
     session name, elapsed seconds, `reset_committed_at`, and bead ID;
     payload includes `Template`; debounce uses an in-memory per-bead set
     cleared when reset pending resolves; no metadata throttle key is added.

5. `ga-8x3f5g.5` - Builder: surface clearRestartRequested errors and verify reset gates
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-8x3f5g.1`, `ga-8x3f5g.2`, `ga-8x3f5g.3`,
     `ga-8x3f5g.4`
   - Acceptance: the swallowed `clearRestartRequested` error is replaced
     with contextual stderr output except for gone errors; `runtime.Fake`
     supports a per-key `RemoveMeta` error seam;
     `TestReconcileSessionBeads_ResetOfLiveSessionIsNotAtomicWithinTick`
     asserts tick 2 produces desired reason `reset-pending` when pool demand
     is zero and `on_demand` mode is active; the yield at
     `session_reconciler.go:1465-1468` is unchanged; `go test ./...`,
     `go vet ./...`, and `make dashboard-check` pass.

## Dependency Graph

- `ga-8x3f5g.1` and `ga-8x3f5g.3` block `ga-8x3f5g.4`.
- `ga-8x3f5g.1`, `ga-8x3f5g.2`, `ga-8x3f5g.3`, and `ga-8x3f5g.4`
  block `ga-8x3f5g.5`.

## Guardrails

- Do not remove or move the yield at `session_reconciler.go:1465-1468`.
- Do not make stop and restart atomic in one reconciler tick.
- Do not add `reset_committed_at` to `PreWakePatch`; it is a forensic
  marker from the stop-commit tick.
- Keep the reset-pending force-wake logic inside the `!WaitHold` guard.
- `SessionResetStalled` must have a registered typed payload and generated
  dashboard/API artifacts must stay in sync.
- No hardcoded role names in code or tests.
