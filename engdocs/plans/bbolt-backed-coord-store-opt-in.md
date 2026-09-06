# Plan: bbolt-backed coord store opt-in

> Owner: `gascity/pm` - Created: 2026-05-27
> Source design: `ga-yjlby` from `gascity/designer`
> Parent architecture: `ga-hf4so`
> Decomposed into: 1 architecture decision bead, 5 builder beads

## Context

The architect selected an opt-in bbolt-backed coordination store as a
minimal viable alternative to managed Dolt. The designer completed the CLI
operator experience for configuration, `gc doctor`, error messages, and
operator documentation. This PM slice turns that design into a dependency
graph that lets core storage work start while the one remaining policy
question is resolved.

The feature is local-use and fresh-start only. Enabling `[beads] backend =
"bbolt"` starts from an empty bbolt store; existing Dolt bead state is not
migrated. Empty backend or `backend = "dolt"` keeps existing behavior.

Tracker import was checked during PM intake. No tracker skills were present
in this session, so the import step was a no-op.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-yjlby.1` | As a builder, I can rely on a settled bbolt backend policy | `needs-architecture` | `gascity/architect` | - |
| `ga-yjlby.2` | As a controller, I can persist beads in an embedded bbolt Store | `ready-to-build` | `gascity/builder` | - |
| `ga-yjlby.3` | As an operator, I can opt into bbolt without starting dolt | `ready-to-build` | `gascity/builder` | `ga-yjlby.1`, `ga-yjlby.2` |
| `ga-yjlby.4` | As an operator, I can see the active coord-store backend in gc doctor | `ready-to-build` | `gascity/builder` | `ga-yjlby.1`, `ga-yjlby.3` |
| `ga-yjlby.5` | As an operator, I can enable and recover from bbolt using docs | `needs-docs` | `gascity/pm` | `ga-yjlby.1`, `ga-yjlby.4` |
| `ga-yjlby.6` | As a maintainer, I can verify the bbolt backend end to end | `ready-to-build` | `gascity/builder` | `ga-yjlby.2`, `ga-yjlby.3`, `ga-yjlby.4`, `ga-yjlby.5` |

## Acceptance Rollup

The parent initiative is complete when all six children are closed and these
outcomes hold:

- `BboltStore` implements the full `beads.Store` interface and passes
  `beadstest.RunStoreTests`.
- On open, bbolt state is loaded into an in-memory hot index; hot reads do
  not query bbolt.
- Every mutation writes through to bbolt before updating the in-memory index.
- `SetMetadataBatch` persists all metadata changes for one bead in a single
  bbolt transaction.
- Main beads, wisps, dependency edges, and the sequence counter persist and
  reload correctly.
- `[beads] backend = "bbolt"` activates the bbolt store, creates or opens
  `.gc/state/bbolt/beads.bolt`, and does not start managed Dolt.
- Empty backend or `backend = "dolt"` keeps existing managed-Dolt behavior.
- Unknown backend values follow the architecture decision in `ga-yjlby.1`.
- `gc doctor` reports `coord-store-backend` for default/dolt, bbolt present,
  bbolt absent, and unknown-value states.
- With bbolt active, city-level `dolt-topology` checks are not registered;
  rig-level Dolt backup checks remain unaffected.
- Operator documentation covers enablement, verification, fresh-start
  semantics, limitations, revert steps, and troubleshooting.
- Integration coverage proves a bbolt-backed city can run a representative
  bead workflow and that no managed Dolt server is spawned.

## Dependency Graph

```text
ga-yjlby.1
ga-yjlby.2
  -> ga-yjlby.3

ga-yjlby.1
ga-yjlby.3
  -> ga-yjlby.4

ga-yjlby.1
ga-yjlby.4
  -> ga-yjlby.5

ga-yjlby.2
ga-yjlby.3
ga-yjlby.4
ga-yjlby.5
  -> ga-yjlby.6
```

## Routing Rationale

`ga-yjlby.1` routes to `gascity/architect` because the designer surfaced a
policy question that affects startup behavior, doctor severity, and operator
messages. PM should not settle that technical policy in a build bead.

The remaining work routes to `gascity/builder` with `ready-to-build`. The
designer already completed the UX surface, so no child bead routes back to
design. Test requirements are included in each build slice and in the final
verification slice so the builder can follow the project's TDD workflow.

`ga-yjlby.5` was later relabelled `needs-docs` and routed back to PM because
the builder prompt excludes operator documentation and this rig has no writer
agent configured. PM owns that documentation-only slice at
`docs/coordination-store/bbolt-opt-in.md`; the final verification slice
remains routed to `gascity/builder`.

## Risks

- Silent fallback on an unknown backend value would mask operator typos. The
  decision bead must make the startup policy explicit before lifecycle and
  doctor work ship.
- bbolt is single-process. Lock-conflict errors need actionable hints and
  deterministic timeout behavior so operators can recover safely.
- The implementation must not start or monitor managed Dolt when bbolt is
  active.
- The bbolt file is local-only state. Documentation must make the lack of
  migration, sync, and Dolt backup automation explicit.
- Doctor output must use existing typed/structured pathways; no hand-written
  JSON or untyped wire payloads should be introduced.

## Out of Scope

- Migrating existing Dolt bead state into bbolt.
- Production multi-user or replicated bbolt deployment.
- New coord-store config sections beyond `[beads] backend`.
- New API or SSE wire types.
- Changes to `CachingStore` beyond wrapping the new backing store.

## Validation Gates

- `go test ./internal/beads/... -count=1`
- `go test ./cmd/gc/... ./internal/doctor/... -count=1`
- Integration test for `test/acceptance/bbolt_backend_test.go` with the
  integration build tag.
- `go test ./...`
- `go vet ./...`
- No hardcoded role names in Go source.
