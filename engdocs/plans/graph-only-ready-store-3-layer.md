# GraphOnlyReadyStore 3-Layer PM Plan

Owner: `gascity/pm`
Created: 2026-07-01
Root bead: `ga-ifavnc`
Parent: `ga-q7opb1`
Source: designer handoff `source:actual-designer`
Target: `gascity/builder`

## Goal

Activate the production `GraphOnlyReadyStore` capability chain so controller
demand reads can use the graph-only ready path instead of falling back to the
federated `Live.Ready()` query.

The completed design validates the existing API contract and requires three
store layers:

- `DoltliteReadStore` is the concrete graph-only ready reader.
- `CachingStore` propagates the capability without using the normal ready
  cache.
- `beadPolicyStore` preserves policy query expansion before delegation.

Tracker import was a no-op in this session because no visible
`tracker-to-beads` or sibling tracker skill was installed.

## Work Packages

| Bead | Story | Route | Depends on |
| --- | --- | --- | --- |
| `ga-ifavnc.1` | As a maintainer, I can lock GraphOnlyReadyStore behavior with contract coverage | `ready-to-build` -> `gascity/builder` | none |
| `ga-ifavnc.2` | As a controller, I can read graph-only ready work from DoltliteReadStore | `ready-to-build` -> `gascity/builder` | `ga-ifavnc.1` |
| `ga-ifavnc.3` | As a controller, I can reach graph-only ready through CachingStore | `ready-to-build` -> `gascity/builder` | `ga-ifavnc.2` |
| `ga-ifavnc.4` | As a controller, I can preserve bead policy expansion on graph-only ready reads | `ready-to-build` -> `gascity/builder` | `ga-ifavnc.3` |

All child beads are labeled `ready-to-build` and `source:actual-pm`, with
`gc.routed_to` set to `gascity/builder`. No design hop is needed because the
source bead is a completed designer handoff.

## Acceptance Rollup

The package is complete when:

- Focused GraphOnlyReadyStore tests cover the DoltliteReadStore, CachingStore,
  and beadPolicyStore layers.
- `DoltliteReadStore.ReadyGraphOnly` exists under the `gascity_native_beads`
  build tag and unconditionally forces `TierMode = TierWisps` before
  delegating to the existing `Ready` path.
- `CachingStore.ReadyGraphOnlyHandle` exposes graph-only ready only when its
  backing store supports it, returns `(nil, false)` otherwise, and bypasses
  `cachedReadyOnly`.
- `beadPolicyStore.ReadyGraphOnlyHandle` wraps supported stores, applies
  `expandPolicyReadyQuery(query...)`, and delegates without changing error or
  result semantics.
- The TierMode override invariant is preserved: caller-supplied `TierBoth` or
  `TierIssues` cannot make graph-only ready return issue-tier beads.
- Store-layer methods remain thin: no new SQL path, no new logging, and no new
  caching behavior.
- `go build -tags gascity_native_beads ./...` passes.
- `go test -tags gascity_native_beads ./internal/beads/... ./cmd/gc/...`
  passes.
- `go vet -tags gascity_native_beads ./...` passes.
- Existing `build_desired_state_graph_ready_test.go` tests still pass
  unchanged.
- `make test-fast-parallel` passes.

## Dependency Order

```text
ga-ifavnc.1
  -> ga-ifavnc.2
      -> ga-ifavnc.3
          -> ga-ifavnc.4
```

This keeps the TDD path explicit: contract coverage first, concrete
DoltliteReadStore capability second, CachingStore propagation third, and policy
wrapper completion plus full package gates last.

## Guardrails

- Preserve the interface in `internal/beads/ready_graph_only.go`.
- Do not weaken the graph-only contract by honoring caller TierMode over
  `TierWisps`.
- Do not cache `ReadyGraphOnly` results in `CachingStore`.
- Do not log in the new store-layer methods.
- Do not route this work back to design.
- Keep the work scoped to the three named layers and their focused tests unless
  a downstream bead records a directly necessary adjacent change.
