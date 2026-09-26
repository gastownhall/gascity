# Plan: Pack-root drift CLI wiring (`ga-i3bm`)

> Owner: `gascity/pm` · Created: 2026-05-01
> Source bead: `ga-i3bm` (closed by this plan)
> Architecture: `ga-rmfw` (closed) · Designer: `ga-i3bm` body (`source:actual-designer`)
> Cluster lineage: `ga-a3ry.1` (binary drift, phases 1–2c shipped)

## Why this work exists

Phase 2c of `ga-a3ry.1` shipped binary drift end-to-end. `gc start` already
hashes the local `gc` binary against the supervisor's `/health` `BuildID`,
prints a `Supervisor:` identity line first, and auto-restarts on drift
(governed by `--no-auto-restart`, `--dry-run`, and
`[daemon].auto_restart_on_drift`).

`runStartDriftCheck` already calls `decideDriftAction(commit, status, nil,
flags)` with `nil` in the third position. That argument is `packDrift
[]string`, and `DetectPackDrift([]PackRootStatus)` from phase 1 is built
to populate it. The decision matrix is already symmetric over
`(binaryDrift || len(packDrift) > 0)`. **Only the inputs are missing.**

## Goal

Wire pack-root drift end-to-end across six call sites so that `gc start`
detects pack drift identically to binary drift and applies the existing
flag/config matrix. Operator-facing UX preserves the wording, channel
routing, and accessibility contract pinned by the designer.

## Work breakdown

| Bead       | Title                                                                                       | Routes to | Gate           |
|------------|---------------------------------------------------------------------------------------------|-----------|----------------|
| `ga-i3bm.1` | Wire pack-root drift end-to-end through supervisor /health (single-PR scope)                | builder   | ready-to-build |

**Single-bead scope is deliberate.** The architect's `ga-rmfw` § 16 and the
designer's design.md § 9 both explicitly chose single-PR shape:
> The implementation is one logical change; splitting it across multiple
> beads would create false synchronization points (handler can't ship
> without resolver; resolver can't ship without runtime field; runtime
> field is meaningless without expandPacks capture).

The PM preserves that shape. No design hop is needed (design is done); no
test-authoring hop is needed (test plan is fully specified by the architect's
§ 13 and designer's § 7, and the builder follows TDD per project culture).

## The six implementation surfaces (in landing order)

1. `internal/config/pack.go` — `expandPacks` captures
   `[]PackRootSnapshot{Dir, ParsedAt}`; `loadCityConfig` returns it.
2. `cmd/gc/city_runtime.go` — `CityRuntime.packRoots` field +
   `PackRootSnapshot()` accessor under `serviceStateMu` RLock; populated at
   startup AND every successful `reloadConfig`.
3. `internal/api/supervisor.go` — extend `CityResolver` interface with
   `PackRoots() []PackRootStatus`; `cityRegistry` impl with dedup-by-Dir +
   min(`ParsedAt`).
4. `internal/api/huma_handlers_supervisor.go` — `SupervisorHealthOutput.Body`
   gains `PackRoots []PackRootStatus` (`omitempty`, `pack_roots` JSON tag);
   `humaHandleHealth` calls `sm.resolver.PackRoots()`. Regen
   `internal/api/openapi.json`, `docs/schema/openapi.json`, and
   `internal/api/genclient/`.
5. `cmd/gc/drift_client.go` — `httpSupervisorClient.Status` maps genclient
   `PackRootStatus` → `drift.SupervisorStatus.PackRoots` field-for-field at
   the wire boundary.
6. `cmd/gc/cmd_start_drift.go` — `runStartDriftCheck` calls
   `DetectPackDrift(status.PackRoots)`; passes `packDrift` into
   `decideDriftAction` (replacing the current `nil`); fail-open on
   `packErr` with stderr warning.

## Acceptance criteria (rolled up)

- All six surfaces ship together in one PR.
- `gc start` reproduces the four scenarios from designer's § 2
  byte-for-byte (modulo dynamic data).
- Wording table from designer's § 3 is preserved exactly. Tests pin it.
- Channel-routing table from designer's § 5 is honored end-to-end.
- All eight UX rules from designer's § 6 are preserved.
- Backward compatibility: older supervisors without `pack_roots` field
  cause `gc start` to degrade cleanly to binary-only drift.
- OpenAPI + genclient regen committed; `TestOpenAPISpecInSync` passes.
- Architect's § 13 test plan + designer's § 7 UX-flavoured tests all pass.
- Existing flag-combo matrix in `cmd_start_drift_test.go` continues to pass.

## Out of scope (architect § 15 + designer § 8)

- Verbose drift output (`--verbose-drift`).
- `--quiet` summarization for very large drift lists.
- JSON output mode for `gc start`.
- Localisation.
- Pack-content hashing (mtime is the shipped signal).
- Color in the drift report.
- `gc reload` integration.
- Detecting drift in agents already running.
- A standalone `gc drift` command.
- Per-file granularity in `/health`.

## Routing rationale

Architect's design (`ga-rmfw`, closed) gives the full implementation
surface. Designer's design (`ga-i3bm` body, `source:actual-designer`) gives
the operator UX contract layered on top. Both explicitly reject further
decomposition. Routed to **builder** with `ready-to-build`. No designer or
validator hop.
