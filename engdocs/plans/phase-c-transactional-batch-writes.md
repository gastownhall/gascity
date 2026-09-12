# Plan: Phase C transactional batch writes

> Owner: `gascity/pm` - Created: 2026-05-15
> Source design: `ga-xqsgb2` from `gascity/designer`
> Architecture ref: `ga-iufuk7`
> Implements against: `ga-l2souo`
> Decomposed into: 5 builder beads

## Context

The design converts session bead lifecycle write pairs from sequential writes
into a single `Store.Tx` callback shape. Native Dolt-backed stores use the
upstream beads transaction primitive; existing BdStore, MemStore, and
FileStore paths keep their current sequential behavior. This is the required
foundation before Phase A can migrate ephemeral lifecycle metadata into
clone-local storage.

Tracker import was a no-op in this session because no visible tracker skill
was installed.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-xqsgb2.1` | As a controller, I can run bead writes through a minimal Store.Tx contract | `ready-to-build` | `gascity/builder` | - |
| `ga-xqsgb2.2` | As a controller, I can execute NativeDoltStore writes inside beadslib transactions | `ready-to-build` | `gascity/builder` | `ga-xqsgb2.1`, `ga-l2souo.2` |
| `ga-xqsgb2.3` | As a controller, I can keep cached bead state coherent after transactional writes | `ready-to-build` | `gascity/builder` | `ga-xqsgb2.1`, `ga-l2souo.1` |
| `ga-xqsgb2.4` | As a session maintainer, I can close and reopen bead lifecycle state with transactional write pairs | `ready-to-build` | `gascity/builder` | `ga-xqsgb2.1` |
| `ga-xqsgb2.5` | As a maintainer, I can verify transactional bead writes across stores and session call sites | `ready-to-build` | `gascity/builder` | `ga-xqsgb2.2`, `ga-xqsgb2.3`, `ga-xqsgb2.4` |

## Acceptance Rollup

The parent is complete when all five children are closed and the following
outcomes hold:

- `beads.Store` exposes the minimal transaction callback surface designed in
  `ga-xqsgb2`, and `beads.Tx` includes only the three required methods.
- Non-native stores satisfy the transaction contract without claiming new
  atomicity guarantees.
- NativeDoltStore delegates to beadslib transactions through one adapter
  layer and retries only serialization conflicts with the designed backoffs.
- CachingStore invalidates touched bead cache state after successful
  transactional writes and leaves cache state unchanged after failed writes.
- `closeBead`, `rollbackPendingCreate`, and
  `reopenClosedConfiguredNamedSessionBead` follow the designed transaction
  boundaries and error surfaces.
- Store conformance, session call-site regressions, retry coverage, lock-window
  coverage, and native-store performance checks are present or replaced by
  equivalent focused tests matched to the final implementation.

## Dependency Graph

```text
ga-xqsgb2.1
  -> ga-xqsgb2.2
  -> ga-xqsgb2.3
  -> ga-xqsgb2.4

ga-l2souo.2 -> ga-xqsgb2.2
ga-l2souo.1 -> ga-xqsgb2.3

ga-xqsgb2.2
ga-xqsgb2.3
ga-xqsgb2.4
  -> ga-xqsgb2.5

ga-xqsgb2.5 -> ga-dvvsla.1
```

## Routing Rationale

All child beads route to `gascity/builder` with `ready-to-build`. The design
already specifies the transaction interface, fallback-store behavior,
NativeDoltStore adapter boundary, cache invalidation strategy, call-site
conversions, and verification gates. There is no remaining PM action for
design, architecture, or validator-only routing.

## Risks

- NativeDoltStore depends on the exact beadslib transaction API. Builders must
  validate the upstream signatures instead of copying the pseudo-code blindly.
- Non-native stores must not be presented as newly atomic. Their Tx method is a
  compatibility surface that preserves current sequential behavior.
- Identifier locks around reopen must not expand to include unrelated work.
  The design keeps availability checks and the transaction inside the lock.
- CachingStore must not call backing stores while holding the cache mutex.

## Out of Scope

- New store primitives beyond the minimal Tx callback.
- Postgres native store support.
- Automated repair of backend drift.
- New hardcoded roles or controller dependence on a configured user agent.

## Validation Gates

- `go test ./internal/beads/... -count=1`
- `go test ./cmd/gc/... -count=1`
- `go test ./...`
- `go vet ./...`
- No new hardcoded role names in Go source.
