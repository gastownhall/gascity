# Plan: bound unresolved terminal usage retries

Root: `ga-2a5ttbk6` · P4 · generated 2026-11-25 UTC.
Planning base: `e5727ef42bf9e0ed59cfecf64df31aeb89a184ef`.

Unresolved terminal intervals currently repeat discovery and append the same
compute fact on every reconcile pass. Read-time deduplication prevents those
physical lines from becoming duplicate charged usage. This work bounds the
repetition while continuing to retry late usage indefinitely for an unchanged
interval identity.

## Approved contract

The authority is the block `ARCHITECT CONTRACT v2 (AMENDED, 120 s CAP)` on
`ga-2a5ttbk6`, approved by PM after rulings R1-R5. V1 is superseded. The
complete v2 block has SHA-256
`2c0bbee8eef23773488b0845fd8ec27bb6bbebc795083bb00433fee13a14873e`.
The child carries sections 4 and 5 verbatim, with SHA-256
`0a62f48b779f8fd5cbe0468ffd2d9f9d8c58de90aab32f1e6f94627d75e7c289`.

The wait after consecutive unsettled probes is 30, 60, 120, 120 seconds,
then remains at 120 seconds. Service occurs on the next actual reconcile
pass. There is no give-up and no shorter eligibility or retention window.
An unchanged identity appends one unsettled compute fact per process;
subsequent unsettled probes append nothing. The existing commit path remains.
Epoch, key or config changes re-probe immediately and restart the schedule.

The two-minute bound is a scheduling limit, not a wall-clock availability
guarantee. A session can wake again before a delayed probe recovers its old
epoch; PM explicitly accepted that exposure for a maximum scheduled gap of
120 seconds. The contract makes no loss-free equivalence claim.

## One work package

| Bead | Scope | Route |
|---|---|---|
| `ga-7dnldt4q` | Requirements and Build/RED matrix from v2 sections 4 and 5 | `needs-tests` → validator → builder, on the same bead |

Exactly one child is authorized. It has a `discovered-from` edge to the root
and no blocking dependency. No separate build bead or blockers on discovery
K, L, deferred T or bounded extraction are added. Priority remains P4.

The approved file scope is:

- `cmd/gc/city_runtime.go`: in-process terminal-sweep memo storage.
- `cmd/gc/usage_compute.go`: cadence, unsettled-append bookkeeping, memo
  cleanup and comments, within the approved helpers and boundaries.
- `cmd/gc/usage_compute_test.go`: the v2 T1-T17b matrix. The existing T12
  test changes only clock injection; its assertions remain.
- `internal/session/fields.go`: rename only the existing
  `usage_model_swept_at` writer-site descriptor from
  `CityRuntime.emitDueComputeFacts` to `CityRuntime.settleTerminalInterval`.
- `engdocs/design/usage-facts-v0.md`: the approved paragraph describing
  cadence, append behavior, process-local state and recovery exposure.

The registry edit records the moved write; it adds no field, field class,
external mutation site, lifecycle rule or session ownership boundary.
Additional session changes return to architecture and PM for scope approval.
Standing owning `AGENTS.md` rules apply to any discovered invariant change.

## Acceptance and evidence

The finite model predicts 62 probe times over 720 ten-second ticks, 722
probes in the first day and 720 per day thereafter for an unchanged identity
without resets. The cap notice occurs on the third unsettled outcome.
Restart, reload and identity-change costs follow FR-7 rather than the daily
bound for an unchanged identity.

PM extracted all 17 Annex files from the live bead and verified their hashes.
Running the pure `schedule_tieout.py` model ended `ALL ASSERTIONS HOLD` and
matched its expected output byte for byte. This establishes internal schedule
consistency; it does not prove the implementation. Historical replay and
exposure counts retain the architect's evidence classes and limitations.
Per-probe CPU and filesystem cost remain unmeasured.

The validator proves the named RED assertions through Bazel and hands this
same bead to the builder. The builder keeps every applicable v2 row green,
including late recovery, no give-up, independent sessions, identity changes,
restart/reload, sink and marker failures, unchanged lanes, fact identity and
the registry checks. Filtered evidence must contain actual named test results.
An infeasible or contradictory row returns to the architect.

Required checks include the named `cmd/gc` tests,
`//internal/session:session_test`, the registry checks and the applicable
repository gates. Acceptance/integration tiers follow any actual affected
driver. Complexity must show zero growth against the pinned base under the
standing rule: no worse diagnostics, new or regressed row, or deleted row for
a function that still lives. Improved rows are allowed. The feature never
edits `engdocs/complexity-baseline.json` or waits on its refresh.

## Handoff and scope limits

PM closes the root only after this plan is committed, pushed and verified
clean. That closure records completed packaging, not a shipped fix. The child
continues through the normal review and landing process; no claim is held to
watch it.

Fact fields, keys, RunID, attribution, Discard behavior, provider settlement
rules, live/closed behavior and worker/typed-store boundaries remain as
specified in v2. Compaction, sink rewrite/deletion, retention changes, key
repair, neighboring-transcript selection, the factory-failure observation,
backfill, pricing and search-root expansion remain outside this work. No
live sweep, fact replay or fleet binary operation is authorized. Normal
Bazel test/build validation of the approved change is authorized.

Tracker import is a no-op: no importer or manifest is installed. The source
bead retains the architecture, approval and Annex evidence; the child retains
the complete implementation contract.
