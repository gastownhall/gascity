# Plan: Phase A local metadata abstraction

> Owner: `gascity/pm` - Created: 2026-05-15
> Source design: `ga-dvvsla` from `gascity/designer`
> Architecture ref: `ga-wrxxrj`
> Implements against: `ga-l2souo`
> Prerequisite: `ga-xqsgb2.5`
> Decomposed into: 6 builder beads

## Context

The design moves ephemeral lifecycle metadata out of durable bead metadata
when the backing store supports clone-local storage. The new Store methods
provide a fast local path for NativeDoltStore and a clear sentinel fallback for
BdStore, FileStore, and MemStore. Session call sites continue to work on every
store by falling back to durable metadata only when clone-local metadata is
unsupported.

Tracker import was a no-op in this session because no visible tracker skill
was installed.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-dvvsla.1` | As a controller, I can address clone-local bead metadata through the Store contract | `ready-to-build` | `gascity/builder` | `ga-xqsgb2.5` |
| `ga-dvvsla.2` | As a controller, I can store clone-local bead metadata in NativeDoltStore without polluting Dolt history | `ready-to-build` | `gascity/builder` | `ga-dvvsla.1`, `ga-l2souo.2` |
| `ga-dvvsla.3` | As a controller, I can cache clone-local metadata without stale bead entries | `ready-to-build` | `gascity/builder` | `ga-dvvsla.1`, `ga-xqsgb2.3` |
| `ga-dvvsla.4` | As a session maintainer, I can keep ephemeral lifecycle keys clone-local when the store supports it | `ready-to-build` | `gascity/builder` | `ga-dvvsla.1`, `ga-xqsgb2.4` |
| `ga-dvvsla.5` | As a controller, I can migrate existing ephemeral lifecycle keys into clone-local metadata once | `ready-to-build` | `gascity/builder` | `ga-dvvsla.2`, `ga-dvvsla.4`, `ga-xqsgb2.2` |
| `ga-dvvsla.6` | As a maintainer, I can verify local metadata behavior and migration safety | `ready-to-build` | `gascity/builder` | `ga-dvvsla.3`, `ga-dvvsla.4`, `ga-dvvsla.5` |

## Acceptance Rollup

The parent is complete when all six children are closed and the following
outcomes hold:

- `ErrLocalMetadataNotSupported`, `SetLocalString`, and `GetLocalString` are
  available through the Store contract with conformance coverage.
- Non-native stores return the sentinel consistently and preserve durable
  fallback behavior.
- NativeDoltStore stores local values under `gc:bead:{beadID}:{key}` and
  translates absent local keys to `(value="", ok=false, err=nil)`.
- CachingStore caches local metadata only after successful backing writes and
  evicts local metadata alongside bead cache eviction.
- `synced_at`, `last_woke_at`, and `pending_create_claim` use a shared
  local-or-durable read/write pattern.
- The one-time migration copies existing durable ephemeral keys into
  clone-local metadata, clears durable values transactionally, handles
  per-bead errors, and writes the migration marker after all eligible beads
  are attempted.

## Dependency Graph

```text
ga-xqsgb2.5
  -> ga-dvvsla.1
      -> ga-dvvsla.2
      -> ga-dvvsla.3
      -> ga-dvvsla.4

ga-l2souo.2 -> ga-dvvsla.2
ga-xqsgb2.3 -> ga-dvvsla.3
ga-xqsgb2.4 -> ga-dvvsla.4

ga-dvvsla.2
ga-dvvsla.4
ga-xqsgb2.2
  -> ga-dvvsla.5

ga-dvvsla.3
ga-dvvsla.4
ga-dvvsla.5
  -> ga-dvvsla.6
```

## Routing Rationale

All child beads route to `gascity/builder` with `ready-to-build`. The design
already resolves the storage contract, namespace, sentinel fallback, cache
behavior, session call-site pattern, migration sequencing, and verification
gates. The Phase A children are blocked behind Phase C because the migration
and cache behavior depend on the Store.Tx foundation.

## Risks

- The upstream beadslib local metadata API must be confirmed before coding the
  NativeDoltStore methods.
- `SetLocalString` must not become hidden durable metadata. The local key must
  stay out of Dolt commit history.
- General call sites must not write local metadata inside Store.Tx callbacks.
  The migration is the only designed exception.
- Migration error handling must be observable without aborting safe fallback
  behavior.

## Out of Scope

- Migrating arbitrary user metadata.
- New lifecycle heuristics or controller decision logic.
- Postgres native store support.
- UI or API wire changes.

## Validation Gates

- `go test ./internal/beads/... -count=1`
- `go test ./cmd/gc/... -count=1`
- `go test ./...`
- `go vet ./...`
- No new hardcoded role names in Go source.
