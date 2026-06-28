# Usage facts & local cost insight (v0)

Status: **proposal, adversarially reviewed** · Relates to
[worker-runtime-transport-v0](worker-runtime-transport-v0.md)

Turn agent execution into **usage facts** so an operator can answer "what did
this run cost me, and which model/runtime is cheapest per task?" — as local
observability, with a generic sink so facts can also be shipped elsewhere.

The data model and seams below were stress-tested against the real tree; the
"obvious" versions broke (notes inline) and were revised.

## Data model

A new package `internal/usage` exposing a usage fact and a narrow write-only sink:

```go
type UsageFact struct {
	RunID     string // groups facts of one execution (see Run identity). A bead id, never frozen on the session.
	SessionID string // the session bead id. Join key to manifold spend (EIA session_id) + recall transcripts. omitempty.
	StepID    string // the acting work bead id when gc.active_work_bead is present; omitempty for ad-hoc/manual/idle sessions.
	Worker    string // session name
	City      string

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

v0 ships a **resolved-value** form of this. `gc hook --claim` resolves the run id
from the just-claimed work bead (the order above) and writes it onto the session
bead as `gc.current_run_id`, updated on **every** claim so a reused pool session
follows its current run instead of staying frozen on its first. A per-operation
reader then reads `gc.current_run_id` straight off the already-loaded session bead
— no extra `GetWithBead`. The tradeoff: a resolved id, unlike a `gc.active_work_bead`
*pointer*, cannot be re-resolved back to the acting work bead, so per-work-bead
`step_id` attribution (`UsageFact.StepID`) is **not** preserved by this form; the
pointer (resolve at `operation_events.go:136` via `GetWithBead`, `manager.go:1356`)
stays the future option if `step_id` segmentation is wanted (open question 1). The
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

A `gc costs` reader (`cmd/gc/cmd_costs.go`) aggregates usage facts by `run_id`
for per-Run cost insight — no external dependency. v0 reads the local sink file
`.gc/usage.jsonl`, so it reflects facts only under the default `local` usage
provider; with `exec:` or `discard` the facts are forwarded out of process or
dropped by request, and `gc costs` shows nothing local. Aggregating
`WorkerOperation` payloads over `events.jsonl` (so cost insight works regardless
of the configured sink) remains a future option; it depends only on the
`WorkerOperation` payload's `CostUSDEstimate` landing (today "always absent"),
**not** on the OTel instruments.

## Honest limits

Estimates are **decision-support and lossy by construction** (64KB eviction,
Anthropic-shape-only extraction today, `RuntimeHandle` gaps, interrupt remnants).
Idempotency fixes double-count, not under-count — so a `gc costs` total is a good
estimate, not an exact accounting. The `Unpriced` flag must be surfaced in any
rollup, else cost sums silently omit unpriced models.

## Decisions

- `run_id` is **layered, resolved per-operation** from the acting work bead
  (graph → poured → nested → self → session-id for manual chat). v0 records the
  resolved id on the session bead as `gc.current_run_id` at claim time (updated on
  every claim, never frozen); a `gc.active_work_bead` pointer that would also keep
  `step_id` stays a future option (run-identity section; open question 1).
- Usage attaches to the **event-log** path (`WorkerOperation` payload) + the
  `Sink`, **not** OTel metric labels (which are cardinality-bounded by design).
- Sink injection is the proven **`exec:<script>`** seam; the `usage.Sink`
  interface is the only added surface. It lives under **`internal/usage`** —
  private to the `gc` binary like every other package until the API stabilizes
  (AGENTS.md "internal/ packages for now"); the stdlib-only constraint is for
  layering (no upward deps), not an out-of-module public API. The `exec:` sink
  bounds each invocation with a per-fact timeout so a hung script cannot stall
  the controller reconcile tick or a worker op-finish.
- Compute is anchored on an **immutable `awake_epoch`** and emitted from one
  shared helper across **all** terminal paths (incl. idle-sleep + subprocess) —
  never `last_woke_at`.
- Durability is a **transactional outbox** in the same `beads.Tx` as the trigger
  clear; the sink inherits real idempotency from the bead store.

## v0 implementation status

The shipped v0 makes these deliberate, documented deviations from the proposal
above. Each keeps cost insight as decision-support (lossy by construction, per
*Honest limits*) while avoiding larger changes the open questions below have not
yet settled.

- **Run identity is recorded per-run at claim; the reader is the remaining gap.**
  `gc hook --claim` writes the resolved run id onto the session bead as
  `gc.current_run_id` on every claim (`cmd/gc/cmd_hook_claim.go`), so a reused pool
  session is stamped with its current run, not its first, and the key is declared
  in `KnownMetadataKeys`. This value form intentionally drops per-work-bead
  (`step_id`) attribution; a `gc.active_work_bead` pointer remains the future option
  for it. The consumer side is staged behind the writer: `ResolveRunID` still
  resolves per-operation off the session bead's own run chain (`workflow_id ||
  molecule_id || gc.root_bead_id-or-self || bead id || session id`) and does **not**
  yet read `gc.current_run_id`, so cost facts still roll up per-session until a
  reader that consumes the recorded value lands (tracked as follow-up ga-2m8abf;
  see open question 1).
- **Compute facts emit from a reconcile scan, not a transactional outbox.** The
  reconcile tick scans the open session-bead snapshot it already loaded, emits a
  fact for any bead in a terminal state lacking its
  `usage_compute_emitted_at:<epoch>` marker, then writes the marker. This is
  at-least-once: a crash between the durable sink append and the marker write
  re-emits next tick, and `IdempotencyKey` collapses the duplicate at read time
  (fix double-count, not under-count). The scan only sees the open set, so a
  session closed directly from active without first reaching an open terminal
  state (asleep/drained/archived/suspended/quarantined) is a known under-count.
  The single-key marker sidesteps open question 3 (`beads.Tx` validation across
  store impls).
- **The awake epoch is `awake_started_at` at nanosecond precision**, stamped
  fresh on every confirmed start/wake — both create-time `ConfirmStartedPatch`
  and the controller's `CommitStartedPatch` wake path — rather than a separate
  UUID. Refreshing it on every interval is what lets later intervals on a reused
  session bill; nanosecond precision keeps two intervals that begin within the
  same wall-clock second distinct.
- **Model facts emit from the transcript-tail watcher, as proposed.** The single
  per-invocation tail extraction that drives the `gc.agent.*` token instruments
  (`recordInvocationTelemetry`) also builds one model `usage.Fact` per pending
  invocation (`modelUsageFact`) and writes it to the usage sink, keyed on the
  invocation's provider message id (or transcript entry uuid when none) and
  deduped by `IdempotencyKey`. It rides the same durable invocation-usage cursor
  as the metrics, so a reused session bills each invocation once and the final
  turn is picked up by the next prompt op (or a cursor-lagging sweep) rather than
  by a finish-defer read of the *prior* turn — the failure mode the emission-seam
  section warns against. Emission is gated on the usage sink, **not** on
  operation-event recording, so a worker handle with a sink but no event recorder
  (the CLI factory path) still emits. Pricing comes from the handle's registry:
  `Unpriced` is set (and cost left zero) exactly when the (family, model) pair has
  no entry, never collapsing an unevaluated model to a priced `$0`. Stamping the
  `WorkerOperation` event payload's own token/cost fields from the same extraction
  (so `gc costs` could read the event log) remains a separate follow-up; only the
  payload's `RunID` is wired today, resolved through the shared
  `beadmeta.ResolveRunID` that the compute-fact emitter also uses so a run's model
  and compute facts group together.
- **`gc costs` reads the local sink, not the event log** (see the `gc costs`
  section): it reflects facts only under the default `local` provider. The read
  is resilient to a partially corrupt log: a malformed line (a torn mid-append,
  or a record a torn predecessor merged into) is skipped with a line-numbered
  warning rather than failing the whole read, so one torn record never blanks
  `gc costs`. The local sink also terminates a torn tail before its next append,
  so a fact whose `Record` returned nil is not lost to a crashed predecessor.
- **Usage is on by default (`local` sink)** so compute facts accrue without
  operator opt-in. Reconcile emission reuses the tick's existing open-session
  snapshot instead of issuing its own store scan, so the steady-state
  control-plane cost is iterating an in-memory slice, not an extra query per
  tick. Sink and marker write failures are logged (never silently dropped).

## Why this lands as one self-contained change

Usage facts v0 is one vertically-integrated feature: the `internal/usage` data
model and sinks, run-id resolution on the worker payload, the compute-fact
reconcile seam, the model-fact transcript-watcher seam, the `[usage]` config + sink
threading, and the `gc costs` reader. Splitting it leaves non-functional
remainders — a sink no emitter writes to, a config key nothing reads, a
`gc costs` that always prints nothing — none independently reviewable or
shippable. Most of the diff is regenerated artifacts (OpenAPI / genclient / TS
for the two new `WorkerOperation` payload fields) and tests; the hand-written
behavior change is small and gated behind a config key that defaults to the safe
`local` sink. It is reviewed as one unit because that is the smallest unit that
does anything end to end.

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
