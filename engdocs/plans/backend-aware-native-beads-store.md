# Plan: backend-aware native beads store

> Owner: `gascity/pm` - Created: 2026-05-14
> Source design: `ga-l2souo` from `gascity/designer`
> Parent architecture: `ga-o92gf7`
> Preflight prerequisite: `ga-g1v9yd`
> Decomposed into: 6 builder beads

## Context

The current provider path has scattered backend checks in and around
`cmd/gc/providers.go`. The design calls for one typed function-style store
factory that decides whether native Dolt storage is safe, falls back to
BdStore with structured diagnostics when it is not, and keeps FileStore
behavior intact for non-bd providers.

This plan follows the designer's order while keeping independently shippable
work available early. The caching-store and native-store adapter work can
start before the factory gates, while the gate-selection work waits for the
preflight checker from `ga-g1v9yd`.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-l2souo.1` | As a controller, I can cache any beads Store implementation | `ready-to-build` | `gascity/builder` | - |
| `ga-l2souo.2` | As a controller, I can use a native Dolt-backed beads store | `ready-to-build` | `gascity/builder` | - |
| `ga-l2souo.3` | As an observer, I receive typed bead events from native store writes | `ready-to-build` | `gascity/builder` | `ga-l2souo.2` |
| `ga-l2souo.4` | As an operator, I get safe fallback when native store gates fail | `ready-to-build` | `gascity/builder` | `ga-g1v9yd.2`, `ga-l2souo.1`, `ga-l2souo.2` |
| `ga-l2souo.5` | As a CLI user, I see native-store diagnostics through provider and status wiring | `ready-to-build` | `gascity/builder` | `ga-l2souo.4` |
| `ga-l2souo.6` | As a maintainer, I can verify native-store selection end to end | `ready-to-build` | `gascity/builder` | `ga-l2souo.3`, `ga-l2souo.5`, `ga-g1v9yd.4` |

## Acceptance Rollup

The parent is complete when all six children are closed and the following
outcomes hold:

- `CachingStore` delegates to any existing `beads.Store` implementation
  without type assertions or BdStore-only assumptions.
- `NativeDoltStore` delegates to the upstream beads library over Dolt, returns
  write errors directly, and does not silently fall back to bd CLI.
- Native write paths emit existing typed bead events only after successful
  storage writes, and every emitted event has registered payload coverage.
- `internal/beads/factory.go` contains the single function-style store factory;
  no speculative factory interface is introduced.
- The factory evaluates gates in the designed order and returns native store
  only when all gates pass.
- Fallback reasons are surfaced through structured logs and diagnostics with
  redacted secrets.
- `cmd/gc/providers.go` remains the caller-facing shim and delegates store
  selection to the factory.
- `gc status --format json` includes the designed beads diagnostic fields.

## Dependency Graph

```text
ga-l2souo.2 -> ga-l2souo.3

ga-g1v9yd.2
ga-l2souo.1
ga-l2souo.2
  -> ga-l2souo.4
      -> ga-l2souo.5

ga-l2souo.3
ga-l2souo.5
ga-g1v9yd.4
  -> ga-l2souo.6
```

## Routing Rationale

All child beads route to `gascity/builder` with `ready-to-build`. The
designer already resolved the component layout, gate order, operator escape
hatch, fallback surfacing, event emission contract, and exclusions. There is
no remaining UI design or architecture decision in this slice.

## Risks

- The factory must use the preflight checker result, not reimplement drift
  detection in `cmd/gc/`.
- `GC_BEADS_FORCE_FALLBACK=1` is a safe fallback escape hatch only. There is
  intentionally no force-enable variable for native mode.
- Logs are operator-facing diagnostics. They must be structured and must not
  include DSN passwords or other redacted secrets.
- Event emission must remain typed. Do not add new event types unless the
  event registry and payload tests are updated.

## Out of Scope

- Postgres native store support.
- Embedded Dolt multi-writer safety.
- `bd schema migrate` integration.
- `gc beads preflight --fix`.
- A new top-level `[backend]` config section.
- New hardcoded agent roles or controller dependence on a configured user
  agent.

## Validation Gates

- `go test ./internal/beads/... -count=1`
- `go test ./cmd/gc/... ./internal/events/... -count=1`
- `go vet ./...`
- `TestEveryKnownEventTypeHasRegisteredPayload` remains green.
- Typed wire constraints remain intact.
- No hardcoded role names in Go source.
