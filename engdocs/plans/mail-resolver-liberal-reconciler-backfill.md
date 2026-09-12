# Plan: mail-resolver liberal canonical match + reconciler metadata backfill

> **Status:** decomposing — 2026-05-11
> **Bug parent:** `ga-pjw3ai` — mail-send to `gascity/designer` fails
> with `"configured named session conflict: gascity/designer conflicts
> with configured named session gascity/designer via live bead
> gm-sndiot"`. Root cause: `gm-sndiot` is a manual-origin session bead
> with `agent_name`/`alias`/`template` all equal to `gascity/designer`
> but missing the `configured_named_*` metadata family (likely lost in
> the 2026-05-07 dolt→PG migration).
> **Architect spec:** `ga-pjw3ai` body (two-layer fix: resolver
> liberalization + reconciler backfill).
> **Designer spec:** `ga-t7m2n7` (~1100-line design body) pins the
> shared predicate `BeadIsLikelyConfiguredNamedOwner`, the verbatim
> `FindCanonicalNamedSessionBead` third branch with tie-break, the
> `repairConfiguredNamedSessionMetadata` method signature + scan
> outline, the `SessionMetadataRepaired` event constant +
> `SessionMetadataRepairedPayload` struct + `Reason` enum const, and
> 11 test names with assertion text.
> Architect builder estimate: ~250 LOC, 1 PR.
> **Designer handoff mail:** `gm-tuhhb8` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-otaulw`.

## Context

The bug originated when sending mail from a worktree to a configured
named session whose bead lost its `configured_named_session`
identifier-family metadata. The architect's bug analysis on `ga-pjw3ai`
established two facts that drive this slice:

1. The bead **does** exist in the city store; the rig-local `bd` view
   simply doesn't see it. The resolver was correctly seeing a live
   session bead — the diagnosis "stale alias cache" was wrong on the
   surface.
2. `gm-sndiot` carries a complete three-key match
   (`agent_name=alias=template=gascity/designer`) but is missing the
   `configured_named_session=true` metadata family. The resolver's
   strict and loose branches both filter on that family, so the bead
   falls through to conflict detection.

The two-layer fix:

- **Resolver third branch (Layer 1)** — read-path fallback. When the
  three-key match holds AND `NamedSessionContinuityEligible(b)`
  passes, treat the bead as canonical. Tie-break by
  `continuation_epoch` desc, `last_woke_at` desc, `updated_at` desc
  (defense-in-depth for the edge case of multiple matches; should be
  0 or 1 in practice).
- **Reconciler backfill (Layer 2)** — convergence to canonical state.
  Every reconcile tick scans open session beads with the same predicate
  and writes the missing `configured_named_session=true`,
  `configured_named_identity=<spec.Identity>`,
  `configured_named_mode=<spec.Mode>` keys via
  `Store.SetMetadataBatch`. Emits typed `session.metadata_repaired`
  events. Best-effort: store-write errors log to stderr and the scan
  continues; mail-send keeps working via the resolver's liberal branch
  in the meantime.

Both layers share `BeadIsLikelyConfiguredNamedOwner` in
`internal/session/named_config.go` — a pure shape check that does NOT
call `NamedSessionContinuityEligible`. Eligibility is the caller's
responsibility (both call sites apply it themselves).

## Plan

One builder bead. The designer's pin is exhaustive: verbatim Go for
the predicate, the resolver function body, the reconciler method
contract + scan outline + wiring diff, the event constant + payload
struct + reason enum, and 11 test names with assertion text. No
further design tier is needed.

| Builder bead | PR shape | Files (key) |
|---|---|---|
| **Predicate + resolver branch + reconciler backfill + event + 11 tests** | One PR, ~250 LOC | `internal/session/named_config.go` (extend), `cmd/gc/repair_named_session_metadata.go` (new), `cmd/gc/city_runtime.go` (5-line wiring), `internal/events/events.go` (constant + KnownEventTypes), `internal/api/event_payloads.go` (struct + const + register), `internal/session/named_config_test.go` (extend, 6 tests), `cmd/gc/city_runtime_named_metadata_repair_test.go` (new, 4 tests) |

### `ga-otaulw` — mail-resolver liberal canonical match + reconciler metadata backfill (P2, `ready-to-build`)

Implements the two-layer fix from `ga-pjw3ai` per the design pin in
`ga-t7m2n7`. Routed to `gascity/builder` via `gc.routed_to`;
`gc.design_parent=ga-t7m2n7` records the back-link.

**Blocked by:** none. The slice is independent of the PG-auth chain
(designer's §8 handoff plan explicitly notes LOCAL-ONLY posture is
not required).

**Acceptance criteria summary** (full list in the bead body):

- `BeadIsLikelyConfiguredNamedOwner` is the predicate; both layers
  call it (do not inline).
- Strict and loose branches of `FindCanonicalNamedSessionBead` are
  byte-for-byte behaviorally unchanged.
- Reconciler writes via `Store.SetMetadataBatch` (NOT `UpdateMetadata`
  — does not exist).
- `configured_named_mode` is written verbatim from `spec.Mode` (NOT
  defaulted to `"always"` — the spec's mode is never empty).
- Store-write failure → stderr log only, no `session.repair_failed`
  event (best-effort; does not propagate to the cycle's outer return).
- Reconciler-tick wiring: backfill runs IMMEDIATELY after
  `loadSessionBeadSnapshot`, BEFORE `rigStores := cr.rigBeadStores()`.
  On `repaired > 0`, snapshot is reloaded before downstream tick
  steps consume it.
- Event `Subject = <bead-id>` (bare, NOT `sessions/<bead-id>`).
- All 11 tests from design §5 implemented and passing (including
  `TestSessionReconciler_BackfillsMissingConfiguredNamedMetadata`
  with idempotency check on second tick).
- `TestEveryKnownEventTypeHasRegisteredPayload` passes (auto-
  satisfied once §4 changes land).
- `TestOpenAPISpecInSync` passes.

**HARD RULES carried from design (bead body §"HARD RULES" — 10
items):**

- Predicate conjunction `agent_name == backing && alias == identity
  && template == backing`, all normalized via
  `NormalizeNamedSessionTarget`.
- Predicate does NOT call `NamedSessionContinuityEligible`.
- Write via `Store.SetMetadataBatch(id, kvs)`.
- Write `spec.Mode` verbatim.
- Store-write failure is best-effort; no `session.repair_failed`
  event.
- Event `Subject = <bead-id>` (bare).
- Strict branch wins over liberal.
- Liberal branch does NOT match closed beads.
- Backfill is idempotent (second cycle = 0 writes, 0 events).
- `buildDesiredState` wiring is NOT required.

## Routing

- Builder bead `ga-otaulw` carries `gc.routed_to=gascity/builder`
  and label `ready-to-build`. Builder will see it via the Tier-3
  pool-queue query.
- `gc sling gascity/builder ga-otaulw` wakes the builder session
  immediately after handoff.
- Mail to builder via `gc mail send gascity/builder` with subject
  pointing at the bead ID for context.

## Risk / non-goals

- This slice does NOT diagnose the original producer of the broken
  metadata. The reconciler converges existing state; tracking down
  the migration-time loss is a separate investigation.
- The slice does NOT backfill `session_name` (which is load-bearing
  for tmux pane routing and downstream consumers).
- The slice does NOT add a `gc doctor` check for broken-metadata
  beads or generalize the three-key predicate.
- The slice does NOT promote manual `bd update --set-metadata` to a
  first-class command.

If operators later want a typed `session.repair_failed` signal,
file a follow-up bead. Designer explicitly excluded that event type
from this slice as scope creep.
