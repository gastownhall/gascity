# 0002 — City Cockpit Overview

Status: implemented · 2026-07-14

## Intent

Replace the city Home page's list-oriented summary with a live operational
instrument panel, and expose the same truthful aggregate contract to the hosted
Forge city Overview. The visual language comes from the reviewed Cockpit
prototype: activity trace, odometer, gauges, a segmented run-state bar, session
meters, run rings, and system lamps.

## Correctness decisions

- The work pipeline does **not** use `hooked` or `review`. Gas City's canonical
  bead status is `open | in_progress | closed`; production stores normalize
  other upstream strings. The segmented instrument instead uses the closed
  `RunStatus` enum: pending (queued), active (running), waiting, and canceling
  (stopping).
- `GET /v0/city/{cityName}/runs` returns typed `status_counts` for all eight run
  states. Counts are computed before the caller's row limit, so a limited list
  cannot shrink the census.
- `GET /v0/city/{cityName}/usage` reads only a bounded tail of the local usage
  fact log. It reports local-estimate availability, recording state, timestamps,
  partial reasons, malformed/oversized records, and unpriced calls. It never
  exposes filesystem paths. An exec/discard provider returns unavailable rather
  than replaying an old local file.
- The hosted proxy allowlists only aggregate reads: exact `usage` and `runs`,
  plus segment-shaped `runs/{id}` and `runs/{id}/steps`. Raw events/SSE,
  cancellation, export, and unknown descendants remain default-denied.
- The activity trace is built from successive real aggregate samples. Forge
  never receives raw event payloads.

## Degradation and accessibility

Every instrument remains mounted while loading, stale, partial, empty, or
unavailable. Text next to the instrument states its provenance; missing data is
never rendered as a healthy zero. Odometer values, gauges, run stages/retries,
and lamp health are named in the accessibility tree. Track-only visuals are not
duplicate controls, interactive targets are at least 24px, motion respects
reduced-motion, and grids reflow by container width.
