# Reconciled Audit: Infra-Class Data Reached Through the Wrong Store

Findings are tracked as beads (`ga-*` ids below); the fixes land incrementally,
so treat a finding as open unless its bead is closed. `/tmp/recon-audit` was the
scratch export the sweep ran against and no longer exists — read the paths below
against the repo.

**Target:** `origin/main` @ `a4e4cc2bfa`, exported read-only to `/tmp/recon-audit` via `git archive`. No checkout in shared clones, no writes, no city/Dolt/tmux contact.

## Ground truth I re-verified before adjudicating anything

| Fact | Source | Why it decides verdicts |
|---|---|---|
| Only `storageSplitNone` and `storageSplitWhole` are servable; anything else is `storageSplitUnsupported` and refuses to boot | `cmd/gc/storage_boot.go:117-155` | Cross-**infra** class mixing is byte-identical in every servable config. The only defect axis is **work ↔ infra**. |
| `openStorageRoutes` opens ONE engine and assigns the same store to every class | `cmd/gc/storage_boot.go:604-638` | Confirms the above at the assignment site. |
| Both binding providers mint ids under `ReservedClassPrefix(BeadClassGraph)` = `"gcg"` — regardless of class | `storebinding/sqlite/beads_engine.go:71-75`, `storebinding/beadsworkspace/engine.go:60-64` | Every relocated bead is `gcg-*`. This alone decides the `api_state.go` prefix-scan findings. |
| Reserved prefixes: graph `gcg`, messaging `gcm`, sessions `gcs`, orders `gco`, nudges `gcn`; **work has none** | `internal/config/reserved_prefixes.go:16-22` | A work-prefix scan can never match a relocated id. |
| Classifier precedence: wisp(¬order-tracking) → message/extmsg → order-tracking → session/type=session → wait → nudge → synthetic convoy → workflow-metadata → **convergence** → work | `internal/coordclass/classify.go:137-177` | `Type:"convergence"` is **ClassGraph**. Every class claim below cites its arm. |
| Neither provider returns a `*beads.BdStore` | same engine files | Decides the `scopedStoreLike` acquittal. |

---

## 1. Merged site table

Ranked: stranded-write → silent-empty → silent-stale → latency-only. **32 MISROUTED, 4 AMBIGUOUS, 11 CORRECT.**

### A. STRANDED-WRITE — a class bead is CREATED through a work handle

| # | file:line | Class (classifier arm) | Reaches | Verdict | Consequence |
|---|---|---|---|---|---|
| 1 | `cmd/gc/convergence_tick.go:73` | Graph (`:172` typeConvergence) | `cr.cityBeadStore()` — city **work** | **MISROUTED** | stranded-write, **boot-fatal** |
| 2 | `cmd/gc/convergence_store.go:323` | Graph (`:172`) | `a.store` (from #1) | **MISROUTED** | stranded-write, **boot-fatal** |
| 3 | `cmd/gc/convergence_store.go:197` | Graph (`:152` wisp kind / `:170` root_bead_id) | `a.store` (from #1) | **MISROUTED** | stranded-write, **boot-fatal** |
| 4 | `cmd/gc/convergence_tick.go:81` (scope built `:85`) | Graph (`:172`) | `cr.rigBeadStores()` — rig **work** | **MISROUTED** | stranded-write, *not* boot-fatal (city containment never reads rig stores) |
| 5 | `cmd/gc/cmd_handoff.go:354` (openers `:149`, `:200`) | Messaging (`:155` typeMessage) | work store as beadmail's **message** leg; session leg IS routed | **MISROUTED** | stranded-write + message never delivered |
| 6 | `cmd/gc/cmd_sling.go:1673` (opener `:506`) | Nudges (`:163` gc:nudge) | `deps.Store` — work | **MISROUTED** | stranded-write, shadow never terminalized |
| 7 | `cmd/gc/cmd_github.go:393` (opener `:32-33`, `:259`) | Graph (`:189-196` workflow metadata) | `openStoreAtForCity(scopeRoot,…)` — work | **MISROUTED** | stranded-write, PR-repair molecule never dispatched |

### B. SILENT-EMPTY — a class read returns nothing and the caller acts on it

| # | file:line | Class | Reaches | Verdict | Consequence |
|---|---|---|---|---|---|
| 8 | `cmd/gc/pool_session_name.go:533` (callers `city_runtime.go:2358`, `cmd_start.go:1005`) | Sessions (`:159` gc:session) | city/one-shot **work** store | **MISROUTED** | silent-empty → **live agents' work released out from under them** |
| 9 | `cmd/gc/api_state.go:768` `beadEventConfiguredStoreLocked` | all five infra | HQ+rig **work** prefixes only | **MISROUTED** | silent-empty → all-stores fallback |
| 10 | `cmd/gc/api_state.go:748` `beadEventStoresLocked` | all five infra | `cs.beadStores` + `cs.cityBeadStore` — **routes store never a candidate** | **MISROUTED** | silent-empty; the binding's `CachingStore` never gets `ApplyEvent` |
| 11 | `cmd/gc/api_state.go:703` `autocloseStoreRefLocked` | all five infra | work-store refs only | **MISROUTED** | silent-empty (`storeRef == ""`) |
| 12 | `cmd/gc/cmd_mail.go:1721` `cmdMailSendJSON` | Sessions (`:159`) | `openStoreAtForCity` — work | **MISROUTED** | silent-empty → every recipient rejected |
| 13 | `cmd/gc/cmd_mail.go:1735` `listLiveSessionMailboxesCached` | Sessions | work (from #12) | **MISROUTED** | silent-empty allowlist |
| 14 | `cmd/gc/cmd_mail.go:982` `resolveMailIdentityWithConfigCached` | Sessions | unrouted param | **MISROUTED** | silent-empty → "no sender identity resolved" |
| 15 | `cmd/gc/cmd_mail.go:961` / `:947` `resolveMailIdentityCached` | Sessions | unrouted param | **MISROUTED** | silent-stale (retained copy resolves) |
| 16 | `cmd/gc/cmd_mail.go:1243` / `:1229` `resolveMailTargetsCached` | Sessions | unrouted param (**fallback leg** after routed `:1202` misses) | **MISROUTED** | silent-stale → mail to a retired mailbox |
| 17 | `cmd/gc/cmd_mail.go:1263` `resolveMailTargetsForCommand` | Sessions | `openCityStore` — work | **MISROUTED** | silent-empty |
| 18 | `cmd/gc/cmd_mail.go:1286` `resolveDefaultMailTargetsForCommand` | Sessions | `openCityStore` — work | **MISROUTED** | silent-empty |
| 19 | `cmd/gc/cmd_mail.go:1376` `openMailTargetStore` | Sessions | `openStoreAtForCity` — work | **MISROUTED** | silent-empty |
| 20 | `cmd/gc/cmd_mail.go:2193` / `:2201` `gc mail reply` | Sessions | work (both arms) | **MISROUTED** | silent-empty |
| 21 | `cmd/gc/cmd_wait.go:715` | Nudges (`:163`) | `openCityStoreWithPath` — work; **routed `sessFront` on line 713** | **MISROUTED** | silent-empty → duplicate nudge re-delivery |
| 22 | `cmd/gc/cmd_order.go:2116` sweep (write arm) | Nudges + Messaging | `openStoreAtForCity` — work | **MISROUTED** | silent-empty/stale → unbounded growth, green sweep |
| 23 | `cmd/gc/cmd_order.go:2098` sweep (dry-run arm) | Nudges + Messaging | same work store | **MISROUTED** | operator preview agrees with the broken run |
| 24 | `cmd/gc/wisp_step_inject.go:46` (`:48`, `:56`) | Graph (`:152`, `:170`) | work | **MISROUTED** | silent-empty → agent runs with no step context, nothing logged |
| 25 | `cmd/gc/doctor_session_model.go:153-154` | Sessions (`:159-160`, literal `session.BeadType` / `session.LabelSession`) | `openStoreForCity` factory — work | **MISROUTED** | silent-empty → doctor reports HEALTHY |
| 26 | `cmd/gc/doctor_work_option_metadata.go:109` | Sessions | same factory — work | **MISROUTED** | silent-empty **and mutates the wrong copies** |
| 27 | `cmd/gc/cmd_graph.go:116` (rig arm `:105`) | Graph | `openStoreAtForCity` — work | **MISROUTED** | silent-empty → "No beads to graph", exit 0 |
| 28 | `cmd/gc/convergence_tick.go:563` startup reconcile | Graph (`:172`) | `scope.store` — work | **MISROUTED** | silent-empty → interrupted loops never resume |
| 29 | `cmd/gc/convergence_store.go:41` `populateIndex` (+ `:304`) | Graph (`:172`) | `a.store` — work | **MISROUTED** | silent-empty → **concurrency cap fails OPEN** |
| 30 | `cmd/gc/cmd_converge.go:825` `openConvergeStore` | Graph (`:172`) | `openStoreAtForCity` — work | **MISROUTED** | silent-empty/stale operator surface |
| 31 | `cmd/gc/cmd_converge.go:334` / `:359` `--all-rigs` | Graph | work per scope | **MISROUTED** | silent-empty; distinct constructor, survives a fix to #30 |
| 32 | `cmd/gc/wisp_autoclose.go:55` **and** `:63`; `cmd/gc/molecule_autoclose.go:80` **and** `:88` | Graph | omitted variadic ⇒ `graphStore = store` (work) | **MISROUTED** | silent-empty; **reachability-limited** (see below) |

### C. AMBIGUOUS

| file:line | Class | Why not CORRECT, why not MISROUTED |
|---|---|---|
| `cmd/gc/cmd_wait.go:1000`, `:1142` | Nudges | Same unrouted `beads.NudgesStore{Store: store}` shape as #21, but **no non-test caller**. Latent, not live. |
| `cmd/gc/order_store.go:97` `openCityOrderStore` | Orders | Stamps `beads.OrdersStore` on an unrouted work handle with no `relocatedOrdersClassStore` leg. **No non-test caller** — a mis-shaped constructor waiting for its first user. |
| `cmd/gc/build_desired_state.go:1113` `collectAllOpenSessionInfos` | Sessions | City leg IS routed (`city_runtime.go:3400`); **rig legs are rig work stores**. `city_runtime.go:3395-3399` names this shape deliberately, and rigs are a different *scope* the city `[storage]` binding does not cover. Real unbudgeted per-tick cost. |
| `cmd/gc/cmd_graph.go:105` (rig arm) | Graph/Work | Adjudicated with `:116` below. |

---

## 2. Adjudicating the disputed sites

### `cmd/gc/order_dispatch.go:447` — by-accessor CORRECT vs by-symptom MISROUTED

**Adjudication: CORRECT (routing) / latency-only — but this is NOT "no action needed."**

Evidence, not vote-counting:

1. `gateStoresFor` (`:1620-1636`) appends **both** infra legs on top of the work scope: `m.graphStoreFor(store)` → `resolveGraphStore`, `m.ordersStoreFor(store)` → `resolveOrderStore`, each guarded by `storeListContains` identity dedup. So ClassOrders evidence **does** reach the orders binding. There is no silent-empty here.
2. The work leg is **positively required**, not residual. `appendOrdersSweepStore`'s doc (`order_store.go:687-692`) states the copy-and-retain consequence directly: *"Trading the work half for the binding half would leave those beads open forever."* Pre-cutover tracking beads live in the work ledger.
3. A retained stale copy **cannot** corrupt the cooldown clock: `lastRunFunc` (`:902-931`) computes `if last.After(latest) { latest = last }` — a max across gate stores. Older retained copies lose.

So `storeFn` returning the target **work** scope is the correct thing for that function to return; class routing is correctly layered on by its consumer.

**But by-symptom's severity framing is the one that matters operationally.** The measured production cost is real: the work leg is read for ClassOrders evidence on every order every tick, `m.cachedLastRun` memoizes only `lastRun` (not `hasOpenTracking`, not the event cursor), and on a Postgres-backed work class that is the ~50-minute rotation in the brief. I am recording this as **CORRECT/latency-only with a mandatory performance bead** — the fix is leg ordering + cache coverage, not re-routing.

> Calibration note: the brief itself describes `:447`'s live cost as latency (`~7.5s of lastRun reads`, `~50 minutes`), not as wrong data. Both lenses agreed on the consequence; only the label differed.

### `cmd/gc/cmd_graph.go:116` — by-accessor/by-symptom MISROUTED vs by-inverse AMBIGUOUS (undeclared dispute)

**Adjudication: MISROUTED / silent-empty.**

The reconciliation summary filed `:105` as single-lens and `:116` as two-lens, obscuring that all three lenses were judging one function with two verdicts. by-inverse's caution was that `gc graph` is *documented* for work-bead dependency graphs, which would make the work store right.

That defense fails on in-tree evidence. `by_id_store_route.go:74` `classRoutedStoreForID` exists specifically for this, and its own header says answering *"which store owns this bead?"* a second time *"is how this repo's split-store bug class reproduces (#5125, #5127)."* Its **only** non-test callers are `cmd_formula.go:726` and `:1465`. `gc graph` accepts arbitrary ids including `gcg-*`, `slingDirForBead` knows only work prefixes, and the failure is `resolveGraphInput` → 0 beads → `"No beads to graph"` → **exit 0** (`:157-169`).

Per the brief's rule I cannot state positively that the work store is right for *every* id this command accepts. The failure is concrete and demonstrable, so MISROUTED, not AMBIGUOUS.

### Anchor drift reconciled (same defect, different line)

`convergence_store.go:323`≡`:325` · `convergence_tick.go:81`≡`:85` · `wisp_autoclose.go:55/:63` (call sites) vs `:84` (variadic default) · `molecule_autoclose.go:80/:88` vs `:123-125` · `city_runtime.go:2358` (by-accessor said `:2360`). I anchor at the **call site** in each case, because that is where the fix goes.

---

## 3. Re-examining every CORRECT — the false-CORRECT audit

Each must survive a positive statement. I re-derived all eleven.

| Site | Positive justification | Held? |
|---|---|---|
| `cmd/gc/cmd_nudge.go:940` | Sole production entry `deliverSessionNudge:784` does `store := openNudgeBeadStore(...)`, whose body is `beads.NudgesStore{Store: resolveNudgesStore(cliStorageRoutes(cityPath), …)}` (`nudge_beads.go:33`). Value flows `:794 → :812 → :924` with no rewrap. The `beads.NudgesStore{}` at `:940` wraps an **already-resolved** handle. | **CORRECT** |
| `cmd/gc/order_dispatch.go:1631` `gateStoresFor` | Both infra legs resolved and appended with identity dedup; the union is the right answer because the gate must see work-scope beads *and* the infra evidence its own dispatch writes. | **CORRECT** |
| `cmd/gc/order_store.go:556` / `orderTrackingFrontDoor:678` | `orderTrackingFrontDoor` body is literally `resolveOrderStore(cliStorageRoutes(cityPath), scopeStore.Store, …)`. Retained work-scope legs are the deliberate pre-cutover drain (`:687-692`). | **CORRECT** |
| `cmd/gc/city_runtime.go:1691` `orderTrackingSweepStores` | `:1712` appends `cr.relocatedOrdersStore()` with an in-code reason; both halves of a split backlog are swept. | **CORRECT** |
| `cmd/gc/extmsg_binding_reaper.go:31` | Reads `gc:extmsg-binding` (ClassMessaging, `:155`) through `cr.sessionsBeadStore()`. Safe **only** by topology: `storageSplitWhole` makes sessions and messaging the identical store object, and any other shape refuses to boot. | **CORRECT (topology-conditional)** |
| `internal/api/handler_status.go:549` | `scopedStoreLike` → `bdStoreBacking(existing)`; `if !ok { return nil, nil }` (`scoped_store.go:104-106`). Unwraps only to `*beads.BdStore`. Neither provider returns one (`OpenSQLiteStore` / `OpenNativeDoltStoreAtWithoutAmbientEnv`). On a split city the branch is skipped and the routed store survives. | **CORRECT** |
| `internal/api/handler_beads.go:90` | Both read and the **create** arm (`:107-113`) take `s.state.SessionsBeadStore()`. This is the pattern `cmd_sling.go:1673` violates. | **CORRECT** |
| `internal/api/huma_handlers_mail.go:200` (+`:339`,`:419`) | `CityBeadStore()` feeds only `cacheLiveOr503()` / `cacheAgeSeconds()` — liveness probes, never a bead read. Data goes via `state.MailProvider(rig)` ← `newCityMailProvider` (`class_store.go:414-417`), which routes. | **CORRECT** |
| `internal/api/huma_handlers_beads.go:387` `beadListFanOut` | Confirmed: appends `relocatedGraphStore(state)` as a final leg. Covers all five classes **only because** `storageSplitWhole` is the sole split shape. | **CORRECT (topology-conditional, expiry noted)** |
| `internal/api/dashport_support.go:253` | `//go:build integration`; seeded mem stores, no `[storage]`, nothing to relocate to. | **CORRECT (not production)** |
| `cmd/gc/class_store.go:146` `ordersBeadStore` | Not a misroute — a **structural gap**. Verified **zero** production callers (also `cityWorkStore()` at `:88` and `:166`). Recorded because accessor *existence* being mistaken for class *coverage* is precisely how the prior sweep cleared `:447`. | **CORRECT (wire it or delete it)** |

**Downgrades applied:** none of the eleven collapsed, but two are now explicitly **topology-conditional** (`extmsg_binding_reaper.go:31`, `huma_handlers_beads.go:387`). They are correct *today* and become defects the moment per-class fan-out is served — which `by_id_store_route.go:109-116` independently flags as the same latent dependency.

**One CORRECT I promoted to a verified acquittal that no lens had actually checked:** `order_store.go:112` `openOrderStoreForOrder` has the same mis-shaped `beads.OrdersStore{Store: <work>}` body as the AMBIGUOUS `:97`, but **two real callers** (`cmd_order.go:698`, `:715`). I traced them: the tracking bead goes through `orderTrackingFrontDoor` (routes), and the molecule goes through `resolveGraphStore` + `moleculeClassStore` (`cmd_order.go:849-851`). Genuinely CORRECT — but it was asserted, not proven, by the one lens that mentioned it.

---

## 4. Coverage honesty — what NO lens covered

The four lenses were dense in `cmd/gc` and partial in `internal/api`. **Everything else is essentially unswept.**

**Not covered by any lens:**
- **`internal/session/manager.go` + `session_reconciler.go` + `session_beads.go`** — ~100 `sessionFrontDoor(store)` sites receiving the store as a spine parameter. by-class explicitly punted; nobody else looked. Two constructors were spot-checked (`internal/worker/factory.go:66`, `internal/api/session_manager.go:11`); the spine itself has **no provenance proof**.
- **The full `isWorkflowMetadata` carrier population** — *any* bead with `gc.root_bead_id` in metadata or MetadataRefs is ClassGraph (`classify.go:189-196`). That spans `internal/formula`, `internal/molecule`, `internal/dispatch`, `internal/graphv2`, `cmd/gc/cmd_convoy_dispatch.go`. Sampled, never enumerated. **This is the single largest hole.**
- **Whole packages nobody swept:** `internal/worker`, `internal/dispatch`, `internal/sling`, `internal/convoy`, `internal/formula`, `internal/molecule`, `internal/graphv2`, `internal/mail` (beyond beadmail constructors), `internal/extmsg` (beyond the reaper), and **all of `pkg/`**.
- **`internal/api`:** `handler_convoy_dispatch.go`, `huma_handlers_convoys.go`, `api_state.go:1526` (`ScopedStoreLike`), and most of `handler_beads.go`.
- **Named-but-unswept `cmd/gc` sites:** `cmd_hook.go:524`, `service_runtime.go:61`, `work_record_gate.go:200`, `cmd_events_reemit_execution.go:142`.

**What a fifth method must be: dynamic, not static.**

All four lenses were static traces, and static tracing cannot prove a negative over store values that arrive through struct fields and closures — which is exactly how the convergence subsystem (three frames from its accessor) and `cmd_handoff`'s unnamed positional arg evaded three of four lenses.

The fifth method should be a **class-tag conformance harness**: wrap every store handle at its resolution point with the class set it was resolved *for*, then assert on every `Create`/`List`/`Get` that `coordclass.Classify(bead)` ∈ the handle's tag. Run the full suite plus a converged split fixture city under it. It finds sites by **execution**, so it covers the packages nobody read, and it cannot be fooled by a `beads.SessionStore{}` wrapper around an unrouted value — the wrapper is a type, the tag is a provenance fact.

Cheap static complements (both would have caught most findings above):
1. An AST boundary test forbidding `beads.{Graph,Orders,Session,Mail,Nudges}Store{}` construction from any expression not derived from a `resolve*Store` / `*BeadStore()` call.
2. Make the autoclose graph store a **required** parameter, not a variadic with a work-store default. The variadic is precisely what let four call sites silently opt out.

Note also that `cmd/gc/frontdoor_di_guard_test.go:361-384` `sessionRelocationRoutedFiles` lists 22 files and **omits `cmd_mail.go`, `cmd_doctor.go`, and every `doctor_*.go`** — exactly where findings #12-20 and #25-26 landed. The test's own comment concedes it is "a regression canary … not a completeness proof." It is currently being read as the latter.

---

## 5. The two known defects — calibration

**`cmd/gc/order_dispatch.go:447` — APPEARS. Both by-accessor and by-symptom found it independently, and they disagreed on the verdict, which is what surfaced it for adjudication.** Adjudicated CORRECT-routing / latency-only above (§2), because `gateStoresFor` now federates both infra legs — remediation landed after the prior sweep. The latency cost the brief measured is real and survives.

**`cmd/gc/city_runtime.go:673/684/1313/1347` — CONFIRMED FIXED on current main. Not carried forward.** Verified directly:
- `buildDesiredState` (`:3400`): `sessionsStore := cr.sessionsBeadStore()` → passed as the build-fn leading store.
- `refreshDesiredState` (`:3424`): `cr.sessionsBeadStore().Store`.
- The doc comment at `:3407-3414` now states the reason explicitly: *"it is the SESSIONS store, not the work store; on a relocated city the work store would take a session-class create the class binding never sees and the boot containment re-check names."*
- The tick-level callers at `:660-667` and `:1304-1314` route through `cr.sessionsBeadStore()`.

**No lens missed either.** All four independently re-verified the `city_runtime.go` sites as fixed and declined to re-report — correct behavior. **This is a positive calibration signal**, with one caveat: three of four lenses reported it as *"the prior sweep's failures are already remediated,"* which is only true for these two. It should not be read as evidence the class is closed — this sweep found **7 new stranded-writes**, including an entire subsystem.

**The calibration signal that matters more:** the convergence subsystem (findings #1-4, #28-31 — 8 sites, 3 of them boot-fatal) was invisible to `by-accessor`, the method the prior sweep used, because it **never calls a named work accessor**. `by-accessor` traced 282 sites exhaustively and still missed it. That is direct evidence that accessor enumeration is structurally incapable of closing this bug class, exactly as the brief warned.

---

## 6. The fix list, ordered

### Tier 0 — its own bead, highest priority: the convergence subsystem has no class seam
Findings #1-4, #28-31. This is **not** a set of missed call sites; it is a subsystem that predates the split and was never wired to `coordclass`. Verified: **zero** occurrences of `resolveGraphStore`, `graphBeadStore`, `moleculeClassStore`, or `cliStorageRoutes` in `convergence_tick.go`, `convergence_store.go`, or `cmd_converge.go`.

Not a mechanical substitution — it needs a **routing decision** first, because of #4: rig-scoped convergence beads are ClassGraph, but the graph binding is city-level and singular. Either rig convergence scopes route to the one city graph binding (changing which store rig loops live in), or rig convergence is declared work-class. That decision has to be made before any line changes. The controller tick is a live production path.

### Tier 1 — own bead each, stranded-write, non-mechanical
- **#5 `cmd_handoff.go:354`** — route the message leg via `resolveMailMessagesStore`. Exemplars: `providers.go:899-901`, `prime_auto_handoff_inject.go:115-117`. **Delete the comment at `:349-353` in the same commit** — it asserts routing the code does not perform and is a live false-CORRECT trap for the next reader.
- **#7 `cmd_github.go:393`** — `openGitHubPRRepairStore` needs a graph leg for the `CookOn` pour; the work store must stay for the parent bead read. Two-store split, same shape as `cmd_order.go:849-851`.
- **#9/#10/#11 `api_state.go:703,748,768`** — the prefix scan needs reserved-class-prefix candidates *and* `beadEventStoresLocked` must include the routes store. Currently the binding's `CachingStore` **never receives `ApplyEvent`** — that is a cache-coherence bug beyond the autoclose symptom. Needs a design call about lock ordering (the in-code comment says the accessors take `cs.mu`, which is why raw fields are read here).

### Tier 2 — safe to batch, mechanical accessor substitution
All are "wrap the existing store in the resolver that already exists," with an in-tree exemplar:

| Batch | Sites | Substitution |
|---|---|---|
| **Nudges** | #6 `cmd_sling.go:1673`, #21 `cmd_wait.go:715`, #22/#23 `cmd_order.go:2098,2116` | `resolveNudgesStore(cliStorageRoutes(cityPath), …)` — exemplar `nudge_beads.go:28-33`; controller twin `city_runtime.go:1663-1664` |
| **Mail sessions** | #12-#20 (`cmd_mail.go` `:947,961,982,1229,1243,1263,1286,1376,1721,2193,2201`) | `cliSessionStore(store, cfg, cityPath)` — exemplar is **in the same file** at `:1202`. One cluster, one root cause; fix the openers and the three leaf resolvers together. |
| **Doctor sessions** | #25 `doctor_session_model.go:153`, #26 `doctor_work_option_metadata.go:109` | Route the `openStoreForCity` factory at `cmd_doctor.go:277`. **Do #26 first** — it mutates. |
| **Autoclose graph store** | #32 `wisp_autoclose.go:55,63`; `molecule_autoclose.go:80,88` | Pass the graph store. **Better: make the parameter required** and delete the variadic — the default is the defect. Exemplar `api_state.go:744-745`. |
| **Converge CLI reads** | #30 `cmd_converge.go:825`, #31 `:334,:359` | Falls out of Tier 0's routing decision; sequence after it. |

### Tier 3 — needs a routing decision, own bead
- **#8 `pool_session_name.go:533`** — highest *operational* severity in Tier 3: it releases live agents' work. `cmd_start.go:982-986` **already documents this as a known open gap** ("a shared work-release-boundary follow-up"). The decision is whether the liveness check reads sessions-only or federates sessions+work during the drain window; both callers (`city_runtime.go:2358`, `cmd_start.go:1005`) must change together.
- **#24 `wisp_step_inject.go:46`** — needs the graph leg; silent prompt-injection loss is the worst observability shape in this set.
- **#27 `cmd_graph.go:116`/`:105`** — wire `classRoutedStoreForID`. The seam exists and is used by `cmd_formula.go` only; extending it is the intended fix, but `gc graph` fans over *sets*, not one id, so the resolver needs a multi-id shape.

### Tier 4 — latency and hygiene, no correctness change
- **`order_dispatch.go:447`** — performance bead. Order the infra legs first, extend `m.cachedLastRun` coverage to `hasOpenTracking` and the event cursor. **Do not "fix" the routing** — the work leg is required by copy-and-retain.
- **`build_desired_state.go:1113`** — budget the unbounded per-tick per-rig `ListAll`.
- **`order_store.go:97`**, **`cmd_wait.go:1000/:1142`**, **`class_store.go:146`** — dead or latent mis-shaped constructors. Wire them correctly or delete them; a zero-caller class accessor is how the prior sweep mistook existence for coverage.

### Tier 5 — the guards, which are worth more than another manual sweep
1. The dynamic class-tag conformance harness (§4).
2. The AST boundary test on typed-class-store construction.
3. Two cheap CI invariants that `by-accessor` derived empirically and that each caught real defects here: **(a)** flag a routed and an unrouted derivation of the same store on adjacent lines (caught #6, #8, #21, and the `cmd_mail.go` cluster); **(b)** flag controller/CLI twin divergence where the watchdog routes and the `gc` command does not (caught #22/#23).

---

**Files referenced (absolute):** all paths above are repo-relative to `/tmp/recon-audit`, a read-only `git archive` export of `origin/main@a4e4cc2bfa`; they correspond 1:1 to the shared clone at `/var/tmp/upstream-prs`. Nothing was written, checked out, or built.
