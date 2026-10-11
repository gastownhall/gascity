# W-test-fixture: migrate raw-bead test fixtures → real store test doubles

**Goal:** eliminate the code smell where TEST code hand-crafts `beads.Bead` literals and
cracks them into `session.Info` via `session.InfoFromPersistedBead` — instead, tests create
sessions through a real store test double and read typed objects back, the way production does.
Terminal payoff: `InfoFromPersistedBead` unexports (`→ infoFromPersistedBead`), the compiler
boundary the migration's DoD called for.

Grounded by a 14-agent read-only categorization of all 68 test files (~498 real call sites).

## Site inventory (~498 codec CALL sites; 8 census-needle string literals excluded)
| Replacement | occ / files | what it is |
|---|---|---|
| **store-read** | ~200 / 40 | bead is ALREADY in a store the test drives → read Info via `sessionFrontDoor(store).Get(id)` / `ListAll`. Includes 34 cmd/gc oracle sites re-routed through the front door (Get runs the codec internally). |
| **store-create-returns-info** | ~117 / 30 | build the fixture via `Store.CreateSessionInfo(spec)` (persists AND returns Info). |
| **info-struct-literal** | ~115 / 36 | `session.Info{...}` literal — no store under test, or a deliberately divergent-from-store fixture. |
| **lowercase-codec-in-package** | ~57 / 19 | internal/session oracles — mechanical `InfoFromPersistedBead`→`infoFromPersistedBead`. |
| **keep-edge-oracle** | 9 / 5 | raw-vs-Info cmd/gc twin oracles — human call (convert Info side to front-door / retire). |

**Package split:** cmd/gc ~420 (the real smell) · internal/session ~67 (rename-only oracles) ·
internal/api 1 · internal/worker 1 (**the sole cross-package caller that blocks the unexport**).
**Volume concentration:** 4 files ≈ 60% — `session_lifecycle_parallel_test.go` (105),
`session_reconcile_test.go` (48), `session_wake_test.go` (~28), `session_reconciler_test.go` (~20).

## Canonical test-double pattern
**New package `internal/session/sessiontest`** (for black-box tests in cmd/gc, internal/api,
internal/worker — NOT internal/session white-box, which keeps its existing `seedSessionStore`/
`sessionBeadFixture` to avoid an import cycle):
```go
func Store(t, seed ...beads.Bead) (*session.Store, *beads.MemStore)   // memstore-backed front door + raw store; seed is VERBATIM
func Info(t, s *session.Store, spec session.CreateSpec) session.Info  // create via front door → Info (store-assigned id)
func InfoFromMeta(t, meta map[string]string) session.Info         // throwaway-store one-liner for standalone fixtures
func SeedBead(t, b beads.Bead) session.Info                       // VERBATIM seed + front-door Get — for fixtures needing
                                                                  // Status=closed / custom labels / pinned CreatedAt / specific id

```
Plus cmd/gc `reconcilerTestEnv` methods (`sessionInfo(id)`, `createSessionInfo(name,template)`)
— collapses ~40 store-read sites in the reconciler-env files.

**Per-category rewrite:**
- **store-read:** `store.Create(bead); f(InfoFromPersistedBead(bead))` → `f(sessionFrontDoor(store).Get(id))` (or `e.sessionInfo(id)`).
- **store-create:** `InfoFromPersistedBead(beads.Bead{...clean metadata...})` → `sessiontest.Info(t, s, CreateSpec{...})`; use `SeedBead` when the fixture sets Status/CreatedAt/custom labels (`CreateSpec` can't express those).
- **info-literal:** `InfoFromPersistedBead(beads.Bead{Metadata:{...}})` → `session.Info{...}` (fields map 1:1). **Mandatory** for deliberately-divergent fixtures (stale twin, `ID:"missing"`, pinned CreatedAt) where a store read would erase the divergence.
- **internal/session oracles:** mechanical lowercase rename.

## Edge-oracle disposition — nothing forces the codec to stay exported
- Genuine codec/equivalence oracles STAY in internal/session on `infoFromPersistedBead` (~54). Several feed deliberately non-round-trippable corpora (degraded/whitespace/non-session shapes) → MUST keep the raw codec (a store round-trip would filter/normalize them).
- cmd/gc oracles can't relocate (their twinned funcs are `package main`) → they DROP the exported codec by reading Info through the front door (`Store.Get` runs the private codec internally, byte-identical).
- `internal/api/session_response_wire_test.go` → `Store.GetPersistedResponse(id)` (drops BOTH `InfoFromPersistedBead` + `PersistedResponseFromBead`).
- `internal/worker/session_record_equiv_test.go` (the sole unexport-blocker) → `Store.Get(id)`.

## Human-decision points (I'll resolve these during execution; flagging for visibility)
1. **`session_reconcile_test.go` shim helpers** (`wakeReasonsForBead`/`healStateInfo`/`healStatePatchFromBead`): keep as the single projection boundary for the `makeBead` corpus vs push `Info` construction up into callers. → **Plan: keep one boundary, route it through a `sessiontest` shim** (smaller, no fan-out).
2. **9 raw-vs-Info twins** (`session_drainack_info_equiv`, `session_w4_split_equiv`, `session_wtick_twins` MatchesRaw): convert Info side to front-door store-read (ships the unexport) vs relocate vs retire-as-golden. → **Plan: convert to store-read now; treat retire-vs-golden as follow-on** (don't block the unexport). `session_wtick_twins` pins TrimSpace fidelity → keep raw/struct there.
3. **`session_record_equiv_test.go`** bead-form vs front-door-form equivalence: front-door adds `IsSessionBeadOrRepairable` narrowing. → **Plan: store-read (equal for canonical typed session beads); if strict bead-form is required, relocate the oracle into internal/session instead.**
4. **struct-literal conversions with time/bool metadata** (`pending_create_claim`, `last_woke_at`, CreatedAt): confirm exact codec metadata→Info field names before flipping (a wrong name silently diverges). → prefer front-door/shim route when not 1:1.

## Phased execution (worktree-isolated, ≤5 files/agent, disjoint sets; each red-teamed)
- **Phase 0 — Foundation (land first, blocks all):** add `internal/session/sessiontest/` (+ `scripts/add-testenv-import.go`) + the `reconcilerTestEnv` methods. No conversions. Verify build + `go test ./internal/session/sessiontest/`. Merge before fan-out (shared file → keep off the parallel worktrees).
- **Phases 1–9 — cmd/gc + api + worker conversions (parallel):** disjoint file groups, one agent each; the 4 big files get a dedicated agent (context-decay). Each wave: convert → `go test` touched packages + `go vet` → confirm the census guard is UNCHANGED (non-test scan unmoved). Red-team each wave (behavior-identical fixtures; divergent fixtures stayed struct-literals; no store round-trip corrupted a degraded corpus).
- **Phase 10 — rename + unexport + census (LAST, gated):** repo-wide grep gate — `InfoFromPersistedBead`/`PersistedResponseFromBead` external callers MUST be zero before flipping. Then lowercase the internal/session sites + the definition; delete the exported name. Convert the census: `InfoFromPersistedBead(` → hard zero-pin; add `infoFromPersistedBead(` as a needle policed to zero in cmd/gc/api/worker. Full sharded suite + `TestTypedClassCodecCensusRatchet` green.

## Risks (from the map — the impl agents + red-team must honor)
1. `CreateSpec` can't express Status(closed)/CreatedAt/custom labels → use `SeedBead` for those; naive `CreateSessionInfo` silently drops load-bearing metadata/labels.
2. Front-door `Get` narrows via `IsSessionBeadOrRepairable` → degraded/non-session/whitespace corpora MUST stay raw (don't blanket-convert oracles).
3. Deliberately divergent-from-store fixtures (stale twin, `ID:"missing"`, pinned CreatedAt) → struct-literal ONLY; a store read erases the divergence the test asserts.
4. `testStore` in some files is a write-tracking MOCK (batch-capture/error-injection), not a memstore → migrating changes how writes are asserted; verify quarantine/patch assertions still fire (~45 nuanced sites).
5. Unexport ordering: the 2 cross-package callers (worker/api) block the Phase-10 build → the grep gate is mandatory.
6. Big-file context decay → one agent per big file; re-read before editing.
7. The census guard is the enforcement mechanism + its own literals are needles → flip ONLY in the terminal step, regenerating the baseline from its printed literal.
8. Import-cycle trap: `sessiontest` imports `session` → internal/session white-box tests can't use it (keep their existing helpers).
9. Map-driven oracle conversions need string→typed parsing → prefer front-door/shim when not 1:1.
