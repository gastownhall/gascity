# Wisp Backpressure

Gas City can cap the total number of live wisp molecules running simultaneously
across a workspace. When the cap is reached, the dispatcher defers new wisp
launches until an active slot opens.

## Why Backpressure Matters

A formula's `on_complete` fan-out can create dozens of wisps in a single tick.
Without a bound, a burst of work can exhaust API rate limits, saturate the
bead store, or starve other workloads sharing the same rig.

Backpressure answers the question "how many wisps can run at once?" at the
workspace level, independently of per-agent `max_active_sessions`.

## Configuration

In `city.toml`:

```toml
[daemon]
max_concurrent_wisps = 10   # positive integer; omit or set to 0 for unlimited
```

| Value | Effect |
|---|---|
| omitted / `nil` | Unlimited — no backpressure (default) |
| `0` | Same as omitted — unlimited |
| `N > 0` | Defers new wisp dispatch when `active >= N` |

## Semantics

- The cap is checked at dispatch time before a new wisp session is started.
- If the active count is at the cap, the dispatcher skips the new wisp for the
  current tick and retries on the next patrol tick.
- Wisps that were already running before the cap was set are not killed.
- The cap applies workspace-wide, not per-agent or per-formula.

## Helper API

`DaemonConfig` exposes two helpers for callers that enforce the cap:

```go
// WispBackpressureEnabled reports whether a cap is configured.
func (d *DaemonConfig) WispBackpressureEnabled() bool

// ShouldThrottleWisps returns true when active >= cap and dispatch
// should be deferred. Always false when backpressure is disabled.
func (d *DaemonConfig) ShouldThrottleWisps(activeCount int) bool
```

## Relationship to Other Limits

| Setting | Scope | What it limits |
|---|---|---|
| `max_active_sessions` | Workspace / rig / agent | Any session (interactive + wisp) |
| `max_wakes_per_tick` | Daemon | Sessions started per patrol tick |
| `max_concurrent_wisps` | Daemon | Live wisp molecules at any moment |

`max_concurrent_wisps` is the coarsest knob. Use it to put an absolute ceiling
on wisp concurrency regardless of how many agents or formulas are active.

## Implementation

| Artifact | Purpose |
|---|---|
| `internal/config/config.go` | `DaemonConfig.MaxConcurrentWisps` field, `WispBackpressureEnabled`, `ShouldThrottleWisps` |
| `internal/config/config_test.go` | 5 unit tests covering disabled (nil, zero), enabled, throttle boundary, and TOML parse |
| `docs/reference/config.md` | Auto-generated — updated via `go run ./cmd/genschema` |
| `docs/schema/city-schema.json` | Auto-generated — updated via `go run ./cmd/genschema` |
