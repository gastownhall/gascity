---
title: "Session Reconciler Tracing"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-04 |
| Author(s) | Codex |
| Issue | `test-ejn` |
| Supersedes | N/A |

## Summary

Gas City needs denser, more forensic controller tracing for failures in
the session reconciler path. Today, operators can usually tell that
something went wrong, but not why a specific reconcile tick chose one
branch, skipped another, or produced surprising provider behavior.
Template-driven bugs are especially painful because the relevant state
often spans config reload, desired-state construction, cap application,
session reconciliation, start execution, and drain progression.

This proposal adds a local, append-only structured tracing framework for
the session reconciler path. The framework is optimized for machine
consumption first, keeps the controller as the sole trace writer, and
records both always-on compact summaries and short-lived high-detail
traces for selected templates. The canonical stream is intentionally
minimal and safe enough to become Phase 2’s evidence source of truth;
unsafe raw payload capture is not part of the canonical v1 design.

Phase ordering is explicit:

1. **Phase 1:** add deep structured tracing for the session reconciler
   path and its immediate upstream demand/config inputs.
2. **Phase 2:** build incident bundles and richer bug-report assembly on
   top of those trace records.

This document covers Phase 1. It deliberately does not try to redesign
all logging in Gas City.

## Motivation

### Pain today

1. The controller has many important negative decisions and early exits,
   but most are invisible unless an operator infers them from absence.
2. Template-level bugs frequently start before `reconcileSessionBeads`
   runs, for example during desired-state construction, `scale_check`,
   pool demand calculation, or config reload.
3. When provider calls fail, the current logs usually show the outcome
   but not the branch chain and input state that led there.
4. Existing observability streams are useful but fragmented:
   event bus records, stderr notes, provider/session logs, and telemetry
   all answer different slices of the question.
5. Operators need a way to say “start logging `repo/polecat` now” and
   get a dense, machine-readable trail for the next few minutes without
   turning on globally noisy debug logging.

### Goals

- Make one reconcile cycle the primary forensic boundary.
- Explain both **what happened** and **why the controller chose it**.
- Keep always-on summaries cheap enough to run continuously.
- Support short-lived, template-scoped detail tracing that can be
  started manually or auto-triggered by high-signal anomalies.
- Include immediate upstream template-demand and config-reload inputs,
  not just session-local decisions.
- Preserve enough raw state that AI consumers can reason directly over
  the traces without heavy preprocessing.
- Keep tracing best-effort and non-interfering with controller behavior.

### Non-goals

- Tracing convergence in this first pass.
- Replacing the existing event bus, telemetry, or provider session logs.
- Capturing unsafe raw payloads in the canonical stream. If future work
  needs unsafe local sidecars, that is separate from this design.
- Building the incident/evidence bundle product in this phase.
- Instrumenting provider internals such as tmux substeps in v1.

## Design Principles

1. **Cycle-first, not function-first.**
   The core question is why one reconcile cycle decided what it did.
2. **Negative decisions are first-class evidence.**
   The absence of an action is often the bug.
3. **Single canonical stream.**
   One append-only local JSONL stream is easier to trust than a
   canonical log plus side indexes.
4. **Controller remains the only trace writer.**
   CLI and offline controls mutate arm state, but the controller emits
   the authoritative trace records.
5. **Best-effort always.**
   Tracing must never affect reconcile decisions or tick completion.
6. **Machine-readable over human-pretty.**
   V1 optimizes for structured JSON consumed by AI and later tooling.
7. **Short-lived detail, continuous baseline.**
   Summaries stay on; dense detail is bounded by template and time.
8. **Claims must match contracts.**
   The design may only promise completeness, durability, or safety where
   it defines an explicit contract and corresponding tests.

## Hard Contracts

### Non-interference contract

Tracing must remain observational only. V1 therefore defines explicit
budgets and degradation behavior:

- baseline hard budget: 128 KiB serialized per cycle
- detailed-template hard budget: 512 KiB and 400 records per template
  per cycle
- promotion-flush hard budget: 128 KiB per promoted template
- max metadata flush wait: 10 ms
- max durable flush wait: 25 ms
- max concurrently auto-armed templates: 4
- max dependency expansion fan-out: 4 direct dependencies

When a budget is exceeded, the tracer degrades in this order:

1. drop optional bulky payload captures first
2. stop emitting additional low-priority detail records for that entity
3. emit explicit overflow/loss markers
4. preserve required records: `cycle_start`, evaluated-template
   baseline, `trace_control`, `cycle_result`

Slow storage is treated as a tracing fault, not as a reason to stall the
controller. Once a flush wait budget is exceeded, the controller marks
the cycle `slow_storage_degraded`, drops further optional tracing for
that cycle, and continues reconcile work without waiting indefinitely on
disk I/O.

Tracing may never mutate reconcile iteration order, work-set
construction, or scheduling. Scope expansion, promotion, and drop logic
operate on a read-only shadow of state gathered by the controller.

### Causality contract

The trace may only claim “the stream alone explains this decision” when
the relevant records carry explicit causal references. Parent/child
hierarchy is not enough.

### Durability contract

The trace stream is append-only but not “best guess” durable. V1 defines
framing, recovery, integrity, sync points, low-space behavior, and a
bounded crash-loss window.

## Scope

### In scope

- One reconcile cycle as the top-level trace boundary.
- The `session_reconciler` path.
- The immediate upstream template-demand path that feeds it:
  - desired-state construction
  - `scale_check` execution and parsing
  - pool demand calculation
  - cap acceptance and rejection
- Same-tick sub-operations launched by the reconciler:
  - planned starts
  - drain advancement
  - provider-facing start/interrupt/peek-style operations
- Config reload records and template config snapshots.
- Manual and auto-triggered template detail tracing.
- One-hop dependency expansion for traced templates.

### Out of scope

- Convergence tracing.
- Provider-internal step tracing such as tmux `send-keys`.
- Remote export as the source of truth.
- Separate derived indexes or incident manifests in v1.

## Proposed Design

### 1) Two trace levels: always-on baseline plus bounded detail

The tracer emits two levels of records:

1. **Baseline**
   - Always on.
   - One `cycle_start` and one `cycle_result` per tick.
   - One cycle-wide shared-input snapshot per tick.
   - One compact evaluated-template summary for every template the
     controller evaluated that tick.
   - Activity-gated per-session summaries only for sessions that changed
     state or participated in meaningful work.
2. **Detail**
   - Enabled by manual arm or auto-trigger.
   - Captures per-template config snapshot, branch decisions, external
     operations, mutations, per-session baselines, and same-cycle
     upstream demand reasoning.
   - Applies to the exact armed template plus one-hop dependencies.

The baseline layer preserves continuity across time. The detail layer
captures the dense branch-level reasoning that is missing today.

### 2) One reconcile cycle is the primary trace boundary

Each controller tick gets a new reconcile trace identity and lifecycle:

- `cycle_start`
- zero or more shared snapshots, template records, session records,
  operation records, and mutation records
- `cycle_result`

The cycle is the unit of causality, durability, and completeness.

Each cycle records:

- `trace_id`
- `tick_id`
- `seq_start` / `seq_end`
- trigger reason for why the tick ran
- controller-instance provenance
- code provenance
- config provenance
- completion status and loss accounting

The cycle end record also serves as a compact machine-oriented rollup so
consumers can answer “what happened this tick?” without scanning every
child record first.

### 2a) Trace boundaries map to concrete controller hooks

The named trace boundaries are tied to concrete runtime hooks rather
than idealized semantic phases:

1. controller tick entry
2. optional `reloadConfig`
3. `buildDesiredState`
4. pool demand / cap calculation
5. `beadReconcileTick` and `reconcileSessionBeads`
6. `executePlannedStarts`
7. `advanceSessionDrainsWithSessions`
8. tick finalization

Records may still interleave logically. Flush groups are ordering and
durability boundaries, not claims that the runtime executes in perfectly
isolated semantic phases.

### 2b) Deterministic ordering and promotion semantics

Record order is deterministic and assigned at flush time, not creation
time.

- `seq` is allocated only when a batch is committed to the stream.
- Each batch is formed from a stable sort of its buffered records.
- Promotion never splices records ahead of an already committed batch.
- If a template is promoted mid-cycle, the tracer first emits a
  synthetic template/session baseline for that template as needed, then
  flushes the buffered detail-capable records for that template, then
  resumes normal batching.

Promotion can therefore expose more detail for the current cycle, but it
cannot reorder already committed records or imply detail context that
was never captured.

### 3) Add explicit instrumentation across the full template-decision path

For traced templates, v1 instruments the following flow as one coherent
timeline:

1. Config reload outcome and compact diff summary when reload occurs.
2. Desired-state eligibility and template demand inputs.
3. `scale_check` execution, parse, clamp, and failure outcomes.
4. Pool demand construction and cap acceptance/rejection.
5. Reconcile decisions and major early exits.
6. Planned start execution, provider-facing start outcome, and rollback.
7. Drain begin, drain advancement, timeout, cancel, and completion.
8. Significant metadata/runtime mutations.

This is the smallest slice that still explains the failures operators
actually care about.

### 4) Trace negative decisions and explicit early exits

A decision record is emitted for each major branch or guard in the
traced scope, including negative outcomes and early exits.

Each decision record carries:

- `record_type: decision`
- `site_code`
- `reason_code`
- `outcome_code`
- `input_record_ids`
- `config_snapshot_record_id` when relevant
- the inputs inspected by that branch
- a concise human detail field when useful
- optional side-effect references

The goal is to answer “why didn’t it do the obvious thing?” without
reverse-engineering the code from missing logs.

### 5) External operations are first-class records

Important shell and provider interactions are recorded as structured
operations, not collapsed into branch outcomes.

Examples:

- `scale_check_exec`
- `template_prepare`
- `provider_start`
- `provider_interrupt`
- `provider_peek`
- `drain_begin`
- `drain_advance`

Each operation record includes:

- `operation_id`
- `decision_record_id`
- inputs
- outputs/results
- duration
- error
- related `site_code`
- `reason_code` / `outcome_code`

V1 stops at the provider API boundary. It does not trace tmux internals.

### 6) Mutations are first-class records

Significant bead metadata and runtime state writes are emitted as
`mutation` records with immutable write-boundary snapshots only.

Each mutation record includes:

- `decision_record_id`
- `operation_id` when produced by an operation
- `target_kind`
- `target_id`
- `write_method`
- changed fields
- `before`
- `after`
- `snapshot_status`
- `write_result`
- `error`

Batched writes are represented as one mutation record with grouped field
diffs. If the tracer cannot capture a trustworthy before/after snapshot
at the write boundary, it must emit `snapshot_status: unavailable`
instead of fabricating best-effort state.

### 7) Use baseline snapshots plus deltas, not full repeated dumps

Within detailed tracing, each traced session or template gets a baseline
snapshot for the cycle, followed by narrower per-decision and
per-operation delta records.

This keeps records self-contained enough for AI while avoiding the much
larger cost of repeating the entire state on every branch emission.

For any detail-traced template or session, the relevant baseline and
config snapshot must appear no later than the first `decision` or
`operation` record that references them in that cycle.

### 8) Minimize sensitive capture and use type-stable truncation

The canonical stream stores raw values only for allowlisted,
non-sensitive fields. Sensitive classes are redacted, omitted, or
fingerprinted in v1:

- env values, credentials, cookies, tokens, keys, auth headers,
  prompts, private messages, provider stdout/stderr, terminal captures,
  and sensitive mutation/config values are not stored raw
- their presence, key names, lengths, hashes, and selected safe
  fingerprints may be stored when diagnostically useful

Fingerprints for sensitive values use keyed HMAC-SHA256 with a
per-city secret, not unsalted raw hashes.

Large text-like fields that are allowed into the canonical stream use a
type-stable wrapper even when not truncated:

Large textual fields use a wrapper of the form:

```json
{
  "value": "...possibly truncated...",
  "original_bytes": 48192,
  "stored_bytes": 16384,
  "truncated": true
}
```

Initial caps:

- safe text payloads: 16 KiB
- safe config/message text: 32 KiB
- audit/detail note fields: 8 KiB

Structured fields do not switch between raw objects and wrapped blobs.
If a structured field is too large or too sensitive, the canonical
stream stores a filtered structured projection plus explicit omission
metadata.

## Record Model

### Common fields on every record

Every record includes:

- `trace_schema_version`
- `seq`
- `trace_id`
- `tick_id`
- `record_id`
- `parent_record_id`
- `caused_by_record_ids`
- `record_type`
- `trace_mode`
- `trace_source`
- `site_code`
- `ts`
- `cycle_offset_ms`
- `city_path`
- `config_revision`
- `template`
- `session_bead_id`
- `session_name`
- `alias`
- `provider`
- `work_dir`
- `session_key` when known
- `operation_id` when relevant

Mode and source are explicit:

- `trace_mode`: `baseline` or `detail`
- `trace_source`: `always_on`, `manual`, `auto`, or
  `derived_dependency`

### Cycle and controller provenance

`cycle_start` and `cycle_result` also include:

- `tick_trigger`
- `trigger_detail`
- `gc_version`
- `gc_commit`
- `build_date`
- `vcs_dirty`
- `code_fingerprint`
- `controller_instance_id`
- `controller_pid`
- `controller_started_at`
- `host`

### Stable vocabularies

The trace stream defines typed constant vocabularies for:

- `record_type`
- `reason_code`
- `outcome_code`
- `site_code`

Freeform text is supplementary only. Line numbers are not part of the
schema contract.

### Required record types

V1 supports at least these record types:

- `cycle_start`
- `cycle_input_snapshot`
- `batch_commit`
- `config_reload`
- `template_tick_summary`
- `template_config_snapshot`
- `session_baseline`
- `session_result`
- `decision`
- `operation`
- `mutation`
- `trace_control`
- `cycle_result`

### Record-type contracts

Phase 1 includes a normative schema package for every `record_type`.
That package defines:

- required fields
- optional fields
- nullability
- enum domains
- additional-property policy
- causal-link requirements
- completeness/loss markers where relevant

Unknown fields must be ignored by readers within the same schema
version. Unknown schema versions must be rejected as unsupported.
Fingerprints, hashes, key-name captures, and other derived-secret hints
are marked as sensitive-derived metadata in that schema package.

Compatibility policy:

- additive fields and additive enum values are allowed within one schema
  version
- field removal, rename, semantic redefinition, or enum removal requires
  a schema-version bump
- vocabulary registries are checked in tests and implementation may not
  emit undeclared stable codes

### Cycle-wide shared snapshot

`cycle_input_snapshot` captures shared inputs that affect many sessions
at once, such as:

- desired-state summary and exact template evaluation set
- `scale_check` counts and raw per-template result summaries
- pool demand summary and cap rejection set
- work-set summary
- ready-wait summary
- whether the underlying store view was partial
- desired template/session counts
- dependency-state summary

### Time fields

All records carry:

- `ts` as an absolute wall-clock timestamp
- `cycle_offset_ms` as relative position within the cycle

Operations and cycle completion also carry `duration_ms`.

## Manual and Auto Detail Tracing

### Manual arming

The operator interface is top-level and controller-oriented:

```bash
gc trace start --template repo/polecat --for 15m
gc trace stop --template repo/polecat
gc trace stop --template repo/polecat --all
gc trace status
gc trace show --template repo/polecat --since 15m
gc trace cycle --tick 1234
gc trace reasons --template repo/polecat --since 15m
gc trace tail --template repo/polecat
```

Manual tracing is keyed by exact normalized template selector, not by
session alias or glob. Re-running `start` for the same template is an
idempotent extend/update, not an error.

`gc trace stop --template repo/polecat` clears manual arms only.
`gc trace stop --template repo/polecat --all` clears both manual and
auto-triggered arms for that template.

### Auto arming

Any template can auto-arm itself on a small, high-signal anomaly list.
Initial triggers include:

- `pending_create_rollback`
- `wake_failure_incremented`
- `quarantine_entered`
- `store_partial_drain_suppressed`
- `config_drift_drain_started`
- `drain_timeout`
- `unknown_state_skipped`
- upstream failures such as `scale_check_exec_failed` or template
  resolution failure

Auto arms are short-lived and extend on repeated triggers.

Auto-arm guardrails:

- per-template trigger cooldown: 5 minutes
- same-trigger dedupe window: 2 ticks
- max concurrent auto-armed templates: 4
- explicit exclusions for known expected transients
- overflow emits `auto_arm_suppressed` or `auto_arm_rate_limited`
  control records

### Dependency expansion

If a template is armed, v1 detail tracing automatically includes its
direct dependencies for that cycle. This expansion is derived-only and
does not create separate persisted arm entries.

Dependency expansion is capped at 4 direct dependencies per source
template per cycle. If the cap is hit, the tracer emits an explicit
`dependency_expansion_truncated` record.

### Expiry semantics

- Manual arm default: 15 minutes
- Auto arm default: 10 minutes
- Repeated triggers extend the active expiry

If a template disappears after config reload, the exact original
selector remains armed until expiry and emits explicit
`template_missing` style records.

## Control Plane

### Persisted arm state

Arm state lives under `.gc/runtime/session-reconciler-trace/` and
survives controller restarts.

Suggested files:

- `arms.json`
- daily segment directories under `YYYY/MM/DD/`

Arm entries are stored separately by source so that manual stop does not
implicitly clear auto-triggered tracing.

Each arm stores:

- scope type and value
- source
- level
- `armed_at`
- `expires_at`
- `last_extended_at`
- trigger or actor metadata
- offline `requested_at` metadata when CLI acts while the controller is
  not running

`gc trace status` should show both the active source arms and the
currently expanded derived scopes they imply for the next cycle.

### Controller remains the only trace writer

The CLI prefers the controller socket when available. If the controller
is offline, the CLI reads or atomically rewrites the persisted arm state
directly. The controller then observes that state and emits the
canonical `trace_control` records when it next starts or ticks.

This keeps the trace stream single-writer while still allowing
offline-capable `gc trace start/stop/status`.

### Trace control records

Manual and automatic changes to trace state are themselves recorded as
`trace_control` records, including:

- `action`: `start`, `extend`, `stop`, `expire`
- `source`
- scope
- expiry
- trigger reason
- actor metadata for manual actions when available

Manual actions record best-effort invocation context such as:

- `actor_kind`
- `actor_user`
- `actor_host`
- `actor_pid`
- sanitized command summary

Raw shell argv is not stored in `trace_control`. `sanitized command
summary` is a constrained projection, not freeform text. Suggested
fields:

- command family, for example `trace.start` or `trace.stop`
- requested template selector
- requested duration
- boolean flags present, for example `all=true`

## Storage and Write Model

### Daily segmented JSONL is the source of truth

Trace data is written locally under `.gc/runtime/session-reconciler-trace/`
as append-only JSONL segments, partitioned by day. Each day may contain
multiple append-only segments to reduce corruption blast radius and make
pruning deterministic.

This is the canonical source of truth for v1. Remote export may be added
later, but it does not replace local forensic storage.

Segment rotation is mandatory:

- maximum 16 MiB per segment
- maximum 512 committed batches per segment
- rotate immediately after any corruption quarantine event

This bounds the blast radius of interior corruption and makes pruning
behavior more predictable.

### Controller-owned single-writer with bounded handoff

Trace batch formation is synchronous in the tick thread, but disk I/O is
performed by a controller-owned single-writer flush loop so storage
latency cannot block reconcile indefinitely.

Each cycle writes in coarse batches at natural boundaries such as:

1. `cycle_start`
2. reload/shared-input phase
3. forward reconcile phase
4. start/drain execution phase
5. `cycle_result`

This preserves crash forensics without paying for one write per branch.

Each batch is serialized fully in memory, written as newline-delimited
JSON records, and terminated by a `batch_commit` record containing:

- `first_seq`
- `last_seq`
- `record_count`
- `crc32`
- `durability_tier`

`batch_commit.crc32` covers the serialized record bytes in that batch
before the `batch_commit` record itself.

### Mid-cycle promotion flush

If a template becomes detailed mid-cycle due to auto-arm or a manual
promotion observed during the cycle, the tracer immediately flushes the
already-buffered records for that template and emits the corresponding
`trace_control` record.

This ensures the most valuable pre-anomaly context is not lost if the
process dies before the next normal phase flush.

Promotion uses the same bounded handoff and wait budgets as any other
flush. It may degrade to partial context, but it may not block the
controller beyond the durable-flush budget.

### Durability, torn-tail, and corruption contract

V1 uses these durability tiers:

- `metadata`: plain append, no forced sync
- `durable`: append followed by `fdatasync`

The following batches are `durable`:

- `cycle_start`
- promotion flushes
- any batch containing detailed anomaly records
- `cycle_result`

The tick thread hands each batch to the flush loop in order and waits
only up to the configured flush budget:

- metadata batches: 10 ms
- durable batches: 25 ms

If the wait budget is exceeded:

1. the controller records `slow_storage_degraded`
2. remaining optional detail for the cycle is dropped
3. reconcile continues without waiting for additional durability

This preserves single-writer ordering while preventing a slow disk from
stretching tick cadence indefinitely.

When a new day directory or segment is created, the controller syncs the
new file and parent directory before appending further batches.

Reader and recovery rules:

- a torn final line is tolerated and reported as one tail-loss marker
- records are trusted only through the last valid `batch_commit`
- interior corruption causes the remainder of that segment to be
  quarantined and a new segment to be opened
- startup repair records corruption and quarantine actions in-band when
  possible and via stderr otherwise

Crash-loss budget: at most records after the last successful durable
batch.

### Deterministic ordering

Within each emitted batch, records are ordered deterministically. Maps,
sets, mutation diffs, env entries, template lists, and grouped results
must be sorted before emission. `seq` records append order, but logical
ordering must also preserve observed causal order:

- records in one causal stream keep their controller-observed
  `capture_index`
- grouping/sorting may reorder only unrelated entities
- the tracer may not invert the observed order of causally related
  decision, operation, mutation, or promotion records

This keeps ordering deterministic without inventing semantically false
timelines.

### Secure file handling

The trace root and control files are created owner-only:

- directories: `0700`
- files: `0600`

The controller and offline CLI must refuse to proceed if secure
permissions cannot be established. File creation and rewrite paths are
symlink-safe and must not follow attacker-controlled links.

## Baseline Summary Model

### Always-on city-level summaries

Every cycle emits:

- `cycle_start`
- `cycle_input_snapshot`
- `cycle_result`

These baseline records are always on.

`cycle_result` includes a compact rollup such as:

- active and detailed template counts
- templates touched
- decision counts
- operation counts
- mutation counts
- reason and outcome counts
- auto arms triggered in that cycle

### Minimal evaluated-template baseline plus richer active summaries

Every evaluated template emits a compact `template_tick_summary`
baseline record with:

- `evaluation_status`
- exact template selector
- demand summary
- dependency-blocked state
- cap / partial-store / missing-template reason when applicable
- `completeness_status`

Active templates then emit richer baseline/detail records as needed.

Examples of richer activity include:

- non-zero demand
- matching open sessions
- start/drain activity
- dependency blocking
- anomaly conditions

Baseline per-session summaries are emitted only for sessions tied to
those active templates or sessions whose state changed during the tick.

This preserves “evaluated and skipped” proof for every template while
keeping the larger per-session layer activity-gated.

### Template summaries even when no session exists

If a template is armed but has zero matching open session beads, the
cycle still emits a template-scoped summary record describing the demand
state and the reason no session matched.

## Config and Reload Visibility

### Explicit reload records

When the controller processes a config reload, it emits a
`config_reload` record with:

- previous and new config revisions
- outcome: `applied`, `no_change`, or `failed`
- compact diff summary
- added, removed, and changed template identities
- provider swap signal when relevant
- reload error when applicable

### Per-cycle config snapshots for detailed templates

For each detailed template, the tracer emits one
`template_config_snapshot` record every traced cycle, even if the
effective config did not change. This keeps every cycle self-contained
for later analysis.

The snapshot includes:

- effective agent/template fields
- resolved provider identity and options
- effective min/max/scale settings
- `source_dir`
- config provenance paths
- config fingerprints

## Completeness and Loss Accounting

### Explicit completeness markers

Every cycle begins with `cycle_start` and should end with
`cycle_result`. The absence of a matching `cycle_result` after
`cycle_start` is itself evidence of process death or abrupt shutdown.

`cycle_result` carries:

- `completion_status`
- `record_count`
- `duration_ms`
- `seq_start`
- `seq_end`
- cycle rollup counts
- per-entity loss summaries

`template_tick_summary` and `session_result` also carry
`completeness_status`, for example:

- `complete`
- `partial_loss`
- `not_traced`
- `promotion_partial_context`

### Loss accounting

Because tracing is best-effort, the trace must make loss visible.

`cycle_result` and any needed intermediate state carry:

- `dropped_record_count`
- `dropped_batch_count`
- `drop_reason_counts`

Per-field truncation is tracked separately on the truncated field
wrappers.

## Retention

V1 uses one simple retention policy for the whole trace stream:

- maximum age: 7 days
- maximum total size per city: 1 GiB

Retention prunes oldest day files first until both constraints are
satisfied, but with two protections:

- pruning runs in a maintenance pass outside the reconcile critical path
- files containing the last 24 hours of detailed anomaly windows are
  protected unless the city is already in emergency low-space mode

If the host reaches low-space mode, the tracer emits explicit
`low_space` and `retention_emergency` markers and degrades to baseline
only until space recovers.

Suggested hysteresis:

- enter low-space mode when free space falls below 128 MiB or an append
  returns `ENOSPC`
- exit only after free space rises above 256 MiB for two consecutive
  maintenance passes

## Safety and Rollout

### Non-interference is a hard rule

Tracing code must never affect reconcile decisions or tick completion.

Requirements:

- tracing failures are best-effort only
- serialization errors never fail the tick
- writer errors never fail the tick
- panic recovery contains trace-only panics
- overflow and low-space modes degrade tracing rather than delaying the
  controller

### Emergency kill switch

Although normal operation stays config-free, v1 includes a hidden
emergency disable path for safe rollout. This can be a process env var
or hidden runtime flag. It is not part of the normal operator workflow.

## Testing

The trace schema is part of the product and must be tested as such.

V1 adds deterministic scenario tests that assert emitted records and
stable codes for representative cases including:

- no-demand / no matching session
- scale-check demand accepted or rejected by caps
- blocked on dependencies
- store-partial drain suppression
- config-drift drain
- pending-create rollback
- wake success / wake failure
- drain timeout / cancel / complete
- config reload success and failure
- mid-cycle auto-trigger promotion
- template missing after reload

Golden-style expectations should validate structured records, not only
human-readable stderr output.

Phase 1 also adds:

- schema contract tests for every record type
- vocabulary registry tests that fail on undeclared or removed codes
- reconstruction tests proving a consumer can rebuild in-cycle state for
  a detailed template from baseline plus deltas
- durability tests for torn-tail recovery, interior corruption
  quarantine, ENOSPC, and low-space degradation
- slow-append and slow-`fdatasync` tests that prove the controller
  honors flush wait budgets
- performance tests that enforce baseline/detail/promotion budgets

## Phase 2: Incident Bundles

Once Phase 1 trace records exist, Gas City can build richer bug reports
as a derived product rather than inventing a second source of truth.

That later work can:

- gather all records for one trace or template/time window
- join transcripts, session logs, and event bus slices using the shared
  correlation fields
- add redaction or export policies
- build summarized evidence bundles for humans and AI

That phase depends on this trace stream being complete, stable, and easy
to correlate.
