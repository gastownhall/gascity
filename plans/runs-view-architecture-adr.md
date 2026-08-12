# ADR: Run View End-State Architecture (Summary + Detail)

Status: Proposed (Lead Architect decision)
Date: 2026-06-28
Deciders: Lead Architect, operator
Priority order: maintainability > testability > UX/performance. Effort-agnostic.

## Context

The dashboard Run view (summary lanes + per-run detail diagram) is
reconstructed almost entirely client-side in TypeScript. The summary view
fans out to four slow supervisor `/v0` reads — `molecule(all=true)` (~6.8s
over ~340k rows), `formulaFeed` (~10s), per-rig `task(all=true)`, and the
active `listBeads` — then folds them in the browser via `buildRunSummary`
(`shared/src/runs/summary.ts`) and `enrichRunSummary`
(`frontend/src/supervisor/runSummary.ts`). The bottleneck is the **data
fetch**, not the fold. The detail view is **already fast**: it fetches a
server-folded `workflowRun` SQL snapshot (`buildWorkflowSnapshot` →
`tryFullWorkflowSQL`, ~190ms) plus the compiled formula
(`api.formulaDetail`), then enriches in TS.

Verified code facts that drove this decision:

- **Run logic is large and shared.** `shared/src/runs/*.ts` is ~5,000 LOC
  non-test (`phaseMapping.ts` 708, `summary.ts` 510, `groups.ts` 359,
  `formula-run.ts` 289, `execution-instances.ts` 264, `health.ts` 224,
  `node-shape.ts` 190, …) plus ~779 LOC of frontend enrich. It powers
  **both** the summary and the detail diagram.
- **Summary and detail share the classifier.** `formula-run.ts` imports and
  calls the *same* `mapRunPhase` / `stageProgress` from `phaseMapping.ts`
  (lines 17, 128, 130) that `summary.ts` uses. Any design that puts summary
  phase in Go and leaves detail phase in TS **re-forks this exact surface** —
  the outcome every credible candidate calls the worst.
- **The drift trap is real.** `phaseMapping.ts` `stagesForFormula`
  (lines 439–575) hand-maintains per-formula step-id stage tables
  (`mol-adopt-pr-v2`, `mol-design-review-v2`, `mol-bug-report-flow-v2`,
  `mol-bug-report-implementation-v2`) that mirror the Go compiler's step IDs
  with **no coupling and no CI gate**. A formula TOML edit silently desyncs
  the run view.
- **The compiler does not own the phase taxonomy.** `recipe.Step.Phase` is
  only `"vapor"`/`"liquid"` (an instantiation hint). The compiler
  (`buildFormulaDetail`) owns step IDs + the node/edge graph, but the named
  stage buckets/labels and the RunPhase vocabulary
  (intake/implementation/review/approval/finalization) are **additive
  interpretation** that lives only in TS today. So "derive stages from the
  compiler and delete `phaseMapping`" is only partly possible — it relocates
  that knowledge, it does not eliminate it.
- **`node-shape.ts` re-encodes Go-owned vocabulary.** The
  `gc.kind`→`RunConstructKind` map and `HIDDEN_CONSTRUCTS` set encode kinds
  (`ralph`, `check`, `spec`, `fanout`, …) that are stamped by the Go
  dispatcher (`internal/dispatch/fanout.go`) and keyed in
  `internal/beadmeta/keys.go`. Same drift class as the stage tables.
- **The substrate already exists.** `citySampler` (lazy per-city background
  tailer), the `fetchStatus` loopback `127.0.0.1:port` pattern, the
  `transientCityEventProvider` read-only `Watch` (no second writer),
  `events.ReadFiltered` (which transparently walks rotated `.gz` archives),
  and `wire_contract_test.go` (field-shape guard) are all present in
  `internal/api/dashboardbff` and `internal/events`.
- **Rotation correction.** `EventsRotationConfig.EnabledOrDefault()` returns
  `true` when `[events.rotation]` is absent, so the production city path
  rotates at **256 MiB by default** (the design doc's "rotation off" claim is
  wrong). But `archive_retain_age` defaults empty → 0 → archives kept
  forever, and `ReadFiltered` walks them, so cold replay still reconstructs
  full history.
- **Liveness exists.** The SPA already runs `useGcEventRefresh([bead])` with
  a 10s debounce floor (`REFRESH_DEBOUNCE_MS`).

## Decision

Adopt **SingleSource-RunProjection**: run *semantics* get exactly one home
in Go, and run *knowledge* that is editable data is lifted into a versioned
declarative spec that is code-generated to both languages under one CI gate.

1. **`internal/runproj` (new, fork-owned).** The single home of run
   projection: the events→bead fold; `BuildRunSummary` (grouping precedence,
   `isRunGroup`, `mapRunPhase`/`stepIdPhase`, `stageProgress`, counts); and
   `BuildRunDetail` (`groupRunBeads`, semantic-id resolution, edges,
   execution instances, display-state, lanes). Summary and detail are two
   entry points over **one** set of primitives — they call the same grouping,
   the same phase classifier, the same formula-identity resolver. Depends only
   on `internal/beads`, `internal/beadmeta`, `internal/formula` (all Layer
   0–1; no upward dependency).
2. **`runspec/*.toml` + codegen (the data layer).** Per-formula stage tables,
   phase-token vocabularies, lifecycle rank, the `gc.kind`→construct map
   (sourced from `internal/beadmeta`/`internal/dispatch`), and the
   hidden-construct set are authored once as data and `go:generate` +
   npm-prebuild into `internal/runspec/spec_gen.go` and
   `shared/src/runs/spec/spec.gen.ts`. A regenerate-and-diff CI test
   (mirroring `TestOpenAPISpecInSync`) fails the build if either generated
   file diverges from the source `.toml`.
3. **Per-city run-projection tailer in `internal/api/dashboardbff`** (modeled
   on `citySampler`): lazy start, cold-replay `.gc/events.jsonl` via
   `ReadFiltered` (archives included), live-tail via read-only `Watch`, fold
   `bead.created/updated/closed/deleted` into `map[beadID]Bead`, and on each
   tick republish the summary + detail projections under a brief lock.
   Server-owned and singular per city, so all viewers converge. The
   monotonic progress/thrash marks move **into the tailer** (shared across
   viewers, survive reload).
4. **Two non-Huma BFF `/api` endpoints** (loopback-only, no OpenAPI churn):
   `GET /api/city/{city}/runs/summary` and
   `GET /api/city/{city}/runs/{runId}/detail`, serving the **existing**
   `RunSummary` and `FormulaRunDetail` DTOs byte-for-byte. Each layers
   session health/census at request time from one loopback `/v0` sessions
   read.
5. **The SPA becomes a pure renderer.** Delete ~5,000 LOC of shared run logic
   and ~779 LOC of frontend enrich. Keep only render components and DTO
   types, plus an import of the generated spec for presentation-only
   label/kind lookups.

### Diagram

```
                       runspec/*.toml  (single source of run KNOWLEDGE)
                              │  go:generate + npm prebuild  (CI: regen-and-diff)
                ┌─────────────┴──────────────┐
                ▼                             ▼
   internal/runspec/spec_gen.go      shared/src/runs/spec/spec.gen.ts
                │                             │ (presentation lookups only)
                ▼                             │
 .gc/events.jsonl ──ReadFiltered(+.gz)──▶ runproj fold  map[beadID]Bead
   bead.created/updated/closed/deleted     │   (read-only Watch, no 2nd writer)
                                           ├─▶ BuildRunSummary ─┐
                                           └─▶ BuildRunDetail ──┤ ONE classifier
                                                                ▼
                          per-city tailer (citySampler-style, warm, server-owned)
                                                                │
   GET /api/city/{city}/runs/summary  ◀──── cached summary  ───┤
   GET /api/city/{city}/runs/{runId}/detail ◀── cached detail ─┘
                    │ + request-time loopback /v0 sessions enrich (health/census)
                    ▼
        RunSummary / FormulaRunDetail DTO  (unchanged shape)
                    │   SSE useGcEventRefresh([bead]) nudge, 10s debounce
                    ▼
        SPA render components + spec.gen.ts (zero run logic)
```

## How the detail diagram is handled (no silent duplication)

The detail diagram is served by a **second projection in the same tailer
over the same fold reading the same spec** — not duplicated logic. Today the
detail view already consumes the compiled Go formula (`api.formulaDetail`,
which `orderRunNodeGroups` uses for ordering) and already fetches a
server-folded SQL snapshot; only the visual enrichment runs in TS. Under this
ADR, `runproj/BuildRunDetail` calls the **same** `mapRunPhase`/`stageProgress`
as `BuildRunSummary`, so the detail StageLadder and the summary lane's stages
are identical **by construction**, not by two TS call sites that happen to
agree. The graph-layout algorithm (`groups.ts` semantic-id disambiguation,
alias maps, loop instancing; `node-shape.ts` targeting) is genuinely hard to
express as data, so it is ported to Go as **code, single-homed**, not forced
into the spec. The only knowledge either view uses is the generated spec; the
only algorithm is the single Go interpreter pair.

## Testing strategy

- **Golden corpus, captured once.** Trimmed real `events.jsonl` slices (e.g.
  the daytona-trial-city log → 10 lanes) with expected `RunSummary` and
  `FormulaRunDetail` JSON captured from the *current* TS output **before** any
  port. The Go interpreters must reproduce them exactly. This preserves the
  ~13 encoded `gascity-dashboard-*` bug fixes through the port.
- **Spec-conformance gate.** Regenerate-and-diff test fails the build if
  `spec_gen.go` or `spec.gen.ts` diverges from `runspec/*.toml` — turning the
  highest-probability drift (formula/stage edits) into a build-time failure.
- **Classifier table/property tests in Go.** Port `phaseMapping.test.ts`
  (411 LOC) token rules — whole-token matching, lead-up-qualifier rejection,
  approval-before-finalization precedence, furthest-stage `LIFECYCLE_RANK`
  determinism — into Go table tests. Property tests: adding a child bead never
  changes a run's root; closing all beads ⇒ complete.
- **Wire contract, strengthened.** Upgrade `wire_contract_test.go` from
  field-presence to running emitted JSON through the **real TS decoders** in
  Vitest, so the DTO (a type with no behavior) cannot drift in shape.
- **Tailer integration test.** Write a temp `events.jsonl`, start the tailer,
  assert the snapshot reflects appended events after a tick, and assert a
  separate recorder can still append (no second-writer regression).
- **Summary/detail consistency test.** One fixture run resolves to the same
  phase/stage through both endpoints — structural, not coincidental.
- **Endpoint degradation test.** Available health when sessions present;
  `unavailable` + `lanesPartial` when the sessions read fails.

## Session-health handling

Health and census are **not** event-sourced and never status-filed —
consistent with the project rule "no status files — query live state."
`session.woke`/`session.stopped` carry only the session name; `lastActive`,
`running`, and `activity` are live process facts. `runproj.BuildRunSummary`
produces lanes with `health`/`census` in the `unavailable` shell. The
endpoint then layers them at request time: one loopback `/v0/.../sessions`
read (the existing `fetchStatus 127.0.0.1:port` pattern), then ported
`deriveRunHealth` + `buildCensus`. When the sessions read fails, health →
`unavailable`, `phaseConfidence` → `inferred`, census → unverifiable, and the
DTO sets `lanesPartial: true` — identical to today's contract, but decided
server-side so every viewer degrades identically. `needsOperator` stays valid
during a sessions outage because it derives from `lane.phase` (a structural
fold fact). The monotonic thrash/progress marks live server-side in the
tailer, so they are shared and survive reload.

## Alternatives rejected

- **ThinServer-FoldOnly** (Go folds events→beads, all run logic stays in TS).
  Lowest execution risk and genuinely lowest *grep-enforceable* Go-vs-TS
  drift, but it **freezes** the verified compiler-vs-stage-table trap rather
  than removing it — disqualifying under an effort-agnostic,
  maintainability-first mandate. It is the correct choice only if effort were
  the priority, which the operator waived.
- **Full Go port without a spec (SingleSource-Go).** Adopted as the *first
  phase* (it is the de-risking deliverable and the strict-subset fallback),
  but as the *end state* it relocates the stage tables and kind map into Go
  *code* rather than data, so adding a formula edits Go source rather than a
  one-line `.toml` — a larger blast radius for the most frequent change.
- **HostedParity-Projection** (embedded SQL engine + `phaseMapping` as a SQL
  UDF). Last on two of three lenses: the most new long-lived coupling for the
  least drift-reduction, it couples the most-bug-fixed function to an engine
  UDF API and smears it across three reasoning sites, and its "diff against
  hosted" parity is aspirational because hosted reads ClickHouse, not the
  embedded store.

## Migration path

Six phases, each ≤5 files, each verifiable and revertable; the fallback
(Phase 4 = single-home-in-Go) is a strict subset of the end state.

- **Phase 0 — Freeze the contract.** Pin the existing DTO interfaces;
  strengthen `wire_contract_test.go` to run emitted JSON through real TS
  decoders (Vitest); capture golden fixtures from current TS output. *Gate:*
  goldens captured, decoder check green.
- **Phase 1 — Go fold + summary interpreter, no wiring.** `internal/runproj`
  with the fold + `BuildRunSummary`, tables as Go literals for now. *Gate:*
  Go golden equals captured TS `RunSummary` (modulo health/census).
- **Phase 2 — Tailer + summary endpoint.** `citySampler`-style tailer
  (cold replay + read-only `Watch`, server-side thrash marks); `runs/summary`
  with request-time sessions enrich. *Gate:* handler + tailer integration
  tests, `make dashboard-check`.
- **Phase 3 — Detail interpreter + endpoint, shared projection.**
  `BuildRunDetail` calling the same classifier; `runs/{runId}/detail`. *Gate:*
  Go golden equals captured TS `FormulaRunDetail`; summary/detail consistency
  test.
- **Phase 4 — SPA cutover behind a flag, then delete TS logic.** Repoint
  both views to BFF (keep SSE nudge + debounce), shadow-compare, then delete
  `shared/src/runs/*` logic + frontend enrich. *Gate:* parity over a soak
  window, SPA tests green, manual preview.
- **Phase 5 — Lift knowledge into the spec (additive upgrade).** Author
  `runspec/*.toml`, add codegen, refactor Go literals to generated constants,
  import generated TS for presentation. *Gate:* regenerate-and-diff CI test;
  full golden corpus green. If deferred, the system rests stably at Phase 4.

## Consequences

- One canonical implementation of every run rule, consumed by both views and
  any future client; AGENTS.md's "object model at the center, API as
  projection" invariant is satisfied at the run layer.
- The most frequent real change (add/alter a formula or stage) becomes a
  one-line data edit that lands atomically on both sides with a CI gate.
- Largest one-time port and a new codegen ritual; cross-runtime portability
  of run *semantics* is sacrificed (the dashboard is always Go-backed).
- Summary first-paint collapses from ~10–38s to a sub-second warm read;
  detail stays ~190ms; wire payload is a few KB; cross-viewer consistency is
  structural.
