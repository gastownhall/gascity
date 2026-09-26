# Plan: rig-aware convergence loops Option A (`ga-ecdms`)

> PM owner: `gascity/pm`
> Source: `ga-ecdms`
> Origin: architecture and designer handoff from `ga-6dy55`

## Goal

Convergence loops should be able to live in either the city bead store or a rig
bead store. Operators should see all loops by default, narrow with `--rig` when
needed, and operate on existing loops by bead ID without remembering which store
owns the loop.

## Context

This is the planned feature that follows the immediate Option B fix. The source
design resolves the open questions: list aggregates by default, handlers are
initialized eagerly and rebuilt on config reload, and `convergence.created`
events carry an optional `store_key` for rig-store loops.

The builder must read `specs/architecture.md` before touching `cmd/gc`,
`internal/events`, generated schema surfaces, or API-adjacent code. The
implementation must preserve the single-store `convergenceStoreAdapter`
invariant; multi-store orchestration belongs in `CityRuntime`.

Because the source includes a completed design handoff, all child beads are
build-ready and routed to `gascity/builder`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-ecdms.1` | Build convergence handler maps for city and rig stores | `ready-to-build` -> `gascity/builder` | `ga-6qnps.3` |
| `ga-ecdms.2` | Route convergence controller operations across stores | `ready-to-build` -> `gascity/builder` | `ga-ecdms.1` |
| `ga-ecdms.3` | Expose rig-aware gc converge CLI behavior | `ready-to-build` -> `gascity/builder` | `ga-ecdms.2` |
| `ga-ecdms.4` | Emit convergence.created store_key for rig loops | `ready-to-build` -> `gascity/builder` | `ga-ecdms.2` |
| `ga-ecdms.5` | Complete rig-aware convergence verification matrix | `ready-to-build` -> `gascity/builder` | `ga-ecdms.3`, `ga-ecdms.4` |

## Acceptance: `ga-ecdms.1`

1. `CityRuntime` has map-keyed convergence handlers and adapters using empty
   string for the city store and rig name for rig stores.
2. Startup initialization eagerly wires city plus configured rig stores.
3. Config reload rebuilds the handler map through the same initialization path.
4. A failing rig-store adapter initialization is logged and skipped without
   aborting city or other rig handlers.
5. Unit coverage proves city plus rig handlers are built.

## Acceptance: `ga-ecdms.2`

1. `handleConvergenceCreate` reads the requested rig name from request params
   and routes to the matching handler.
2. Unknown or unavailable rig create requests return the configured
   rig-not-available error.
3. `approve`, `iterate`, and `stop` locate existing loop beads by searching
   handlers, city first then rigs.
4. `convergenceTick` iterates active bead IDs across all adapters without
   adding cross-handler locking.
5. `convergenceStartupReconcile` iterates all convergence stores.
6. Unit coverage proves create routing, unknown rig error, and tick iteration
   across handlers.

## Acceptance: `ga-ecdms.3`

1. `gc converge create --rig <name>` sends the rig request parameter; omitting
   `--rig` preserves city-store behavior.
2. `gc converge list` aggregates city plus rig stores by default.
3. Multi-store table output includes `STORE` as the second column, using `city`
   for the city store.
4. `gc converge list --rig <name>` narrows to one store and suppresses the
   `STORE` column.
5. `gc converge status` and `gc converge test-gate` search all stores by bead
   ID without requiring `--rig`.
6. JSON list output includes a `store` field for every row.

## Acceptance: `ga-ecdms.4`

1. Convergence `CreatedPayload` includes optional JSON field `store_key`.
2. City-store created events keep current wire behavior when store key is empty.
3. Rig-store created events include the rig name as `store_key`.
4. Payload registration and event schema tests remain in sync with
   `KnownEventTypes`.
5. No other convergence event payload gains `store_key` unless a failing test
   proves it is required.

## Acceptance: `ga-ecdms.5`

1. Unit tests cover `initConvergenceHandlers` building city plus rig handlers.
2. Unit tests cover `convergenceTick` iterating all handlers.
3. Unit tests cover `handleConvergenceCreate` routing and unknown rig errors.
4. Unit tests cover `create --rig` request params and list `STORE` column
   behavior.
5. Integration coverage exercises an end-to-end convergence loop in a rig store:
   create, tick, terminate.
6. `go test ./...` and `go vet ./...` pass, or unrelated pre-existing failures
   are documented with exact output.

## Dependency Graph

`ga-6qnps.3` -> `ga-ecdms.1` -> `ga-ecdms.2`

`ga-ecdms.2` -> `ga-ecdms.3`

`ga-ecdms.2` -> `ga-ecdms.4`

`ga-ecdms.3` + `ga-ecdms.4` -> `ga-ecdms.5`

Option A should not start until the P2 Option B verification bead is complete.
The controller foundation lands before CLI and event surfaces, and the final
verification bead closes the feature only after both user-facing behavior and
event payload behavior are covered.

## Out Of Scope

- Adding `--rig` to approve, iterate, stop, status, or test-gate.
- Moving multi-store logic into `convergenceStoreAdapter`.
- Cross-handler locking.
- Hardcoded role names or role-specific convergence behavior.
- New dashboard/API surfaces unless generated schemas must update due to typed
  event payload changes.

## Risk

The highest risk is breaking the single-writer-per-bead invariant by hiding
multi-store behavior inside the store adapter or by adding cross-handler locks.
The second risk is operator confusion if list/status behavior is inconsistent:
list should aggregate by default, `--rig` should narrow, and status should find
the loop by bead ID.
