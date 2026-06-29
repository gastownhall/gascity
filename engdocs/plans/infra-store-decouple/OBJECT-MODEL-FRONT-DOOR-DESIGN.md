# Object-Model Front-Door Design — closing the non-work raw-bead leak

Status: DESIGN (spec only — no production code changed by this document)
Scope: `cmd/gc/`, `internal/api/`, `internal/beads/`, `internal/session/`,
`internal/nudge`/`internal/nudgequeue`, `internal/orders`, prompts/packs
Builds on: PR #3773 (mail + session API/response half promoted to domain objects)
Primary worktree (source of truth for the code cited): `.claude/worktrees/upstream-store-pr`

---

## 1. The principle and the leak it fixes

**Principle.** It is an *interface leak* for any caller — the
controller/reconciler, `internal/api`, the `cmd/gc` CLI, or an agent prompt
running `bd` — to read or write the attributes of a **non-work object**
(session, nudge, mail, order, graph) via raw bead operations:

- `Create(beads.Bead{...})`
- `Get(id)` followed by reading `b.Metadata` / `b.Status` / `b.Title` / `b.Labels`
- `SetMetadata` / `SetMetadataBatch`
- `List(beads.ListQuery{...})` / `ListByLabel` / `ReadyLive`
- `Close` / `CloseAll` / `Update(beads.UpdateOpts{...})`
- `Dep*` / `Children`

Every such op on a non-work object must become a **front-door API**: a typed
domain method (e.g. `sessionStore.SetState(id, Asleep)` instead of
`SetMetadataBatch(id, {state: "asleep"})`), with bead serialization confined
*inside the implementation* — serialize at the edge. Where an **agent** performs
the op from a prompt via `bd`, there must be a `gc` command front door (AGENTS.md:
"if a tool has a CLI the agent uses it"; ZERO hardcoded roles; no raw-bead poking
of non-work objects from prompts).

**What this leak costs us.** Backend-decoupling (Dolt vs sqlite vs Postgres) and
class routing are only sound if every non-work class is read and written through a
single typed seam. Today the class is *statically visible* (#3773 added
`beads.SessionStore`, `beads.NudgesStore`, …) but the seam is **routing-only**:
each wrapper embeds `beads.Store` and promotes every raw op
(`internal/beads/class_store.go:27-79`). So a call site carrying a
`beads.SessionStore` type still invokes `store.SetMetadataBatch(id, {...})`
verbatim. The metadata-key vocabulary (`state`, `wait_hold`, `live_hash`,
`order-run:`, `seq:N`, …) is therefore smeared across `cmd/gc/*.go`, and any
backend-specific serialization choice leaks with it.

**What stays raw by nature (NOT a leak).**

- **work** — the generic task substrate, the federation/by-id root, and the
  HTTP/SSE wire contract are all `beads.Bead`. Work stays bead/raw.
- **graph** — the bead DAG. Its front door is *DAG operations*
  (`ApplyGraphPlan`, typed DAG reads), **not** per-attribute setters. There is no
  per-attribute graph setter to remove.

---

## 2. Layering / object model

The target shape for every non-work class is one pipeline:

```
            caller (controller / internal/api / cmd/gc / gc-command handler)
                              │  speaks ONLY typed domain values
                              ▼
   ┌───────────────────────────────────────────────────────────────────┐
   │  domain type            session.Info / nudgequeue.Item /            │
   │  (crosses the boundary)  mail.Message / OrderRun / GraphApplyPlan   │
   └───────────────────────────────────────────────────────────────────┘
                              │
                              ▼
   ┌───────────────────────────────────────────────────────────────────┐
   │  FRONT-DOOR API / store  typed methods: SetState, Enqueue,          │
   │  (the only public surface) CreateRun, ApplyGraphPlan, …             │
   └───────────────────────────────────────────────────────────────────┘
                              │  CODEC EDGE — serialize at the edge
                              ▼  (MetadataPatch builders / decodeNudgeItem /
                              │   beadToMessage / outcome→label / plan→creates)
   ┌───────────────────────────────────────────────────────────────────┐
   │  typed beads.XStore      beads.SessionStore / NudgesStore /         │
   │  (compile-time class tag) OrdersStore / MailStore / GraphStore      │
   └───────────────────────────────────────────────────────────────────┘
                              │  resolveClassStore / resolve<Class>Store
                              ▼  (single-store identity OR federated routing)
   ┌───────────────────────────────────────────────────────────────────┐
   │  bead substrate          beads.Store (BdStore / NativeDoltStore /   │
   │  (Dolt / sqlite / PG)     caching delegators) — raw bead I/O        │
   └───────────────────────────────────────────────────────────────────┘
```

The **codec edge** is the load-bearing line: above it nothing speaks
`beads.Bead`, `beads.ListQuery`, `beads.UpdateOpts`, or `map[string]string`
metadata literals; below it nothing speaks domain intent. Today only **mail**
fully realizes this. The work of this design is to push the codec edge down for
**session / nudge / order** and (optionally) tighten **graph** reads.

**Method home (recommended, per object).** The typed `beads.XStore` lives in
`internal/beads` (Layer 1). The domain types (`session.Info`, `session.State`,
`nudgequeue.Item`) live in higher layers. Hanging domain methods directly on
`beads.XStore` would force `internal/beads` to import `internal/session` etc. —
a layering inversion. The clean home is a thin domain wrapper *in the object's own
package* that holds the typed `beads.XStore` by value and owns the codec — exactly
the precedent already set by `session.InfoStore`
(`internal/session/info_store.go:75`, holds a `beads.SessionStore` by value). The
`class_store.go` wrapper stays the routing tag; the domain wrapper owns intent.

---

## 3. Per-object designs

### 3.1 session — CLEAN-FRONT-DOOR

**Feasibility.** Both codec halves already exist in `internal/session`; they just
aren't joined behind a typed store.

- **Write codec exists:** `internal/session/lifecycle_transition.go` has ~20
  `MetadataPatch` builders (`SleepPatch`, `DrainAckStopPendingPatch`,
  `AcknowledgeDrainPatch`/`CompleteDrainPatch`, `RestartRequestPatch`,
  `ConfigDriftResetPatch`, `ClosePatch`, `RetireNamedSessionPatch`,
  `QuarantinePatch`, `ReactivatePatch`, `ConfirmStartedPatch`/`CommitStartedPatch`,
  …). They capture domain intent as typed values — but the **write** is still raw
  `setMetaBatch(store, session.ID, patch)` → `store.SetMetadataBatch(...)`
  (`cmd/gc/session_beads.go:1825`).
- **Read codec exists:** `InfoFromPersistedBead` + `InfoStore.Get/List`
  (`internal/session/info_store.go:21-126`) project `beads.Bead → session.Info`
  with zero raw beads escaping, backend-invariant — but it is documented as having
  "no production callers yet."

The fix is mechanical: add typed write+read methods on a `session` domain wrapper
that confine `SetMetadataBatch`/`Update`/`Close`/`Get` inside the impl.

**Front-door API (method → replaces).**

| method | replaces |
| --- | --- |
| `ApplyPatch(id, session.MetadataPatch) error` — the single write primitive | `setMetaBatch(store, session.ID, patch)` at `session_beads.go:1825` and ~20 reconciler sites (`session_reconciler.go:66/378/2122/2136/2293/2367/3388/3404/3436/3674/4049/4157`, `soft_reload.go:146`, `session_circuit_breaker.go:748`) |
| `SetState(id, session.State, reason) error` | the canonical state-heal `SetMetadataBatch(id, {state: asleep\|creating\|…})` in `session_reconcile.go:902-928` (`healState`/`healStateWithRollback`) |
| `Sleep(id, reason, now) error` (wraps `SleepPatch`) | `session_reconciler.go:2293` (max-age), `:2367` (idle-timeout) |
| `BeginDrainAckStopPending(id, now) error` | `session_reconciler.go:66` (`markDrainAckStopPending`) |
| `FinalizeDrain(id, ack, complete, reason, freshWake, now) error` | `session_reconciler.go:378` |
| `RequestRestart(id, sessionKey, now) error` | `session_reconciler.go:1818` |
| `ResetConfigDrift(id, next State, sessionKey, now) error` | `session_reconciler.go:3674`; `soft_reload.go:146` |
| `RecordStartedHashes(id, session.StartedHashes) error` | fingerprint backfill/rebaseline at `session_reconciler.go:2136/2148/2162/4049/4157` |
| `SetSleepIntent` / `ClearSleepIntent` | `session_reconciler.go:2568`; wait-hold intent at `cmd_wait.go:294/1293` |
| `SetWaitHold(id, on, reason) error` | `SetMetadataBatch(sessionID, {wait_hold, sleep_intent})` at `cmd_wait.go:294`; clear at `:1293` |
| `StampMarker(id, key, value)` / `ClearMarkers(id, keys…)` | `:3016` stranded marker; `StampWaitLookupCapMetadata` at `cmd_wait.go:757`; drift-deferral set/clear at `:3388/3404/3436/2122`; circuit clear at `session_circuit_breaker.go:748` |
| `RecordCurrentBead(id, beadID) error` | `recordCurrentBeadIDOnWake` (`session_bead_cycle.go:21`) |
| `GetInfo(id) (session.Info, error)` (promote `InfoStore.Get`) | `store.Get(id)` + read `.Status/.Metadata` at `session_reconciler.go:342`, `controller.go:290`, `cmd_wait.go:228/1115/1289/1389` |
| `GetState(id) (session.State, closed bool, error)` | `Get(id)` + check `Status=="closed"` at `session_reconciler.go:342`, `session_beads.go:2143/2209/2347` |
| `ListInfo(session.ListFilter) ([]session.Info, error)` (promote `InfoStore.List`) | `ListAllSessionBeads` feeding `loadSessionBeadSnapshot` (`session_bead_snapshot.go:85`); `ListByLabel("gc:session")` at `cmd_stop.go:351`; `cmd_mail.go:1085` |
| `Close(id, reason, now) (closed bool, error)` | `closeBead`/`closeFailedCreateBead` at `session_beads.go:2347/2354/2357/1840` |
| `CreateSession(session.CreateSpec) (id, error)` | `Create(beads.Bead{Type:session,…})` + `SetMetadata(session_name)` at `session_beads.go:1184/1203` |
| `Reopen(id) error` / `Retire(id, reason, identity, now) error` / `RepairType(id)` | `Update{Status:open}` at `session_beads.go:347`; retire+archive at `:454/459/520/529`; type-repair at `:908` |

**Replaces ~46 raw bead ops.**

**Codec boundary.** READ: `InfoFromPersistedBead` (side-effect-free,
backend-invariant). WRITE: the `MetadataPatch` builders, with `ApplyPatch` as the
single chokepoint that calls `SetMetadataBatch`. What crosses *in*: `id string`,
`session.State`, `session.MetadataPatch`, typed spec structs
(`CreateSpec`/`StartedHashes`/`ListFilter`). What crosses *out*: `session.Info`,
`session.State`, `ok`/`closed` bools, errors. Zero `beads.Bead`,
`beads.ListQuery`, `{state:…}` literal, or metadata-key constant crosses the
`cmd/gc` boundary.

**Open risks.** (a) `MetadataPatch.Apply` treats empty string as *delete*
(`lifecycle_transition.go:25-28`) — `ApplyPatch` must preserve that on every
backend or heal/clear silently breaks on PG. (b) `loadSessionBeadSnapshot` keys
the whole tick off `b.Metadata[session_name|template|configured_named_identity|
common_name|pool_slot|agent_name]`; `session.Info` lacks
`pool_slot/configured_named_identity/common_name`, so `ListInfo` can only replace
the snapshot if `Info` is extended **or** a richer typed `SessionSnapshot` is
introduced. This is the single largest read-side change.

> Cross-class WORK probes and the close-path work-reassignment trio are **NOT**
> the session front door — see §5.

### 3.2 nudge — CLEAN-FRONT-DOOR (one caveat)

**Feasibility.** The entire raw-bead surface is already collapsed into four leaf
helpers in `cmd/gc/nudge_beads.go` (`find`/`findAny`/`ensure`/`markTerminal`) plus
one rollback at `cmd_nudge.go:1863`, all keyed by `nudge_id`. The domain type
(`nudgequeue.Item`) already exists. ~6 raw ops total.

**Caveat — the bead is a SHADOW.** The flock'd queue file
(`internal/nudgequeue/state.go`, `WithState` over `state.json`) holds the
canonical `[]Item`. The bead exists for observability/event-emission. So the front
door is a thin veneer over `ensure`/`markTerminal`/`find`, NOT a new storage
authority. The methods must stay callable **inside** the `withNudgeQueueState`
transaction (`cmd_nudge.go:1800/1893/1942/2002`) so shadow and authority stay
coherent under one lock.

**Front-door API (method → replaces).**

| method | replaces |
| --- | --- |
| `Save(item nudgequeue.Item) (beadID, created bool, err)` | `ensureQueuedNudgeBead` → `Create(beads.Bead{…})` at `nudge_beads.go:104/132-142`; confines the Item→meta codec (`:115-131`) and label construction |
| `Terminalize(item, state, reason, commitBoundary, now) error` | `markQueuedNudgeTerminal` → `SetMetadataBatch+Close` at `nudge_beads.go:149/167/173`; confines the close-reason floor (`:218`) and BeadID-then-find fallback (`:182-198`) |
| `Find(nudgeID) (Item, bool, error)` | `findQueuedNudgeBead` (`:52`) — existence gate at `cmd_wait.go:1163`; returns a **decoded Item** |
| `FindIncludingTerminal(nudgeID) (Item, bool, error)` | `findAnyQueuedNudgeBead` (`:56`) used to read TERMINAL state at `cmd_wait.go:1230`, `cmd_nudge.go:2157/2179`; returns typed `state/terminal_reason/commit_boundary` so callers stop cracking `nudge.Metadata[...]` (`cmd_wait.go:1241-1256`, `cmd_nudge.go:2164/2180`) |
| `RollbackEnqueue(beadID) error` | `cmd_nudge.go:1863-1866` (`SetMetadata(close_reason)` + `Close`) |
| *(internal)* `decodeNudgeItem(b beads.Bead) (Item, error)` | **THE MISSING HALF OF THE CODEC** — today only Item→Bead exists; `reference_json` is written (`:258`) but never read back |

**Replaces ~6 raw ops.**

**Codec boundary.** Item flows *out* to the bead for observability; the
`Bead→Item` decoder exists only to read **controller-stamped terminal fields**
(`state`/`commit_boundary`/`terminal_reason`) that the queue file does not retain.
A decoded Item is therefore **partial** — queue-only runtime fields
(`Attempts`, `ClaimedAt`, `LeaseUntil`, `DeadAt`, `CreatedAt`) live only in
`state.json`. **Open question:** return a narrower typed view
(`NudgeShadow{ID, State, TerminalReason, CommitBoundary}`) from `Find*` rather than
a half-populated `Item`, so callers can't trust empty `Attempts`/`LeaseUntil`.

### 3.3 mail — CLEAN-FRONT-DOOR (already achieved)

**Feasibility.** Mail is the reference shape. Every message op crosses as
`mail.Message`/`mail.HandoffIntent`/`mail.ArchiveResult` through `mail.Provider`
(`internal/mail/mail.go:88-149`); the codec is confined to
`beadmail.beadToMessage` (`:924`) and `createMessageBead` (`:169`). No caller in
`cmd/gc` or `internal/api` constructs a `Type="message"` bead or reads its
metadata. **replacesRawOps = 0 for the mail object.**

**The residual raw ops in the mail CLI/API are SESSION reads, not mail leaks** —
they resolve a mailbox address from a session bead and belong behind the session
front door:

| method (SESSION front door) | replaces |
| --- | --- |
| `sessionStore.MailboxAddress(identifier) (addr, err)` | `cmd_mail.go:953/975/1172` `Get(sessionID)` + `b.Metadata[alias\|session_name]` (`sessionMailboxAddress` `:903`) |
| `sessionStore.MailboxAddresses(identifier) ([]string, err)` | `cmd_mail.go:1172` (`:913`, alias-history aware); `handler_extmsg.go:113-122` |
| `sessionStore.ListLive() ([]session.Info, err)` | `cmd_mail.go:1085/1092` `ListAllSessionBeads` + `b.Metadata[...]` |

These must be tracked under the session work so mail doesn't "look done" while the
callers still import `session.ListAllSessionBeads`.

### 3.4 order — CLEAN-FRONT-DOOR

**Feasibility.** The order-*tracking* record is a pure non-work coordination
object (`Title "order:<scoped>"`, label `order-tracking`, hand-written outcome
labels `exec`/`exec-failed`/`exec-env-failed`/`wisp`/`wisp-failed`/
`wisp-canceled`/`trigger-env-failed`, the `order-run:<scoped>` attribution label,
and the event cursor encoded as the label pair `order:<scoped>` + `seq:<N>`). None
of the STAYS-BEAD exemptions apply. ~31 raw ops across `order_dispatch.go` +
`cmd_order.go` collapse to a small typed `OrderRun` vocabulary.

The non-trivial part: the dispatcher deliberately exploits bead mechanics —
`CreatedAt` is the cooldown clock; an **open** tracking bead == in-flight
single-flight marker; closed-immediately == cooldown-advance-only — plus a
two-tier (wisps/issues) union. The typed API must **name** these, not hide them.

**Front-door API (method → replaces).**

| method | replaces |
| --- | --- |
| `CreateRun(scoped, RunOpts) (OrderRun, error)` — returns an OPEN run | `order_dispatch.go:557-561` (trigger-env-failed), `:621-625` (normal pre-dispatch) |
| `CreateRunClosed(scoped, RunOutcome, *EventCursor) (OrderRun, error)` — create+close, cooldown-only | `cmd_order.go:752-760` (manual-run tracking), create half of `:805-814` |
| `SetOutcome(runID, RunOutcome) error` — enum {Exec,ExecFailed,ExecEnvFailed,Wisp,WispFailed,WispCanceled} | outcome labels at `order_dispatch.go:1173/1189/1217/1227/1293/1417/1444`, `cmd_order.go:817/832` |
| `SetCursor(runID, scoped, seq uint64) error` | `order_dispatch.go:1187` (pre-side-effect cursor persist), cursor half of `:1442` |
| `CloseRun(runID, reason) error` | `cmd_order.go:758/814` defer-Close; immediate-close half of `CreateRunClosed` |
| `RecentRuns(scoped, limit) ([]OrderRun, error)` | `cmd_order.go:1397-1402` (`gc order history`) |
| `LastRun(scoped) (time.Time, bool, error)` — cooldown clock read | `internal/orders/runtime_helpers.go` `LastRunFuncForStore` |
| `EventCursor(scoped) (uint64, error)` — `MaxSeqFromLabels` confined inside | `cmd_order.go:1688-1706` (`bdCursor`), `CursorFuncForStore` |
| `OpenTracking(scoped) ([]OrderRun, error)` / `HasOpenWork(scoped) (bool, error)` — single-flight gate | `order_dispatch.go:1471-1477` (`listCanonicalOpenOrderTrackingBeads`), `:1499-1531` (`hasOpenWorkStrict`) |
| `SweepOrphaned(SweepOpts)` / `SweepStale(now, StaleSweepOpts)` | `order_dispatch.go:1880-1915`, `:2059-2123`; `city_runtime.go:1481` |
| free funcs `RecentRunsAcrossStores` / `EventCursorAcrossStores` / `SweepAcrossStores` (loop `[]OrdersStore`) | `cmd_order.go:1386-1426`, `bdCursorAcrossStores` `:1708-1723`, retention-across-stores `:2125+` |

**Replaces ~31 raw ops.**

**Codec boundary.** Crossing in/out: a scoped order name (string), a `RunOutcome`
enum, a `uint64` `EventCursor`, `time.Time` (`CreatedAt` = cooldown marker), and an
`OrderRun{ID, Scoped, Outcome, CreatedAt, Open, Cursor}`. Confined inside: the
`Title`/label encoding, outcome→label mapping, the cursor→(`order:<scoped>`,
`seq:<N>`) label-pair codec, `NoHistory`, `TierBoth`, `IncludeClosed`,
`close_reason` metadata. **Must reuse the canonical label constants already
mirrored in `internal/coordclass` for routing** (do not re-declare them; the
coordclass drift test guards both).

**Caveat — no domain type exists yet.** Unlike session (`Info`), nudge (`Item`),
mail (`Message`), the order object has **no** pre-existing domain type. `OrderRun`
+ `RunOutcome` + `EventCursor` are net-new and must be designed, not promoted.

### 3.5 graph — DAG-FRONT-DOOR (already substantially built)

**Feasibility.** The **write** half is a clean front door today:
`beads.GraphApplyStore.ApplyGraphPlan(ctx, *GraphApplyPlan) (*GraphApplyResult,
error)` (`internal/beads/graph_apply.go:10-12`) is the only graph-class mutation
path — plan in, symbolic-key→id map out, codec confined to `BdStore`/
`NativeDoltStore` impls. **There is no per-attribute graph setter anywhere**, and
the inventory found **ZERO graph leak sites** across reconciler, CLI/API, and
prompts. **replacesRawOps = 0; gcCommands = NONE.**

The remaining DAG-walk **reads** (`storeHasOpenDescendantsByWalk`,
`enumerateWispGCRoots`, `collectAttachedBeads`, with callers reading
`b.Status`/`b.ParentID`/`b.Metadata[gc.root_bead_id]` at
`cmd/gc/order_dispatch.go:1581-1700`, `wisp_gc.go:149-260`,
`attachment_metadata.go:8`) traverse the **WORK/molecule substrate** — the v1
materialization of a formula — which the principle explicitly keeps bead/raw. They
are **NOT leaks.**

**Optional type-hygiene only** (defer unless parity with session/nudge/order is
wanted): promote those walks into typed `GraphStore.Descendants` /
`HasOpenDescendants` / `EnumerateRoots` / `CollectAttached` / `PurgeClosedSubtree`
returning `GraphNode`/`GraphSubtree` instead of `[]beads.Bead`. If pursued, those
methods must call `GraphApplyFor(s.Store)`/`HandlesFor(s.Store)` internally — the
`class_store.go` wrapper does **not** promote optional capabilities (assert on the
embedded `.Store`).

---

## 4. gc commands to introduce

**Verdict: NONE.** All three inventory areas agree, and the prompt audit is
explicit:

- **inv:prompts** searched every prompt/template/formula/skill asset in the repo
  (`internal/bootstrap/packs/core/{skills,formulas,orders,assets/prompts,overlay}`,
  `cmd/gc/prompts/*.md`, `examples/**`) and found **zero** raw `bd` ops on a
  session, nudge, mail, order, or graph object. Every raw `bd` op an agent runs
  targets a **WORK** bead (its assigned task or a molecule/formula step — the
  substrate the principle keeps bead/raw).
- Agents already reach every non-work object through existing commands:
  - mail → `gc mail send|reply|inbox|count|peek|read|thread|archive|mark-read|mark-unread|delete|check` + `gc order sweep-nudge-mail`
  - session → `gc session new|suspend|close|kill|nudge|logs`, `gc agent add|suspend|resume`
  - drain/restart signals → `gc runtime drain|undrain|drain-check|drain-ack|request-restart`
  - nudge → `gc session nudge`
  - order → `gc order run|check|history|list` + `gc order sweep-tracking`
  - wait → `gc session wait`
  - graph/molecule navigation → `gc bd` / `bd mol` (WORK substrate, allowed)

The prompt-side front-door obligation is **already met.** The remaining work is
**100% Go-internal** (go-cli + go-api + go-controller): the existing `gc` command
*handlers* still call raw `store.SetMetadataBatch/Create/Close/List` inside their
own bodies — the leak is **inside the handler impl**, not at the prompt boundary —
so no new command is required, only routing those handlers through the typed
stores above.

**Boundary flag (out of scope for this repo):** pack-specific prompts ship in
external pack repos (gastown/workflows/maintainer). Those must be audited
separately; this audit can only see the built-in core pack.

---

## 5. Reconciler rework — the work-vs-session split

The deepest and densest leak is the controller/reconciler. The critical
structural fact: many ops in session-lifecycle files are **WORK ops keyed by a
session identity**, not session ops. On a single-store (default) city they run
against the *same store object* the session arm uses, so they are literally
"WORK-List-through-the-session-store." They must split:

- **session-attribute ops → the session front door** (§3.1): every
  `SetMetadataBatch(session.ID, patch)` state-heal, every `Get(session.ID)` →
  read `.Status/.Metadata`, the `[]beads.Bead` snapshot.
- **cross-class WORK / readiness / assignment ops → a WORK/assignment API, NOT
  the session store.** Proposed surface:
  `workStore.OpenAssignedTo(identity)`, `ReadyAssignedTo(identity, tier)`,
  `ReleaseAssignedTo(sessionID)`, `ReassignAssignedTo(old, new)`. Work itself
  stays bead/raw *inside* that API (it is the substrate); the point is that the
  session reconciler stops calling `store.List(ListQuery{Assignee})` /
  `beads.ReadyLive` directly.

**Cross-class WORK leak list (must move off the session store):**

| site | op | belongs to |
| --- | --- | --- |
| `session_reconciler.go:2933` `firstOpenAssignedWorkBeadInStoreByIdentifiers` | `List(ListQuery{Assignee,Status,Live,TierBoth})` | `workStore.OpenAssignedTo` |
| `session_reconciler.go:3100` `collectSessionAssignedWork` | `List(ListQuery{Assignee,…})` | `workStore.OpenAssignedTo` |
| `session_reconciler.go:3238` `sessionHasReadyAssignedWorkForTier` | `beads.ReadyLive(ReadyQuery{Assignee,TierMode})` | `workStore.ReadyAssignedTo` |
| `session_reconciler.go:3246` `sessionHasOpenAssignedWorkForTier` | `List(ListQuery{Assignee,Status,Live,TierMode})` | `workStore.OpenAssignedTo` |
| `session_reconciler.go:3221` `sessionHasOpenAssignedWispWork` | `CachedList` + open-assigned wisp probe | `workStore.OpenAssignedTo(TierWisps)` |
| `session_beads.go:2408` `releaseWorkFromClosedSessionBead` | `List(Assignee,Status)` + `Update(Assignee:"",Status:open)` | `workStore.ReleaseAssignedTo(sessionID)` |
| `session_beads.go:696` `unclaimWorkAssignedToRetiredSessionBead` | `List(...)` + `Update(Assignee:"",run_target)` | `workStore.ReleaseAssignedTo` |
| `session_beads.go:762` `reassignWorkAssignedToRetiredSessionBead` | `List(...)` + `Update(Assignee:&new)` | `workStore.ReassignAssignedTo(old,new)` |
| `build_desired_state.go:1116/1135/1177/1186` `collectAssignedWorkBeads` | `listBothTiersForControllerDemand` / `liveReadyForControllerDemandQuery` | **legit WORK arm** — should be the single place WORK reads are expressed; the session-keyed probes above route *through it* |
| `build_desired_state.go:1708/1717/1758` | `beads.HandlesFor(store).Live/All.List` + `Ready` | the WORK front-door plumbing |
| `city_runtime.go:2124` `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete` | re-home orphaned pool WORK assignments | `workStore.Reassign/Release` |
| `city_runtime.go:1481` `warnIfClosedOrderTrackingBacklog` | `Live.List(Label:order-tracking)` | **orders** front door (§3.4) |
| `cmd_wait.go:769/797/828` wait wake-state assembly | `List(Label:gc:wait)` + `loadSessionWaitBeads` | **wait** front door (partially in `internal/session/waits.go`) |
| `cmd_wait.go:915/925/938` `loadWaitDependencyBead` | `cityStore.Get(depID)` / `scopeStore.Get(depID)` | cross-class **WORK** dep read |

**Coupling.** `closeBead` (session) calls `releaseWorkFromClosedSessionBead`
(work). So the close path touches both APIs. The session front-door PR can land
first against a *stubbed* work/assignment API, or they land together — but the
close path is the join point and must be sequenced deliberately (see §7).

**Existing partial front doors to reuse (not reinvent):**
`internal/session/waits.go` (`ReassignWaits`, `CancelWaits`,
`ListSessionWaitBeads`); `internal/extmsg` (`ReassignSessionBindings`,
`ReassignSessionParticipants`, `CloseSessionBindings`);
`internal/session/names.go` (`WithCitySessionIdentifierLocks`, alias reservation).
The retire cascade (`reassignStateAssignedToRetiredSessionBead` `session_beads.go:782`,
`cancelStateAssignedToRetiredSessionBead` `:800`) already calls these — it just
orchestrates them raw alongside work `Update`s.

---

## 6. Honest caveats

1. **graph stays a DAG front door, not per-attribute setters.** Its write seam is
   `ApplyGraphPlan` (done). Its reads traverse the WORK/molecule substrate and
   stay bead/raw. `replacesRawOps=0`; read tightening is optional type-hygiene.
2. **work stays bead/the wire contract.** The generic task substrate, the
   federation/by-id root, and the HTTP/SSE wire (`beads.Bead`) are intentionally
   raw. The work/assignment API in §5 is a typed *façade over* the substrate for
   the **session-keyed** probes; it does not turn work into a closed domain type.
3. **nudge decoder gap.** Only `Item→Bead` exists today; `reference_json` is
   written but never read (`nudge_beads.go:258`). `decodeNudgeItem` is the one
   genuinely net-new piece of nudge logic, and a decoded Item is **partial**
   (queue-only runtime fields absent). Prefer a narrow `NudgeShadow` view over a
   half-populated `Item`.
4. **order has no domain type.** `OrderRun`/`RunOutcome`/`EventCursor` are net-new
   and must be designed. The cooldown-clock (`CreatedAt`), open-bead-==-in-flight,
   and two-tier-union semantics are load-bearing and must be *named* by the API.
5. **wait and order-tracking are distinct non-work objects** also raw-poked from
   `cmd_wait.go`/`order_dispatch.go`. Wait has a partial front door
   (`internal/session/waits.go`); a full `WaitStore` (Register/SetState/Get/List)
   is its own object, out of the five named here but interleaved at the cmd_wait
   call sites with session writes.
6. **`MetadataPatch` empty-string-clears** is a cross-backend contract that must
   be verified on PG, or every heal/clear breaks silently.
7. **External pack prompts** are unaudited (out of this repo).

---

## 7. Phased migration plan

Each phase: ≤5 files where possible, build-green, TDD (write the test asserting
the typed method's serialization first), and **wire/runtime byte-identical** (the
typed method must emit the exact same bead writes the raw op did — assert with a
fake store recording `SetMetadataBatch`/`Create`/`Close` calls). The reconciler
split is **last** because it is the densest and riskiest.

This plan **extends #3773**, which already promoted (a) the mail half
(`mail.Provider` over `mail.Message`) and (b) the session API/response half
(`session.Manager.GetWithPersistedResponse` → `session.Info` →
`sessionResponseWithReason`) and added the routing-only `class_store.go` wrappers.
#3773 built the *type tags*; this plan turns the tags into *front doors*.

**Phase 0 — domain types + wrapper skeletons (no call-site change).**
Introduce/confirm the domain wrappers that hold `beads.XStore` by value:
extend `session.InfoStore` toward a full `session.Store` (reads first); add the
`nudge` wrapper + `decodeNudgeItem` (with golden round-trip tests); design the
`OrderRun`/`RunOutcome`/`EventCursor` types + `orders.Store`. No production caller
switches yet. Build-green, all new code under test.

**Phase 1 — mail residual SESSION reads.** Add `sessionStore.MailboxAddress/
MailboxAddresses/ListLive(->Info)` and route `cmd_mail.go:953/975/1172/1085/1092`
+ `handler_extmsg.go:113` through them. Closes the "mail looks done" trap. Small,
isolated, no reconciler.

**Phase 2 — nudge front door.** Route `nudge_beads.go` (`Save`/`Terminalize`/
`Find`/`FindIncludingTerminal`) and `cmd_nudge.go:1863` (`RollbackEnqueue`)
through the wrapper. Keep methods callable inside `withNudgeQueueState`. Replace
the `cmd_wait.go`/`cmd_nudge.go` `Metadata[...]` cracks with typed fields.

**Phase 3 — order front door.** Route `order_dispatch.go` + `cmd_order.go` raw
ops through `OrdersStore` typed methods, reusing `coordclass` label constants.
Preserve cooldown-clock and open-bead-in-flight invariants under test. Keep
multi-scope federation as loops over `[]OrdersStore`.

**Phase 4 — session writes (heal/lifecycle).** Route the ~20 `SetMetadataBatch`
heal sites + `closeBead`/`CreateSession`/`Reopen`/`Retire` through the session
front door (`ApplyPatch`/`SetState`/`Sleep`/…/`Close`). Reconciler still reads
the `[]beads.Bead` snapshot — only **writes** move here. Largest single PR;
shard across `session_reconciler.go`, `session_beads.go`, `session_reconcile.go`,
`soft_reload.go`, `controller.go`, `session_circuit_breaker.go`.

**Phase 5 — session reads + snapshot.** Route `Get(session.ID)`→`.Metadata`
reads through `GetInfo`/`GetState`/`ListInfo`. This is where the snapshot decision
lands: extend `session.Info` with `pool_slot/configured_named_identity/
common_name` OR introduce a typed `SessionSnapshot`, then convert
`loadSessionBeadSnapshot`.

**Phase 6 — work/assignment API + the cross-class split (LAST, riskiest).**
Introduce `workStore.OpenAssignedTo/ReadyAssignedTo/ReleaseAssignedTo/
ReassignAssignedTo` routing through the existing
`build_desired_state.go` controller-demand plumbing. Move every cross-class WORK
op in §5 off the session store onto it, including the `closeBead →
releaseWorkFromClosedSessionBead` join. This is the densest, highest-risk change;
it lands only after the session front door is stable so the close path has a typed
session side to bind to.

**Phase 7 — (optional) graph read tightening + wait/order-tracking sweep
cleanup.** Only if parity is desired. Promote graph DAG walks to typed reads;
route the order-tracking sweep/backlog reads (`city_runtime.go:1481`) through the
orders front door.

**Phase 7 — DONE (scaffold cleanup pass).** The order front-door scaffold left
four exported symbols with no non-test caller. They were removed rather than
wired, because wiring could not be byte-identical:

- `Store.OpenTracking` / `Store.HasOpenWork` did a per-scoped `Live.List`. The
  dispatch open-work gate (`cmd/gc/order_dispatch.go`
  `orderDispatchTrackingIndex.hasOpenTracking`/`hasOpenWork`) deliberately
  batches *all* scoped orders per store into one query and layers a wisp-aware
  descendant check on top (`hasOpenWorkStrict`); the per-scoped methods would
  reintroduce the N-serial-query pattern #3201/#3191/#3197 eliminated.
- `RecentRunsAcrossStores` could not replace the `gc order history` loop
  (`cmd_order.go`) byte-identically: that loop preserves per-store error
  semantics (`i == 0 && len(results) == 0`), dedups by
  `scoped\x00id\x00createdAt`, and retains per-order `Name`/`Rig` attribution
  the single-`scoped` free func discards.
- `EventCursorAcrossStores` (and the `Store.EventCursor`/`Store.LastRun` methods
  it kept alive) duplicated `CursorAcrossStores`/`LastRunAcrossStores`, which are
  already the production cooldown/cursor read path; the dead variant even queried
  a different label (`order:` vs `order-run:`), so it was not equivalent to
  `bdCursorAcrossStores`.

The wired-and-kept order front door is `CreateRun`/`CreateRunClosed`/
`SetOutcome`/`SetCursor`/`CloseRun`/`RecentRuns`.

**Permanent raw-by-design exceptions (NOT leaks, do not "front-door"):**

1. The dispatch open-work gate's in-memory tracking index
   (`orderDispatchTrackingIndex`) — a performance-critical batched read, not a
   per-attribute op.
2. The cooldown/cursor runtime helpers (`LastRunFuncForStore`,
   `CursorFuncForStore`, and their `*AcrossStores` forms) — the production
   gate-read path.
3. The order-tracking sweep/backlog reads (`order_dispatch.go` sweeps,
   `city_runtime.go:1481` `warnIfClosedOrderTrackingBacklog`) — distinct
   tier/error semantics; left raw deliberately.
4. The session reconciler close-gate re-read at `session_reconciler.go:342` — a
   full status/metadata resync left raw BY DESIGN.

---

## 8. Invariants to preserve

1. **Wire byte-identical.** No HTTP/SSE payload changes. The typed methods are an
   *internal* refactor; `internal/api/openapi.json` and generated TS types must be
   untouched (`TestOpenAPISpecInSync`, `make dashboard-check`).
2. **Projection-invariance.** `InfoFromPersistedBead` reads only bead fields and
   must round-trip identically across bd/sqlite/PG. Any new decoder
   (`decodeNudgeItem`, `OrderRun` decode) must be backend-invariant and
   side-effect-free.
3. **No typed-nil traps.** The `class_store.go` wrappers do **not** promote
   optional capabilities; methods needing `GraphApplyFor`/`HandlesFor`/`Counter`
   must assert on the embedded `.Store`, never assume the wrapper satisfies the
   optional interface. Guard against a non-nil interface wrapping a nil store.
4. **Byte-identical runtime / bead writes.** Each typed method must emit the exact
   same `SetMetadataBatch`/`Create`/`Close`/`Update` calls (same keys, same
   empty-string-clear semantics, same labels, same `NoHistory`/`TierBoth`) as the
   raw op it replaces — asserted by a recording fake store in the phase's tests.
5. **Empty-string-clears contract** (`MetadataPatch.Apply`) preserved on every
   backend.
6. **Single-source label constants.** Order/coordination label strings reuse the
   `internal/coordclass` canonical constants; the coordclass drift test keeps
   guarding both the routing tag and the codec.
7. **Lock coherence (nudge).** `Save`/`Terminalize` stay callable inside
   `withNudgeQueueState` so the bead shadow and the `state.json` authority stay
   coherent under one flock.
8. **ZERO hardcoded roles / no judgment in Go.** The front-door methods move
   *serialization*, never decisions; no `if state == ...` reasoning migrates into
   Go.

---

## Appendix — front-door verdict at a glance

| object | verdict | domain type | replacesRawOps | gc cmd | notes |
| --- | --- | --- | --- | --- | --- |
| session | CLEAN-FRONT-DOOR | `session.Info` (exists) | ~46 | none | both codec halves exist; join behind a typed store |
| nudge | CLEAN-FRONT-DOOR | `nudgequeue.Item` (exists) | ~6 | none | bead is a SHADOW; needs the missing `decodeNudgeItem` |
| mail | DONE | `mail.Message` (exists) | 0 | none | residual ops are SESSION reads, owned by session |
| order | CLEAN-FRONT-DOOR | none (net-new) | ~31 | none | must name cooldown-clock / open==in-flight / two-tier |
| graph | DAG-FRONT-DOOR (done) | `GraphApplyPlan` (exists) | 0 | none | write seam done; reads are WORK substrate, stay raw |
