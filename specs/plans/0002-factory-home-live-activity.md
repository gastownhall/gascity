# 0002 — Factory Home: live activity redesign

Status: **active — implementing** · Decided 2026-07-10 in conversation (grill-me
interview) · Owner: csells · Renumbered from 0001 (taken by the merged docs plan)

**Vision picked (2026-07-10): Cockpit** — the instrument-panel concept from
round two (gauges, VU meters, rings, lamps, oscilloscope). Implementation
branch: `dash/cockpit-home`.

## What

Replace the dashboard home page (`/`, currently `AmbientHome.tsx`: calm census +
attention panel + concern region) with a **live factory activity view**. The page's
job flips from "speak only when something needs you" to "show everything happening
in the city right now." Attention/needs-you surfacing moves entirely to the nav
badges and per-tab panels, which already carry it.

The same page ships publicly at factory.gascity.com showing the real factory
running on the beads / gastown / gascity rigs. It must read as unmistakably ALIVE.

## Decisions (settled — do not relitigate)

1. **Deliverable path: mockups → pick → implement.** First deliverable is 2–3
   full-page animated HTML mockups (shared widget library, simulated-but-realistic
   event data, light/dark, current design tokens) for csells to react to. The
   winning direction then gets implemented in the SPA against live data.
2. **New telemetry is green-lit.** Where a desired metric has no API today, the
   implementation phase builds it. Known gaps found in research:
   - Token/cost: facts exist in `<city>/.gc/usage.jsonl` (`internal/usage`) and on
     live `worker.operation` event payloads (prompt/completion/cache tokens,
     `cost_usd_estimate`, model) — but no HTTP endpoint reads usage.jsonl. New
     endpoint needed for daily baselines; SSE already carries live increments.
   - Host CPU/RAM: `/api/health/system` is procfs-only (all zeros on macOS);
     needs a cross-platform implementation.
3. **Audience: desktop operator first.** Dense widgets, fine type, more items per
   viewport. No concessions to TV/kiosk legibility; spectators get the operator view.
4. **Keep the current design system.** Warm-paper OKLCH tokens, Inter Variable,
   5-step type scale, flat-page rule (no cards/pills), glyph+word status,
   hairlines+weight hierarchy, light/dark via data-theme. The page earns a new
   *motion register* (continuous data-driven movement) but not a new *visual
   language*. Motion respects prefers-reduced-motion and ships a pause control.
5. **100% real data, 100% clickable.** Every animated element must map to a real
   event/entity and deep-link to the appropriate existing detail surface
   (`/beads?bead=`, `/agents/:slug`, `runDetailHref(runId, scope)`,
   `/mail?message=`, `/activity?mode=events&type=`). No decorative motion that
   isn't backed by a real event (the anti-Norse rule). Mock data simulates the
   real event vocabulary (75 typed event types, `internal/events/events.go`).
6. **Widget invention is delegated** to the design work (csells asked to be
   surprised); layout/motion/widget choices do not need re-approval before the
   mockup round.

## Course correction (2026-07-10, after round-one mockups)

Round one (two "ledger" variants: typeset lists + tables in the existing flat
page register) was **rejected**: it duplicated the other tabs' information as
lists. New binding directives from csells:

7. **The home shows data MOVING** — graphs, charts, dials, gauges, flows.
   Never lists/tables restating what Agents/Beads/Runs/Mail already show.
8. **"Keep current styling" means tokens/colors/font/light-dark only** — NOT
   the flat-page/typeset-list ethos of the old ambient home.
9. **Mockup variants must be wildly different concepts**, not layout
   permutations of one idea.

Round two delivers three visions (tmp/factory-home-visions.html): Cockpit
(instrument panel: gauges/VU meters/rings/lamps/oscilloscope), Flow (particle
factory floor: beads flying ready → stations → review → closed, mail arcs,
ship tray, order countdown rings), Radar (polar scope: phosphor sweep, event
blips on domain rings, sessions orbiting at token-burn speed, run arcs).

## Research artifacts (session scratchpad, 2026-07-10)

Five research docs were produced (event vocabulary, API/data-surface inventory,
design-system reference, routing/drill-down inventory, dashboard-visualization
web research). Durable copies of anything needed long-term should be re-derived
from source; the load-bearing facts are restated above.

## Implementation plan (Cockpit, branch `dash/cockpit-home`, planned 2026-07-11)

Scouted 2026-07-11: mockup extraction, SPA map, backend map. Load-bearing
corrections to the earlier research: (a) `worker.operation` SSE payloads have
token/cost FIELDS but only `AgentName`/`RunID` are wired today
(`TestWorkerOperationPayload1aWiringStatusPin` pins this) — live burn/token
gauges require wiring those fields; (b) usage belongs on the typed `/v0`
plane (`cityGet`), while host CPU/RAM stays on the `/api/*` BFF plane
(sanctioned carve-out).

**Phase 1 — backend telemetry (TDD, each item red-green):**

1. `GET /v0/city/{cityName}/usage` — Huma typed endpoint (`cityGet` in
   `internal/api/supervisor_city_routes.go`, input embeds `CityScope`) over
   `usage.ReadFacts(<city>/.gc/usage.jsonl)`: totals, today window (from
   `Fact.At` unix-millis), trailing recent rate, optional by-model buckets,
   `warnings` surfaced. Memoize behind the server response cache (ReadFacts
   loads the whole file). Gates: `make spec-ci` + `make dashboard-ci`
   (regenerated OpenAPI + TS client committed together).
2. Cross-platform host metrics — refactor `dashboardbff/health.go`
   `currentSystemHealth()` behind a `hostMetrics()` seam; split
   `health_linux.go` (existing /proc readers), `health_darwin.go`
   (`golang.org/x/sys/unix`: hw.memsize, loadavg, boottime, Maxrss; Mach
   free-mem), `health_other.go` (zeros). No gopsutil (test-transitive only
   today; not worth promoting for four scalars). BFF plane: no spec regen.
3. ~~Wire `worker.operation` 1a fields~~ Deferred 2026-07-11, then
   **UN-DEFERRED and DONE the same day** (gap close-out): model + four token
   counts + cost/unpriced are stamped at operation finish from the same
   sessionlog-tail batch and `pricing.Registry.Estimate` call that feed the
   usage facts, so the two surfaces agree by construction. Pin test moved six
   fields to wiredAlready; doc tags updated (spec regenerated). Still
   unwired for lack of a source: prompt_version, prompt_sha, bead_id,
   latency_ms. Known inherited lag: extraction at finish reflects the prior
   completed invocation.

**Phase 1 status:** item 1 (usage endpoint, incl. a `recent_by_session` block
for the VU bank) ✅; item 2 (darwin host metrics) ✅; item 3 deferred as above.

**Phase 2 status (2026-07-11):** items 4–7 ✅, then a full **gap close-out**
against the mockup the same day (see `0002-cockpit-gap.md`, CONVERGED):
instruments never dismount (degraded sources show per-instrument microcopy,
never replacement text); pipeline = bead statuses ready/hooked/in
progress/review (new `status.work.hooked`/`review` counts, spec + clients
regenerated) → `/beads`; odometer = beads closed today (event-seeded since
local midnight + live increment), 4 digits, cost sub-line always present,
unlinked per spec; lamps = orders/patrol/dolt store/mail traffic; gauge tick
labels + outside warn arc + worker.operation hrefs + live feed-derived
burn/tokens with usage-poll fallback; VU bank transposed to tall columns with
live-session roster, rig/name sort, live tok/min; pause freezes all live
updates; dot pulse, "last event … ago", needs-you hairline band +
glyph/label/age/overflow (still exactly one accent element, strictly
tested). Final state: SPA 98 files / 856 tests, cockpit 79 tests,
warning-clean; typechecks, dashboard-check, dashboard-smoke, full
internal/api + internal/worker + dashboardbff Go suites all green.

**Item 8 remaining — the only open step:** live verification against a
supervisor running the NEW `gc` binary (operator restarts the supervisor;
this session does not restart resident daemons unprompted), then the visual
pass against the mockup PNGs.

**Phase 2 — frontend cockpit (Vitest per component, warning-clean):**

4. `useGcEventFeed` — extend `hooks/useGcEvents.ts` to surface the parsed
   typed envelope to subscribers (today it parses then discards; refresh-nudge
   semantics stay for existing callers).
5. Instrument components (net-new; no chart lib): `Gauge` (JSX SVG, 240°
   sweep, warn zone, CSS-transitioned needle), `Odometer` (digit columns,
   sized past 4 digits), `PipelineBar`, `VUBank` (keyed React render, peak
   hold), `RunRings` (dashoffset arcs, attempt warnings), `StatusLamps`
   (ok/warn only — maroon reserved), `Oscilloscope` (canvas via ref + local
   rAF over a ring buffer, outside React reconciliation; ink cache from
   ThemeContext, not MutationObserver). Instrument state updates throttled
   ~4Hz; house out-quart easing token; prefers-reduced-motion + pause control.
6. `CockpitHome` page: composes instruments in the mockup's four bands
   (scope / dials / pipeline / 5fr-4fr-3fr grid — add the missing responsive
   fallback). Data: `useRunSummary()` (SSE-live census), `/v0/.../status`
   counts, new usage endpoint, event feed for scope+gauges. Every element
   deep-links via existing helpers (`runDetailHref`, `beadHref`, `mailHref`,
   `activityEventHref`, `/agents/:slug`). Needs-you strip stays the sole
   accent, fed by `useAttentionModel()`.
7. Swap `/` to `CockpitHome`; retire `routes/AmbientHome.tsx`, the ambient
   components (`PhaseCensus`, `StatusSentence`, `ConcernRegion`,
   `FirstRunNote`, `AttentionSummaryPanel`) and their tests. KEEP
   `test/assertions/oneMarkRule.ts` (shared by other routes).
8. Gates: `npm test` (warning-clean enforced), `make dashboard-check`,
   `dashboard-smoke`, and live verification against a running city.
