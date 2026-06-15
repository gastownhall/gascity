# Usage facts & local cost insight (v0)

Status: **proposal, adversarially reviewed** · Relates to
[worker-runtime-transport-v0](worker-runtime-transport-v0.md)

Turn agent execution into **usage facts** so an operator can answer "what did
this run cost me, and which model/runtime is cheapest per task?" — as local
observability, with a generic sink so facts can also be shipped elsewhere.

The data model and seams below were stress-tested against the real tree; the
"obvious" versions broke (notes inline) and were revised.

## Data model

A new package `usage` exposing a usage fact and a narrow write-only sink:

```go
type UsageFact struct {
	RunID  string // groups facts of one execution (see Run identity). A bead id, never frozen on the session.
	StepID string // the acting work bead id. omitempty.
	Worker string // session name
	City   string

	Kind string // "model" | "compute"

	// model facts (from sessionlog tail extraction):
	Upstream, Model, Backing                                        string
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int

	// compute facts (from the reconcile/teardown seam):
	Runtime     string
	WallSeconds float64

	CostUSDEstimate float64 // pricing.Registry.Estimate (pricing.go:214). list-price; decision-support.
	Unpriced        bool    // true when (provider,model) had no price: tokens still emitted, cost FLAGGED not dropped.

	Provider       string // "anthropic"|"codex"|… (extractor shape differs per family)
	UpstreamReqID  string // provider response id: Anthropic message.id / OpenAI response.id (model);
	                       // sessionID+awakeEpoch (compute); CONTENT-HASH for codex (NOT positional codex-event-<idx>).
	At             int64  // unix millis, stamped by emitter
	IdempotencyKey string // natural key per kind (below) — gives the sink real dedup
}

// Sink is the write-only extension point, mirroring the events.Recorder split.
type Sink interface { Record(ctx context.Context, f UsageFact) error }
```

Cost is a **list-price estimate** (decision-support), absent-not-zero on a
pricing miss (`Unpriced=true`). Idempotency keys are natural: model =
`hash(RunID + ":" + UpstreamReqID)`; compute =
`hash(RunID + ":" + sessionID + ":" + awakeEpoch)`.

## Run identity — per-operation, from the acting work bead

A **Run** = one execution of a formula / order / chat. The tempting "run_id =
wisp root bead id" is **wrong**: pool/canonical session beads are **reused
across many runs** (`build_desired_state.go:2542/2625`), so a run_id frozen on
the session bead misattributes every later run to the first. Resolve
**per-operation from the acting work bead**, in order:

1. graph workflow → `workflow_id` (`sling_core.go:476`)
2. poured / wisp work bead → `molecule_id` (`sling_core.go:255/319`)
3. nested / sub-formula → `gc.root_bead_id`-or-self (`molecule.go:231-234`)
4. plain work bead → its own id
5. manual chat (no work bead) → `session.Info.ID`

The worker learns the acting work bead via a **mutable** `gc.active_work_bead`
pointer written on the session bead at dispatch (updated on every claim) plus
`operationEventPayload.BeadID`; resolve at `operation_events.go:136` via the
existing `GetWithBead` (`manager.go:1356`) or the dispatch-set `BeadID`. The
session bead is a *current-pointer*, never a frozen id.

## Emission seams

### Model fact — a transcript watcher, not the op-finish defer

Do **not** emit in the `Message`/`Nudge` finish defer: `manager.Submit` returns
*before* the turn completes (`submit.go:78-122`), so the defer reads the *prior*
turn — on a reused session that's a *different run*, and the final turn before
`Stop` is never read. Instead drive emission from a **transcript-tail watcher**
keyed on per-assistant-message id + a **durable cursor** (bead metadata), firing
on each new assistant message with usage. On startup, reconcile from the durable
cursor, not the 64KB tail.

Reuse the single tail extraction (the `gc.agent.*` token instruments) to also
stamp the `WorkerOperation` event payload (`event_payloads.go:300`): wire the
declared-TODO `BeadID` (`operation_events.go:64`), add `RunID`/`Unpriced` to both
mirror structs (kept in sync by `TestEveryKnownEventTypeHasRegisteredPayload`),
and set `CostUSDEstimate` (today "always absent"). Don't add a second extractor.

Caveats to handle, not hide: 64KB tail eviction (`tail.go:34`); Anthropic-shape
only (`tail.go:258`) — make extraction provider-agnostic via a per-reader
`UsageExtractor`, or emit an `unsupported_provider` fact so other families are
never *silently* dropped; `RuntimeHandle` turns (`runtime_handle.go:206/238`)
have no bead/transcript and must get a minimal identity or emit a
`no-attribution` fact.

### Compute fact — one shared helper, immutable awake epoch

Stamp an **immutable** `awake_started_at` + fresh `awake_epoch` UUID at
`ConfirmStartedPatch` (`lifecycle_transition.go:191`). Compute
`wall_seconds = transition_now - awake_started_at` and emit from **one shared
helper** called on **every** terminal path — crash/orphan
(`session_reconcile.go:523`/`healState:938`), graceful stop (`controller.go:1071`,
resolved by bead id not name), graceful **idle-sleep** (`session_sleep.go:319`
`SleepPatch`), and the **subprocess/one-shot** exit. Do **not** anchor on
`last_woke_at` — it is a wake-*attempt* lease cleared in 7+ non-teardown paths
(double-count/loss), and `now - creation_complete_at` over-counts (re-stamped per
wake, spans all intervals). Snapshot the runtime kind into bead metadata at Start
(`auto.DetectTransport` needs liveness, unreadable post-mortem).

## The Sink — generic extension point, durable outbox

`newUsageSinkByName` in `cmd/gc/providers.go` mirrors the proven `exec:` idiom
(events `providers.go:121`, beads `:482/:626`, mail `:682`). A `[usage] provider`
config key (default `"local"`, last-wins in `compose.go`, paralleling
`EventsConfig`) selects: `local` → an OSS local sink; `exec:<script>` → an
out-of-process sink (JSON `UsageFact` over stdin) for anyone who wants to forward
facts to their own aggregator. Constructed once in `newControllerState`
(`api_state.go:86`), threaded via `worker.FactoryConfig` to the seams.

**Durability — a transactional outbox, not an in-memory buffer.** An in-memory
channel is not an outbox: the compute trigger clear (`clearLastWokeAt`, a
synchronous-durable write, `session_reconcile.go:615`) happens before any async
flush, so a crash in between loses the fact permanently and idempotency can't
dedupe a fact never delivered. Instead, inside the **same `beads.Tx`**
(`beads.go:251` — *not* the non-atomic `SetMetadataBatch`) that clears the
interval, write a durable marker `usage_compute_emitted_at:<awake_epoch>` (copy
the `strandedEventEmittedKey` idiom, `session_reconciler.go:2417`) and append the
fact to durable local storage; the reconcile tick re-emits any epoch lacking its
marker. For model facts, a durable per-`(session, UpstreamReqID)` cursor lets a
sweep re-read any session whose cursor lags its transcript. `Record` is
non-blocking; errors are swallowed-but-**logged** (never a silent drop).

## `gc costs`

A `gc costs` reader (`cmd/gc/costs.go`, NEW) aggregates facts / `WorkerOperation`
payloads by `run_id` over `events.jsonl` for per-Run cost insight — no external
dependency. It depends only on the `WorkerOperation` payload landing (the
`CostUSDEstimate` field, today "always absent"), **not** on the OTel instruments.

## Honest limits

Estimates are **decision-support and lossy by construction** (64KB eviction,
Anthropic-shape-only extraction today, `RuntimeHandle` gaps, interrupt remnants).
Idempotency fixes double-count, not under-count — so a `gc costs` total is a good
estimate, not an exact accounting. The `Unpriced` flag must be surfaced in any
rollup, else cost sums silently omit unpriced models.

## Decisions

- `run_id` is **layered, resolved per-operation** from the acting work bead
  (graph → poured → nested → self → session-id for manual chat), via a **mutable
  `gc.active_work_bead` pointer** — never frozen on the session bead.
- Usage attaches to the **event-log** path (`WorkerOperation` payload) + the
  `Sink`, **not** OTel metric labels (which are cardinality-bounded by design).
- Sink injection is the proven **`exec:<script>`** seam; the public `usage.Sink`
  interface is the only added surface.
- Compute is anchored on an **immutable `awake_epoch`** and emitted from one
  shared helper across **all** terminal paths (incl. idle-sleep + subprocess) —
  never `last_woke_at`.
- Durability is a **transactional outbox** in the same `beads.Tx` as the trigger
  clear; the sink inherits real idempotency from the bead store.

## Open questions

1. **Pooled-session compute attribution.** One awake interval on a reused session
   can span work from multiple runs. Either segment compute by active-work-bead
   transitions (one fact per `(run, sub-interval)`) or roll it up at the
   pool/worker level and leave it run-unattributable. Open.
2. **Metadata-store placement** for the durable cursors/markers — bead metadata
   vs a dedicated store.
3. **`beads.Tx`** is currently unused in `cmd/gc`/`internal/session` non-test
   code; adopting it must be validated against every store impl (Mem/File atomic;
   Bd/exec sequential per `beads.go:240/251`).
4. **Crashed-interval `wall_seconds`** needs a periodic `heartbeat_last_seen_at`
   to bound the end; its cadence is new write-amplification to size.
