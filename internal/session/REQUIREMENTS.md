# Session Requirements

| Field | Value |
|---|---|
| Status | Seed draft |
| Scope | Session behavior source of truth, stored beside `internal/session` |
| Related design | `PLAN.md` owns the extraction sequence. `DESIGN.md` (not yet landed; iterating under design-review bead `ga-unpr2y`) owns long-form architecture direction. |

This document is the reconciliation ledger for session behavior. Future agents
should use it as the frame for session changes: expected behavior, implementation,
and tests must be brought back into agreement whenever any one of them changes.

## Purpose

Session is the primitive for starting, stopping, prompting, observing, and
addressing provider-backed agent sessions. A session is not the work itself.
Work is durable in beads; sessions are replaceable runtime identities that can
be created, suspended, drained, closed, resumed, or archived while work remains
recoverable.

## How To Reconcile

For every session change:

1. Read this document and the nearest `AGENTS.md`.
2. Identify the scenario rows affected by the change.
3. Update code, tests, and this document so they describe the same behavior.
4. Add a new scenario row for any new behavior or bug fix.
5. Cite proof in the row: a test path, source path, issue, commit, or command.

If a row is wrong but tests currently enforce it, update the row and tests in
the same change. If a row describes the right product behavior but code differs,
fix code and prove the row with a test.

## Canonical Vocabulary

Use the exact projection language from `internal/session/lifecycle_projection.go`:

- Base states: none, creating, start-pending, active, asleep, suspended,
  failed-create, draining, drained, archived, orphaned, closed, closing,
  quarantined, stopped.
- Desired states: undesired, desired-asleep, desired-running,
  desired-blocked.
- Runtime projections: unknown, alive, missing, fresh-creating,
  stale-creating, start-requested.
- Identity projections: none, concrete, canonical, historical,
  reserved-unmaterialized, conflict.
- Blockers: held, quarantined, missing-config, identity-conflict,
  duplicate-canonical.
- Wake causes: pending-create, pin, attached, pending, named-always, work,
  scale-demand, explicit.

Avoid inventing parallel terms such as "live", "dead", "enabled", or "active"
unless the row names how they map to the canonical projection.

## Global Invariants

- Session state is bead-backed and projected. Runtime facts are observations,
  not durable truth.
- `ProjectLifecycle` is the canonical read model for lifecycle decisions and
  user-facing projection. Callers must not scatter raw `state` or
  `sleep_reason` interpretation.
- Session targeting is exact and bounded: direct session bead ID, open exact
  `session_name`, then open exact current `alias`.
- Ordinary config names and `template:<name>` are factory/config targets, not
  live session targets.
- Closing or confirming the death of a session must not strand assigned open
  work.
- Reconciliation must be idempotent. Re-running the same controller pass should
  converge instead of consuming duplicate budgets, duplicating sessions, or
  oscillating states.

## Scenario Ledger

### Lifecycle Projection

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-LIFE-001 | Legacy compatibility states are projected | `state=awake` behaves as active. Stored `drained` remains projected as drained but keeps compat state asleep. `failed-create` remains distinct. Closed bead status wins over stale active metadata. | `internal/session/lifecycle_projection_test.go` |
| SESSION-LIFE-002 | Pending create claim | An open creating bead with `pending_create_claim=true` projects desired-running with wake cause pending-create. | `internal/session/lifecycle_projection_test.go` |
| SESSION-LIFE-003 | Holds and quarantine block wake | Future `held_until` or `quarantined_until` makes an otherwise runnable identity desired-blocked while preserving the underlying wake cause. | `internal/session/lifecycle_projection_test.go`; `cmd/gc/session_reconcile_test.go` |
| SESSION-LIFE-004 | Creating staleness | Fresh creating uses `pending_create_started_at` when present. Stale creating becomes stale-creating and reconciles away from creating; fresh creating stays creating. | `internal/session/lifecycle_projection_test.go`; `cmd/gc/session_reconcile_test.go` |
| SESSION-LIFE-005 | Runtime liveness projection | Alive runtime projects active/awake. Missing runtime projects asleep or missing unless the creating/start-pending grace rules apply. Missing due to rate limit or runtime-missing must preserve resume identity when the tests require it. | `internal/session/lifecycle_projection_test.go`; `cmd/gc/session_reconcile_test.go` |
| SESSION-LIFE-006 | Missing config | A session whose backing config target is missing is desired-blocked with blocker missing-config instead of silently waking. | `internal/session/lifecycle_projection_test.go` |
| SESSION-LIFE-007 | Terminal wake conflict | Closed or closing bead IDs cannot be woken. Archived historical beads cannot be woken by bead ID unless continuity-eligible. Active metadata on a closed bead does not override terminal status. | `internal/session/lifecycle_projection_test.go`; `internal/api/session_model_phase0_lifecycle_spec_test.go`; `internal/session/waits_test.go` |
| SESSION-LIFE-008 | User-facing projection guard | CLI/API/doctor user-facing consumers must use projection helpers rather than raw state interpretation. | `internal/session/lifecycle_projection_test.go` static guard |

### State Transitions

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-STATE-001 | Legal transition table | The only legal command transitions are the table in `internal/session/state_machine.go`. Notable edges: none -> start-pending on create; creating -> active on ready; active -> asleep on sleep; active -> draining on drain; active/asleep/suspended/quarantined/draining -> archived where listed; any non-none state -> closed on close. | `internal/session/state_machine.go`; `internal/session/state_machine_test.go` |
| SESSION-STATE-002 | Illegal transitions reject | Ready is valid only from creating, sleep only from active, wake not from active, and drain not from asleep or suspended. Unknown commands fail. | `internal/session/state_machine_test.go` |
| SESSION-STATE-003 | UI affordances follow reducer | Allowed command rendering for active sessions is derived from the transition table. | `internal/session/state_machine_test.go` |

### Identity And Targeting

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-ID-001 | Explicit session names | Human-chosen session names are trimmed, max 64 chars, must match session-name syntax, and must not use the auto `s-` prefix. | `internal/session/names.go`; `internal/session/names_test.go` |
| SESSION-ID-002 | Aliases | Aliases may include `.`, `_`, `-`, and `/` segments, but cannot be `human`, cannot use `s-`, and cannot look like `gc-<digits>`. Live alias collisions are rejected. | `internal/session/names.go`; `internal/session/names_test.go` |
| SESSION-ID-003 | Session resolution order | Normal resolution tries direct session bead ID, then open exact `session_name`, then open exact current `alias`. It does not fall through to template names, agent names, template basenames, or historical aliases. | `internal/session/resolve.go`; `internal/session/resolve_test.go` |
| SESSION-ID-004 | Read-only closed lookup | Closed sessions remain inspectable only through the allow-closed resolver, after live matches fail. | `internal/session/resolve.go`; `internal/session/resolve_test.go` |
| SESSION-ID-005 | Named identity reservation | A configured named identity without a bead is reserved-unmaterialized. It is undesired by default, desired-running when mode/wake cause requires it, and blocked by identity conflicts. | `internal/session/lifecycle_projection_test.go`; `internal/session/named_config_test.go` |
| SESSION-ID-006 | Canonical vs historical named beads | Continuity-eligible materialized named beads can remain canonical or historical as specified by projection. Duplicate canonical or conflicting live claimants block materialization. | `internal/session/lifecycle_projection_test.go`; `internal/session/named_config_test.go` |
| SESSION-ID-007 | Terminal named identity wake | Waking a closed named bead ID is rejected and does not create a successor. Waking the named identity after terminal close uses a fresh canonical bead when allowed. | `internal/api/session_model_phase0_lifecycle_spec_test.go` |
| SESSION-ID-008 | Session-targeting surfaces | API session operations reject `template:<name>` as a live session target. Bare ordinary config names do not create ordinary sessions. | `internal/api/session_model_phase0_interface_spec_test.go` |
| SESSION-ID-009 | Mail is session-targeting | Mail rejects template factory targets and bare ordinary config recipients. Bare configured named session mail uses the configured mailbox without materializing a session; existing live named session mail uses the live mailbox. | `internal/api/session_model_phase0_interface_spec_test.go` |
| SESSION-ID-010 | Aliasless multi-session identity | Aliasless multi-session or pool sessions use generated concrete runtime identities so independent sessions do not collide. | `internal/session/manager_test.go`; `cmd/gc/session_template_start_test.go` |

### Start, Wake, Suspend, Close

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-START-001 | Create state sequence | Creating a session writes a bead-backed start intent and commits runtime-start metadata through lifecycle patches. Pending create metadata must be cleared atomically when start is confirmed or rolled back. | `internal/session/lifecycle_transition_test.go`; `internal/session/manager_test.go`; `cmd/gc/session_lifecycle_parallel_test.go` |
| SESSION-START-002 | Stale create rollback | A stale creating bead with pending create metadata heals away from creating instead of oscillating back to creating. Never-started pending create can migrate to start-pending while rollback lease is active. | `cmd/gc/session_reconcile_test.go` |
| SESSION-START-003 | Explicit wake | Waking suspended/asleep/eligible archived sessions records durable explicit wake intent and clears stale wake blockers through lifecycle patches. | `internal/session/waits_test.go`; `internal/session/lifecycle_transition_test.go` |
| SESSION-START-004 | Suspend named session | Suspending a reserved configured named session materializes exactly one canonical bead in suspended state. | `internal/api/session_model_phase0_lifecycle_spec_test.go` |
| SESSION-START-005 | Close cleanup | Closing a session closes the bead, removes runtime MCP snapshots, clears bead-scoped wake/hold overrides, and can retire configured named identifiers when terminal. | `internal/session/manager_test.go`; `internal/api/session_model_phase0_lifecycle_spec_test.go` |
| SESSION-START-006 | Stop failure does not close | If provider stop fails during detailed close, the bead remains open; successful stop closes it. | `internal/session/manager_test.go` |
| SESSION-START-007 | Template override safety | Template overrides are rejected for running sessions, recent wake-in-flight sessions, and pending create claims. Suspended sessions, old wake timestamps, and failed-create states can be updated where tests allow. | `internal/session/manager_test.go` |
| SESSION-START-008 | Parallel lifecycle start | Independent start candidates can begin in the same wave before dependent sessions. A failed dependency blocks its dependent but not unrelated siblings. | `cmd/gc/session_lifecycle_parallel_test.go` |

### Reconciler, Pools, And Scaling

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-RECON-001 | Worker boundary | Production CLI session creation/lifecycle operations route through `internal/worker/handle.go`; direct manager creation is limited to documented root exceptions. | root `AGENTS.md`; `cmd/gc/worker_boundary_import_test.go` |
| SESSION-RECON-002 | Cold pool scale from zero | A min=0 pool with custom scale check returning 0 wakes one session when routed work exists in any active store, including city-store work targeting a rig-qualified agent. Demand is clamped to 1. | `cmd/gc/scale_from_zero_test.go`; commit `a2b2da046` |
| SESSION-RECON-003 | Existing rig session prevents cold wake | If a matching active session exists in the rig store, the pool is not treated as cold and the custom scale result can remain 0. | `cmd/gc/scale_from_zero_test.go` |
| SESSION-RECON-004 | Wake reasons respect holds | Holds suppress config, attached, and work wake reasons except wait-only wake where the tests explicitly preserve it. | `cmd/gc/session_reconcile_test.go` |
| SESSION-RECON-005 | Pool work wake is demand-gated | Pool sessions wake from config only when desired demand is positive. Dependency-only pool slots do not wake on generic work demand. Manual pool sessions remain config-eligible. | `cmd/gc/session_reconcile_test.go` |
| SESSION-RECON-006 | Provider health gate | When provider health is red, the reconciler skips respawn, emits one alert per red episode, and does not consume restart budget. Absent, stale, or unknown health fails open. | commit `b5a7f3be3`; `cmd/gc/provider_health_gate_test.go`; `cmd/gc/session_reconciler.go` |
| SESSION-RECON-007 | Progress-aware health | When configured, an alive runtime with no progress after the threshold is not desired and can be drained/respawned. Attached sessions, startup grace, pending interaction, and min-floor idle pool workers are exempt. Provider-health red takes precedence. | commit `dbda1e380`; PR #3113 (`TestProgressStall_MinFloorIdleWorker_NotRecycled`); `cmd/gc/session_progress_test.go`; `cmd/gc/session_reconciler.go` |
| SESSION-RECON-008 | Idle timeout ladder | An alive session idle past its threshold stops with sleep reason `idle-timeout` unless a timer blocker (user hold, quarantine) or pending interaction defers it. A pending interaction cancels any cancelable pending drain (matching generation) and keeps the session out of that tick's wake pass; blocker deferrals do not. Idle stops never consult assigned work. Precedence: blocker, then pending, then stop. | `internal/session/lifecycle_timers.go` (`DecideIdleTimeout`); `internal/session/lifecycle_timers_test.go`; `cmd/gc/session_reconciler_test.go` (`TestReconcileSessionBeads_IdleTimeout*`); `cmd/gc/session_lifecycle_chaos_test.go` (`TestSessionLifecycleChaosPendingInteractionDefersIdleTimeout`, `...CancelsExistingDrainBeforeIdleTimeout`) |
| SESSION-RECON-009 | Max session age ladder | An alive session whose age since `creation_complete_at` exceeds `max_session_age` stops with sleep reason `max-session-age` unless a timer blocker, pending interaction, or open assigned work defers it to the next tick. Assigned-work query errors fail closed as busy. Missing anchor or nil tracker skips evaluation. Precedence: blocker, then pending, then assigned work, then stop. | `internal/session/lifecycle_timers.go` (`DecideMaxSessionAge`); `internal/session/lifecycle_timers_test.go`; `cmd/gc/session_reconciler_test.go` (`TestReconcileSessionBeads_MaxSessionAge*`) |
| SESSION-RECON-010 | Dead-session exit classification | A dead session is classified through three lanes in order: rate-limit (crash candidate whose provider screen shows a rate-limit message is quarantined with sleep reason `rate_limit`, no crash counted), rapid crash (death inside the stability window records a wake failure and clears `last_woke_at`), churn band (death past stability but before productivity records churn; at or past productivity the churn counter clears). Crash candidacy requires: dead, non-subprocess provider, no pending drain, parseable `last_woke_at`, create lease not in flight. The rapid lanes ignore `pending_create_claim` and `sleep_reason`; the churn lane additionally skips on claim, deliberate sleep reasons, subprocess, and drains. Rate-limit candidacy is not band-limited. | `internal/session/lifecycle_exits.go` (`DecideSessionExit`, `IsDeliberateSleepReason`); `internal/session/lifecycle_exits_test.go`; `cmd/gc/session_reconcile_test.go` (`TestCheckStability_*`, `TestCheckChurn_*`); `cmd/gc/session_reconcile_ratelimit_test.go` |
| SESSION-RECON-011 | Crash and churn accrual | Each rapid crash advances `wake_attempts`; reaching the max quarantines with sleep reason `quarantine`. Each churn event advances `churn_count`; reaching the max quarantines with sleep reason `context-churn`. Both quarantines are metadata-only (no state-machine move). Crash and churn events force a fresh conversation: `session_key` clears and `continuation_reset_pending` is set; wake failures additionally clear `started_config_hash` so the next wake runs as a first start, churn keeps it. Rate-limit backoff sets the session asleep with cleared wake stamp and pending-create markers, without counting a crash or touching conversation metadata. | `internal/session/lifecycle_exits.go` (`WakeFailureAccrualPatch`, `ChurnAccrualPatch`, `ConversationResetPatch`, `RateLimitQuarantinePatch`); `internal/session/lifecycle_exits_test.go`; `cmd/gc/session_reconcile_test.go` (`TestRecordWakeFailure_*`); `cmd/gc/session_reconcile_ratelimit_test.go` |

### Work Release And Drain Safety

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-WORK-001 | Close releases assigned work | Successful session bead close releases non-closed work assigned by bead ID, `session_name`, or configured named identity. In-progress work reopens. The release path is idempotent. | commit `a3a3b9fcf`; `cmd/gc/session_beads_test.go` |
| SESSION-WORK-002 | Confirmed-dead pool workers release work | A pool worker confirmed dead by runtime/provider checks can be closed even when assigned work exists; closing releases the orphaned work. Suspended, orphaned, or reconfigured guards still apply. | commit `47b580e9f`; `cmd/gc/session_beads_test.go` |
| SESSION-WORK-003 | Orphan pool step beads | Open pool step beads assigned to dead session identities are collected and released after session drain without relying on stale snapshots. | commit `8068393d8`; `cmd/gc/pool_session_name_test.go` (`TestCollectAndReleaseOrphanPoolStepBead_Issue2793`) |
| SESSION-WORK-004 | No-wake drains cancel on assigned work | Pending no-wake or orphan drains are canceled when assigned work reappears, and recovered drain-ack metadata is not allowed to suppress that wake demand. | commit `d565a34e2`; `cmd/gc/session_reconciler_test.go`; `cmd/gc/session_wake_test.go` |

### Runtime, Submit, And Observation

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| SESSION-RUNTIME-001 | Runtime observation fallback | Runtime observation can treat a live process as running when a session probe false-negatives. Providers without process names do not force a running session dead. | `internal/session/manager_test.go` |
| SESSION-RUNTIME-002 | Pending/respond missing runtime | If a runtime session is missing, pending/respond surfaces treat it as no pending interaction rather than panicking or claiming a pending state. | `internal/session/manager_test.go` |
| SESSION-RUNTIME-003 | ACP routing | ACP sessions route through the auto provider at creation; suspended ACP sessions resume on the ACP backend; active ACP sessions can reroute before nudge. | `internal/session/manager_test.go`; `internal/session/submit_test.go` |
| SESSION-RUNTIME-004 | Stop turn | Stop-turn interrupts active sessions and is allowed for pool-managed and pool-slot-only sessions where tests permit it. | `internal/session/manager_test.go`; `internal/session/submit_test.go`; `internal/session/submit_family_test.go` |
| SESSION-RUNTIME-005 | Transcript lookup | Transcript paths prefer session key, allow closed sessions, avoid ambiguous historical work-dir fallback, and use provider-specific fallback when work dirs collide across providers. | `internal/session/manager_test.go` |

## Maintenance Rules

- Add one row per behavior scenario, not one paragraph per file.
- Prefer concrete input and expected output over broad prose.
- Keep evidence current. If a cited test is deleted, move or replace the row.
- Do not use this document as a scratchpad for implementation plans. Capture
  durable requirements, invariants, scenario outcomes, and proof only.
