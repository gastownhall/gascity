## refactor: strong-typed per-class store seam — the compiler enforces which class owns which bead

### What

Introduces a **compile-time, per-coordination-class store seam** so a future relocated / per-class bead backend becomes a change at **one resolution point** rather than at scattered call sites — and so the compiler, not reviewer discipline, enforces that a bead operation of one class can never be handed another class's store. On `main` there is a single store, so every typed store wraps the *same* underlying store value and behavior is **byte-identical**; this PR is purely the structural, statically-enforced boundary.

#### Named-struct typed stores in `internal/beads`

`internal/beads/class_store.go` declares six strongly-typed wrappers, each a named struct **embedding `beads.Store`** (field name `Store`):

- `WorkStore`, `GraphStore`, `SessionStore`, `MailStore`, `OrdersStore`, `NudgesStore`

Because each embeds the `Store` interface, every wrapper promotes all `Store` methods and *is* a `Store` for ordinary operations — but a function that takes/returns a statically-known class takes/returns *its* typed store, and the compiler then refuses to let a caller pass a store belonging to a different class.

**Optional capabilities unwrap via `.Store`.** Type assertions for optional capabilities (`Counter`, `GraphApplyStore`, `GraphApplyFor`, `StorageCreateStore`, `Backing`/`ReadyLive`, …) are *not* promoted through the embedding — asserting on a wrapper asserts on the wrapper, not the underlying store. Call sites assert on the embedded `.Store` field, and pass `.Store` when handing a value to a generic `beads.Store` helper shared across classes.

#### The five relocatable classes are strongly typed end-to-end, with real consumers

The five classes a relocated backend can move — **Graph, Session, Mail, Nudges, Orders** — are now typed from the accessor down to their consumers, so a class op cannot be handed another class's store without a compile error:

- per-class accessors on `controllerState` and `CityRuntime` return the typed store: `graphBeadStore() beads.GraphStore`, `sessionsBeadStore() beads.SessionStore`, `mailBeadStore() beads.MailStore`, `nudgesBeadStore() beads.NudgesStore`, `ordersBeadStore(scope) beads.OrdersStore`;
- the `api.State` interface exposes the typed accessors `GraphBeadStore() beads.GraphStore`, `SessionsBeadStore() beads.SessionStore`, `NudgesBeadStore() beads.NudgesStore`, and the API session/nudge/graph handlers consume them as those types;
- the single controller funnel `resolveClassStore(workStore, cfg, cityPath, class, rec)` plus the `resolve{Graph,Session,Mail,Nudges,Order}Store` wrappers select the backing store — **the swap point**.

This **replaces the earlier accessor-by-discipline approach**, where `SessionsBeadStore()` / `NudgesBeadStore()` returned a bare `beads.Store` with *zero typed consumers* (the type system did nothing; correctness depended on every call site choosing the right accessor by convention). Now the consumers are typed, so the choice is checked at compile time.

#### Orders is scope-federated, typed at the single-order boundary

Orders are scoped (city + per-rig/pool). The class is typed exactly where a *single* order resolves to *one* store:

- `resolveOrderStoreTarget` / `openOrderStoreForOrder` / `openCityOrderStore` and the cached resolvers return `beads.OrdersStore`; `doOrderRun*` / `doOrderHistory*` consume `beads.OrdersStore`.
- The **federated reads stay `[]beads.Store` by design** and are preserved byte-identically: the legacy dual-read + multi-scope order-tracking sweep enumerates heterogeneous scopes (`orderTrackingSweepScopedStore` carries a bare `beads.Store` so its structural label/key assertions still resolve), and the cross-store gate/history helpers (`orders.LastRunAcrossStores`, `CursorAcrossStores`, …) are class-agnostic. `unwrapOrdersStores` converts the typed per-order slice to `[]beads.Store` exactly at that federated-read boundary, each element carrying the same underlying store.

#### Work is the default/residual class

Work is the residual class — everything `coordclass.Classify` does not route elsewhere. It now has a typed accessor:

- `cityWorkStore() beads.WorkStore` and `workBeadStores() map[string]beads.WorkStore` (the per-rig work tail) on both `controllerState` and `CityRuntime`; the rig-work-tail consumers (build-desired-state, the session reconciler's work split) take the unwrapped `map[string]beads.Store` via `unwrapWorkStores`, each value the same underlying store.
- **`CityBeadStore()` deliberately stays `beads.Store`.** It is the documented **federation / by-id / default root** — the probe-everything candidate for `storeref` by-id resolution and the federation entry point — distinct from the work class. The ~51 remaining non-test `CityBeadStore()` call sites are federation/by-id/default uses (handler by-id reads, convoy/mail/extmsg/sling handlers, worker factories, the runtime's own `cityBeadStore()` root); they are **not** reclassified, because conflating the federation root with the work class would be high-churn and low-safety. `cityWorkStore()` is the accessor a genuine work-class op uses; today it wraps the same underlying store `CityBeadStore()` returns.

### Runtime — byte-identical

Every typed store wraps the **same underlying store value** the call site already used — never a re-wrapped or reconstructed instance — so on a single-store city every class collapses to that one concrete store, `beads.Store` gains zero methods, and the existing suite passes unchanged. The per-class **identity-conformance** tests assert **pointer equality on the embedded `.Store`** for every accessor (city-resident classes against `CityBeadStore()`, work against `BeadStores()`/`rigBeadStores()`).

Two leaf packages anchor the routing axis:

- **`internal/coordclass`** — the single source of truth for a bead's coordination class: `Classify(bead) Class` / `ClassifyGraphPlan(plan) Class` over `Work` (default), `Graph`, `Messaging`, `Sessions`, `Orders`, `Nudges`. This is a *routing* axis, distinct from the storage-tier policy (`policyNameForBead` stays in `cmd/gc`, keyed on the fine policy name; tiers unchanged).
- **`internal/storeref`** — by-id resolution across an ordered `[]beads.Store`: `PrefixOwner` (id-prefix → owning store) + `Resolve` (probe federation, first hit wins).

### Why now

This establishes the **one swappable, compiler-checked boundary** for "which store owns this bead class / id" — the minimal seam a per-class or relocated backend needs — while `main` stays single-store. **No backend is added here.** This is the structural template the multi-backend fork rebases onto: a relocated / per-class backend plugs in at `resolveClassStore` (its body goes from `return workStore` to the per-class dispatch) and, for orders, returns the typed scoped store from the per-order resolver — **with no call-site change**, because the call sites are already typed against the class. The seam's API deliberately matches the downstream multi-backend branch so that work rebases onto this template rather than carrying a parallel architecture.

### Out of scope (deliberate)

No per-class/relocated backend, no create-time cross-store routing, no cross-store convoy wiring, no `Claimer` interface — these have no second implementation on `main` and belong with the backend that justifies them (the fast-follow).
