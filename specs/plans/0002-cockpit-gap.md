# Gap Analysis: Cockpit implementation vs mockup — 2026-07-11

> **CONVERGED (2026-07-11, same day).** A zero-trust re-verification against
> the code confirmed all ranked items closed: every drifted/missing/partial
> row below is implemented and pinned by a test (856/856 SPA tests, 79
> cockpit tests, warning-clean; dashboard-check + smoke + full internal/api +
> internal/worker Go suites green). The one fresh-eyes find of the re-check
> (uppercase pause control vs the mock's lowercase) was fixed on the spot.
> Remaining: the live pass against a supervisor running the new binary —
> operator's call. The analysis below is preserved as the historical record.
>
> **LIVE PASS DONE (2026-07-11).** Binary built/installed, supervisor
> restarted, city verified live (usage 200, hooked/review counts on status,
> new bundle served). A screenshot-vs-mockup band-for-band re-analysis found
> the page ≈96% converged with four cosmetic deltas, all closed same-session:
> dial row now spreads space-between like the mock; odometer sub-line and
> lamp value alignment match the mock's t-label conventions; and the
> lowercase-pause fix is now actually IN the served bundle (the prior binary
> embedded a pre-fix dist — lesson: `make install` embeds whatever dist/ is
> on disk; run `make dashboard-build` first). Low traffic on a quiet city
> renders instruments near rest — that is data honesty, not a gap.

Spec: `tmp/factory-home-visions.html` (Cockpit vision + shared chrome),
`tmp/factory-visions-cockpit-{dark,light}.png`, plan 0002 binding decisions.
Implementation: `dash/cockpit-home` (`routes/CockpitHome.tsx`,
`components/cockpit/*`). Method: fresh-eyes requirement extraction and
implementation evidence inventory joined row-by-row; observed behavior from a
live render (old-binary supervisor, so `/usage` 404 → worst degraded path).

**Completion ≈ 64%** = (27 done + 0.5 × 11 partial) / 51 rows; 7 drifted + 4
missing score zero. Perceived gap is larger: drift concentrates in the most
visible behaviors and the old binary hid two gauges, the VU bank, and the
odometer sub-line.

## Drifted (contradicts the spec's intent — redo)

1. **Band collapse**: gauges 2–3, VU bank, pipeline swap to italic sentences
   when a source is null (`CockpitHome.tsx:322,330,337`). Spec: instruments
   never dismount; needles rest, per-instrument "no data" microcopy.
2. **Pause** only reaches the oscilloscope (`:277`). Spec: pause freezes all
   live updates (mock HTML 1491).
3. **VU meters transposed**: wells `h-[58px] w-[130px]` (`VUBank.tsx`); spec
   58w × 130h tall columns, flex-end, bank min-height 190px.
4. **Pipeline** shows run phases → `/runs` (`:82-88`); spec: bead statuses
   ready/hooked/in-progress(/review) → `/beads`.
5. **Odometer** counts runs complete; spec: beads closed today (+ "$ est
   today" always).
6. **Lamps** are sessions/mail-unread/store/events; spec: orders / patrol /
   dolt store / mail traffic with spec hrefs.
7. **Live-ness**: burn/tokens/VU step on 20s polls; spec: move with
   `worker.operation` events (token/cost wiring was deferred — now un-deferred,
   #1255/#1256).

## Missing

- Numeric gauge tick labels (0/75/150 · 0/300/600 · 0k/20k/40k) — component
  supports `tickFormat`, page passes none.
- Scope baseline hairline (y=h−12.5, `--rule`).
- Live-dot pulse (opacity by event recency); chip label flip to "paused".
- Needs-you age + "· N more" overflow.

## Partial

- Chip age lacks "last event … ago" wording; scope readout below canvas not
  top-right; warn arc overlaid on track instead of outside (R+3); odometer 3
  digits not 4; gauge 2/3 + lamp hrefs off-spec; VU sort by volume not
  rig/name; needs-you strip missing hairline band + maroon glyph + "needs you"
  label; captions wording.

## Done (letter + spirit, evidence in analysis)

Header/nav/badges/title/synopsis; calm strip + sole-maroon rule
(test-enforced); scope buffer/DPR/reduced-motion/head dot; gauge
geometry/needle/readouts; pipeline track/alpha/legend mechanics; VU
fill/peak-hold mechanics; run rings complete; lamp tone discipline; layout
bands + 5fr/4fr/3fr responsive grid; easing/4Hz/reduced-motion; theme +
canvas ink via ThemeContext; anti-Norse honesty of every rendered number.

## Ranked close-out (in flight on dash/cockpit-home)

1. Instruments never leave the wall (kill band-collapse paths).
2. Backend counts: `closed_today`, `hooked`, `blocked` on status work counts.
3. Bead-status pipeline → `/beads`.
4. Mockup lamp set (orders/patrol/store/mail-rate from feed + attention).
5. Gauge tick labels, outside warn arc, spec hrefs.
6. VU transpose + live-session roster + rig/name sort.
7. Pause-freezes-all; dot pulse; chip wording; needs-you glyph/age/overflow;
   scope baseline + readout placement; 4-digit odometer.
8. worker.operation token/cost wiring (un-deferred).
9. Live pass against the new binary (only way to see the non-degraded page).
