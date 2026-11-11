# Budget deferral and demand eligibility

PM intake: `ga-55zldp35`. Architecture handoff: `ga-0bqv5p8k` (P2).
Planning base: `607e3570e3fa2409cb6d216c36a243b35a99d6fb`.

## User impact and scope decision

Work withheld by the admission budget must not create fresh demand seats that
cannot claim it. Other eligible work for the same template must still run.
Acceptance must demonstrate the actual demand and spawn decision, as well as
accurate diagnostics.

Keep this P2 incident separate from `ga-5sxxd0w`, the existing P3 status-blocked
diagnostic and drain-ack fix. That bead has an architect ruling and does not
change capacity demand. The route-exclusion axis in `ga-yngwfn` also remains a
separate scope. Neither is a blocking prerequisite for this incident.

The proposed predicate edit in the intake is insufficiently specified to send
straight to implementation. The demand-counting path differs from the
diagnostic path, and the live pack and claim helper have different expiration
rules. The architect must resolve that contract before implementation is
decomposed. One architecture child carries this decision; PM has not created
overlapping test and implementation beads.

## Evidence and its limits

The mayor and beads/builder report repeated `no_work` drains and a false
"still claimable" divergence for budget-parked `be-r6jcc`. PM independently
read 15 beads/builder session records created from 2026-11-10T18:03:32Z through
2026-11-11T02:26:03Z: 14 were closed and the latest was active at inspection.
That confirms the session count and window, rather than the trigger and drain
reason of every individual session.

Read-only source inspection on the planning base found:

- Actual default demand collection uses
  `defaultScaleCheckCountsAndDemand` in `cmd/gc/build_desired_state.go`, then
  `demandServableForTemplates` and `demandRowServable` in
  `cmd/gc/demand_serve_predicate.go`. The collector increments the template
  count and records `WorkBeadIDs`. Both the legacy collector and
  `cmd/gc/reconcile_gather_demand.go` call this collector.
- `classifyDemandRowClaimability` is consumed by
  `cmd/gc/demand_divergence.go` and the drain-ack open arm in
  `cmd/gc/session_reconciler.go`. Changing this classification alone does not
  demonstrate that capacity demand or fresh session creation stops.
- `hookCandidateBudgetDeferred` in `cmd/gc/cmd_hook_claim.go` trims and parses
  RFC3339 data, defers only a strict future instant, and treats absent or
  malformed data as eligible with respect to this check. Its tests already
  cover future, past, missing, malformed, fresh-claim and reclaim cases.
- The deployed `packs/actual/builder/pack.toml` query in the management repo
  excludes any nonempty `gc.budget_deferred_until` or
  `gc.rebuild_deferred_until`, regardless of expiration or validity. Its
  comments explain that re-admission depends on the live daily budget and
  belongs to dispatch, which clears the stamps on successful admission.
  `routing-invariant.sh` recognizes stale stamps hidden by this query.
  No corresponding production Go read of the rebuild key was found.

Consequently, "expired is claimable today" describes the budget helper's
individual check, rather than an established end-to-end serving contract.
Removing the pack's presence exclusion without a ruling could bypass its
re-admission guard. This plan leaves the policy and implementation choice to
the architect.

`gc trace status` reported the orchestrator running with no active detail
arms. The read-only reasons query for beads/builder over 12 hours reported
`retained=2340` without proving the causal cycle. PM did not arm tracing,
reproduce against production work, change runtime state, or launch a model.

## Protected mitigation and rollout

At inspection, `be-r6jcc` was open and unassigned, routed to beads/builder,
with both its budget stamp and the mayor's ordinary `defer_until` equal to
2026-11-12T00:00:00Z. PM leaves these fields untouched. The workaround preserves
the intended admission time while suppressing demand for this particular row.

The mayor owns its exit. A plan, review, or closed implementation bead is not
evidence that the running orchestrator contains a fix. The eventual rollout
must identify landed code, establish that the running binary includes it, and
record a bounded read-only observation of corrected demand. Source-work
execution still follows its existing admission and authorization rules.

## Work package and acceptance

| Bead | Priority | Intake | Required outcome |
| --- | --- | --- | --- |
| `ga-0bqv5p8k` | P2 | `needs-architecture` | Buildable demand/claim/serving contract and one test-first implementation scope |

The child has native `discovered-from:ga-55zldp35` provenance and a
`related:ga-5sxxd0w` reference. There is no blocking dependency on the separate
P3 diagnostic work. No external tracker skill is installed; tracker import
is a no-op.

The architect's deliverable must satisfy these measurable requirements:

1. Trace the path that actually creates a fresh seat and name the relevant
   eligibility entry points, including the applicable legacy and v2 paths.
   A future budget stamp alone, without the ordinary `defer_until` workaround,
   must yield zero capacity demand and no runtime `Start`. An eligible sibling
   for the same template must still yield demand.
2. Define future, equality, past, absent, blank, whitespace, malformed and
   timezone-offset cases across demand, serving and claim. Resolve both keys
   where their observed serving exclusion applies. Preserve the existing
   strict-future/fail-open helper behavior unless the design explicitly rules
   on a broader policy change. State how expiration and admission remain
   consistent with the current presence-only pack guard.
3. Keep deferral diagnostics accurate: no false "still claimable" report or
   drain-ack assigned-work alarm for a deferred row. Preserve the existing
   status-blocked ruling, classification precedence and fail-closed read-error
   behavior. Justify any event or wire change separately.
4. Specify deterministic tests with injected time for actual collection and
   spawn decisions, the boundary cases, eligible siblings, and ordinary hold,
   dependency and query-failure controls. Evidence must include named passing
   tests; a helper-only pass cannot establish the incident is fixed. Tests
   require no real model sessions, long sleeps, quota bypass or production
   re-stamping.
5. Keep admission policy in the existing configuration and admission machinery.
   Preserve the worker, typed-store and event boundaries. No hardcoded role,
   new primitive, blanket template suppression, new periodic cleanup or
   automatic mutation of protected source work is authorized. Identify the
   owning `AGENTS.md` update if an invariant changes.
6. Return one `needs-tests` bead with the full implementation scope and ordering
   guard under `Build:`. The validator passes that same bead to the builder.
   If cross-repository pack work is required, identify its owner and represent
   any real landing prerequisite with a same-store tracker. Do not create a
   second build bead for the same scope.
7. Require targeted and full Bazel gates and the matching integration tier for
   lifecycle behavior. Require zero complexity growth against the pinned
   target merge base under the full shared complexity rule: a green base
   requires a green merge; otherwise compare base/merge rows and require no
   new or regressed threshold or deleted still-live function. Feature work
   must not edit `engdocs/complexity-baseline.json`. Any API, event or schema
   changes also carry their owning typed-wire and dashboard gates.
8. State the rollout evidence and mayor-owned mitigation exit described above.

PM completion means a committed, pushed, verified-clean plan and a verified
architecture route and context handoff. It does not mean the incident has
been fixed or the protected source work may execute.
