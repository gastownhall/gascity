# Spec: Stores return domain objects (de/serialization only at the edges)

Status: active migration spec. Grounded on `origin/main` @ `9e3e32065`.
Authoritative decisions come from the project owner; this doc is the contract the
work items execute against.

## 1. Principle

A `beads.Bead` is the **serialized / storage form**. A domain object
(`session.Info`, `mail.Message`, `orders.OrderRun`, `nudgequeue.NudgeShadow`,
`session.WaitInfo`) is the **type-safe form**. De/serialization happens **only at
the store edge**:

- The **only** primitive that crosses in from API/CLI is a bead ID = an opaque
  **handle** (a string). Business logic never cracks a handle.
- `store.Get(handle) -> domain object` (deserialize once), and writes go back
  through the store (serialize once). `beads.Bead` and every `*FromBead` codec
  live **only inside the edge layer**.
- The **interior** — reconciler tick, CLI command bodies, API handlers, worker
  boundary — receives and passes **domain objects**. It never holds a raw
  typed-class bead and never calls a typed-class `*FromBead`.

Wrapping a bead with an accessor in place (`bead.Metadata["x"]` →
`InfoFromPersistedBead(bead).X` *in business logic*) does **not** satisfy this and
is the specific antipattern being corrected — the bead must not be in business
logic's hands at all.

## 2. Class model (authoritative: `internal/coordclass/class.go`)

`Classes() = { ClassWork, ClassGraph, ClassMessaging, ClassSessions, ClassOrders, ClassNudges }`.

| Class | Domain object | Rule |
|---|---|---|
| **ClassWork** | `beads.Bead` | **Bead IS the domain object.** `internal/dispatch`, sling, convoy (user/sling), assigned-work scans legitimately hold `Bead`. Do NOT type. |
| **ClassGraph** | `beads.Bead` | **Bead IS the domain object.** molecule/step/gate/wisp/control-bead/graph-walk handling holds `Bead`. Field codecs on graph beads (`molecule.WorkflowBeadFromBead`, `beadmeta.MoleculeFailedMetadataKey`, `convoy.ConvoyFields`) are fine. Do NOT type. |
| **ClassMessaging** | `mail.Message` | Typed. `mail.Provider` is the seam (already complete). Also covers `extmsg` families (audit separately). |
| **ClassSessions** | `session.Info` (+ `session.WaitInfo` for wait sub-type: `type=gate` + `gc:wait`) | Typed. `session.Store` exists (read `Get`/`List`, write `ApplyPatch` chokepoint + lifecycle methods). `WaitInfo` is greenfield (seeded by PR #4056). |
| **ClassOrders** | `orders.OrderRun` | Typed. `orders.Store` returns `OrderRun` but has **no `Get(handle)`** yet. |
| **ClassNudges** | `nudgequeue.NudgeShadow` (partial read-only view; authority for the full `Item` is the flock'd `state.json`, not the bead) | Typed. Handle for this class is the **durable nudge ID**, not the bead ID. |

Convoy is not its own class; it resolves to Work (user/sling) or Graph (synthetic).

## 3. Read model

- `store.Get(handle) -> domain object`; typed `List`/`Query` methods **shaped for
  real consumers** (bulk reads for the order-dispatch tracking index; union
  `ListAll(opts)` for the reconciler feed).
- **READ-TIER CONTRACT IS MANDATORY.** Every typed read declares and preserves its
  tier, implemented inside the edge on the embedded `.Store`:
  - Order-dispatch index / sweeps / single-flight reads = `HandlesFor(.Store).Live`
    (cache-bypass is the duplicate-dispatch guarantee). Pin with a test that the
    caching layer is bypassed.
  - API session read model = **cache-first** `cachedListStore` union-merge
    (dashboard perf #3939/#3941 depends on it). `ListAll(opts)` must port that
    union, not do a naive `store.List`.
- **Mixed-class reads take multiple class stores.** Where evidence for one verdict
  spans classes (orders last-run/cursor read order-tracking beads **and** graph
  wisp roots; `HasOpenWork` walks a subtree with mail/nudge children), the edge
  method takes `(OrdersStore, GraphStore)` etc.; the walk stays bead-shaped
  **inside** the edge and only a typed verdict escapes. Never rebase such a read
  onto a single class store (that plants the single-store-assumption bug the
  graph-store-split audit root-caused).
- `store.List` returning a fixed shape cannot serve all callers: `ListAll(opts)`
  carries at least `IncludeClosed`, `Sort`, `Live`, `Limit`. Pin the reconciler
  union semantics (`type`+`label`+`IsSessionBeadOrRepairable`, closed-excluded)
  with a characterization test against `ListAllSessionBeads` before any consumer moves.

## 4. Write model

**`Store.save()` as intent. Never `obj.save()` (Active Record). Never autosave.**
The domain object stays a pure value with zero persistence coupling; I/O is
explicit and confined to the edge.

- **`store.ApplyPatch(handle, patch) -> refreshed domain object`** for
  high-cardinality partial writes (the reconciler does ~57–61/tick). It returns
  the **LOCAL fold** via the existing `Info.ApplyPatch` projection
  (`internal/session/info_apply_patch.go`), **never a re-`Get`** (a re-Get per
  patch blows the tick budget under Dolt, ~2s/op). **Exception:** status-*close*
  transitions are documented as not foldable → they keep a `Store.Get` refresh.
- **Typed intent methods** for well-known lifecycle ops: `SetState`, `Sleep`,
  `MarkFailed(runID, outcome, cursor)` (one Update — `SetOutcome`+`SetCursor` as
  two writes is NOT equivalent), `CloseRuns(ids, reason)`, `SweepStale`.
- **Count-returning whole-operation methods** for retention/GC sweeps
  (`SweepReadMessagesBefore -> int`, `StaleShadowsBefore`, `ClosedRunsForRetention`
  + `DeleteRun`). The sweep LOOP moves **inside** the edge because retention
  vocabulary (`close_reason`, wisp-tier delete, terminal keys) is deliberately not
  on the domain object. Preserve cross-phase shared budgets and dry-run/sweep parity.

## 5. Boundary metric + exemption census

**Target: zero `beads.Bead` in typed-class DECISION paths, with a checked-in
exemption census.** Absolute zero is not achievable and is not promised — some
interior machinery is legitimately class-generic and holds typed-class beads raw
forever:

- **Work/Graph business logic** (Bead is the domain object).
- **Generic event wire** (`internal/api` `BeadEventPayload` carries `Bead` for all classes).
- **Policy-store class router** (`cmd/gc/bead_policy_store.go` — calls `Classify`/
  `ClassifyGraphPlan`, must see raw beads to route them).
- **By-id federation / observability** (`findBeadAcrossStores`, `collectBeadsAcrossStores`,
  `gc bead show`; `CityBeadStore` stays the documented federation/by-id root).
- **Doctor / diagnostic lanes** (`doctor_*` — diagnose the raw substrate below the domain).

These files hold raw beads but **never call the per-class codecs**, so the compiler
endgame (§6) still lands.

## 6. Enforcement (`cmd/gc/typedclass_edge_guard_test.go`)

Same `runtime.Caller` file-scan style as `frontdoor_di_guard_test.go`. Three tiers:

1. **Codec-census ratchet** (lands FIRST, as the baseline). Scan non-test `.go` in
   `cmd/gc`, `internal/api`, `internal/worker`, `internal/dispatch` EXCLUDING the
   edge set; count per-file typed-class codec/raw-export needles; compare EXACTLY
   against a checked-in `map[file]count`. Any **increase / new file fails**; any
   **decrease fails until the census is ratcheted down** (progress recorded, never
   silently regresses).
2. **Bead-free file lists** — grows per converted file (like `frontDoorStoreFreeFiles`);
   `beads.Bead` must not appear at all. Mixed work+session files
   (`session_reconciler.go`, `build_desired_state.go`, `order_dispatch.go`) stay
   OFF this list with in-code censuses (a substring guard can't tell a work bead
   from a session bead).
3. **Compiler endgame** — when a class's census hits zero, unexport its codec
   (`InfoFromPersistedBead` → `infoFromPersistedBead`, `PersistedResponseFromBead`
   → `persistedResponseFromBead`, `PollerKeyFromBead` → `pollerKeyFromBead`,
   `WaitInfoFromBead` → `waitInfoFromBead`, `DecodeShadow` deleted,
   `RunFromTrackingBead` → `runFromTrackingBead`); cracking a typed bead in the
   interior becomes untypeable. (These exact names are also the WI-0 census
   needles — keep them in sync.)

Header documents the permanent exemptions from §5.

## 7. Invariants to preserve

- **#4017 relocation seam:** domain stores are built FROM `resolve*Store` outputs
  (wrapping the exact cached store value, never a re-wrapped instance) so create-side
  capability assertions (`GraphApplyFor`/`HandlesFor`/`Counter`) keep passing.
  Domain stores are stateless one-field wrappers → per-call construction at the
  front doors is safe.
- **Front-door flip touches both** `cmd/gc/class_store.go` accessors AND
  `api.State`'s typed accessors (15+ `internal/api` sites), in one motion per class.
- **No typed `Get` for another class's handle.** Handles always arrive WITH class
  context (typed endpoint / typed list / class-known reference like nudge
  `Reference{waitBeadID}`), so per-class `Get` never needs class discovery.
  Class-ambiguous by-id stays on the exempt federation surfaces.
- **Graph-plan cross-class embedding:** `ClassifyGraphPlan` routes an entire plan
  to `ClassGraph` if any node is graph-marked, so a bead's class does not determine
  its physical store for graph-plan-created beads; parent/child edges cannot span
  stores. Membership/by-id reads must not assume class ⇒ store.
- **Manager vs Store:** `session.Store.Get` returns PERSISTED `Info`; live
  enrichment (Attached, transport, runtime-downgraded state) requires `Manager` and
  cannot be mechanically swapped — persisted reads → Store, live reads → worker
  Handle/Manager keyed by handle.
