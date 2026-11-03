# Orders in controller-backed gc status: decision brief

Operators should see order-health problems when `gc status` uses its API
route. PR #6486 currently adds the Orders block only to the local collector.
The API view has no order-health field, so a healthy controller can make that
block disappear from the operator's view.

Owner: PM bead `ga-io9q5wh`, derived from architecture record `ga-k2v17pq`.
Planning base: `0b202116befc5fc82feb7e9a1fe0cc57c3cae30e`.
Status: **proposal awaiting the operator's choice**. Recommendations below
do not authorize implementation or change the architect's settled D1-D5.

## Choose the product scope

| Choice | Operator outcome | Cost and limitation |
|---|---|---|
| C1: controller-served order health — PM recommendation | The API text route can show order health without the CLI reading local event logs | Controller memoization, typed optional API field, generated clients/spec and dashboard verification; exact shape waits for bounded-reader findings |
| C2: bounded CLI leg as an interim | The API text route can show order health when local logs are accessible | Reuses the bounded reader and avoids a wire change; retains local-log access as a product restriction and adds CLI work |
| Defer route parity | Keep the local-fallback-only feature and state its limitation | API-route operators still lack the Orders block |

The architect recommends C1 as the end state and permits C2 as an interim
after the bounded reader. PM recommends C1 directly: its coverage does not
depend on which route an operator happens to take or whether local logs are
available. The route's historical usage distribution is unmeasured; no
claim about how many operators use either route supports this recommendation.

## Confirm the JSON scope separately

PM recommends keeping the current `gc status --json` contract unchanged in
this scope, consistent with ruling D4. The closed schema at
`schemas/status/result.schema.json` currently declares no Orders field.

If the operator instead requires machine-readable Orders here, record that
choice explicitly. It requires an optional declared addition on both routes,
schema and compatibility checks, and reconciliation of existing
work. The architect has closed `ga-k6ofaz.3` as abandoned under D4; its
older implementation spec must stay retired. `ga-kbwrop.1` remains an open
builder vehicle and needs reconciliation against that ruling before any
JSON implementation. A newly approved C1 JSON extension would need a named
child against the current schema, with parity coverage for both routes.
An optional field on the HTTP status response alone does not authorize a
new key in CLI `StatusJSON`.

Scripts can already use `gc doctor --json --check order-firing-current
--check order-outcome-healthy`. Child A adds typed non-OK findings and child
B adds the bounded mode. This addresses the standing machine-readable
health request while the operator decides the status command's scope.

## Existing prerequisites and sequencing

| Work | Existing bead | Boundary |
|---|---|---|
| A: correct findings, bound the local leg to 3 s, omit JSON scan | `ga-4c9nly7` | Revision of PR #6486; D1-D4 only |
| B: sound bounded outcome reader and explicit Unresolved | `ga-w17lm4u` | Separate work from main; preserve exact doctor semantics |
| C: this route-parity decision and resulting breakdown | `ga-io9q5wh` | Separate from PR #6486; no implementation choice accepted yet |

A and B already carry their implementation requirements and builder routes.
They are budget-deferred at intake. This plan changes neither their scope
nor their budget state. C1 design preparation can proceed after approval,
but its wire shape must wait for B's findings. C implementation follows the
prerequisite outcomes and their verified landing, rather than treating a
builder handoff close as delivery.

## Conditional C1 breakdown, not yet filed

| Proposed scope | Acceptance boundary | Ordering |
|---|---|---|
| Controller-owned health view | Reuse domain verdicts; compute on the controller's cadence and memoize; unavailable evidence remains explicit | Confirm the architectural seam, cadence and freshness contract with the architect after B's findings |
| Typed status API projection | Add an optional typed status field and client conversion; status requests perform no inline order scan; Lite omits it; older responses remain decodable | Approved view contract, then source/spec/client changes together |
| CLI text-route parity | Map API findings to the same text rows as the local route; healthy orders never appear; unresolved checks remain visible; preserve the chosen JSON contract | A/B outcomes plus the API projection |

Each implementation scope would use one test-first carrier through validator
and builder, with same-store dependency edges. No implementation beads have
been created from this proposal. If C2 or defer is selected, replace this
conditional breakdown with the chosen scope before dispatching work.

## Acceptance and verification after the decision

The chosen work must preserve the existing domain ownership of order
verdicts, doctor detail strings, worst-first findings and the cap of 20.
It must expose unavailable evidence honestly and leave agent `Partial`
semantics intact. The architecture record's bounded-reader soundness rule
remains authoritative; the CLI must never parse doctor prose to infer health.

Implementation verification uses the owning Bazel targets, including
`//cmd/gc:gc_test` and, for C1, `//internal/api:api_test`, generated-artifact
and API-contract coverage. Filtered evidence must show real named PASS
results. API changes also require `make dashboard-check` and the manual
dashboard preview required by `internal/api/AGENTS.md`. Regenerate BUILD
inputs when files or imports change. Complexity must have zero growth
against the pinned base; never refresh `engdocs/complexity-baseline.json`
in this feature.

The architect's live-city timing measurements and extrapolation are inputs
to the decision, not PM test results. New live timing must use the approved
temporary-binary procedure and reaper override; this task authorizes no
installation or production deployment.

## Decision handoff

The source assignment explicitly requires PM to decide with the operator.
Mayor receives the committed brief and decision record `ga-ozm0c17`, asking
for the route choice, whether an interim is authorized, and JSON scope.
Only an actual operator selection, or a mayor relay quoting its source,
clears that record. PM keeps `ga-io9q5wh` open behind that dependency and
releases its claim while waiting. Mayor acknowledged the request in
`gm-wisp-ky7gf1`, added it to the morning operator message, and will record
the actual answer with its source; no choice has been accepted. After the
decision, PM updates this plan,
reconciles architectural details and existing JSON scopes, creates the chosen
work packages, verifies their handoffs, and closes planning only after the
artifact is committed and clean.
