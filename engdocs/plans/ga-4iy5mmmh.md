# Plan: accurate recovered-write classes

Work: `ga-4iy5mmmh` · Priority: P4 · Date: 2026-11-26 UTC

## Outcome

A write recovered on its final retry must report the approved class semantics
instead of always claiming a serialization conflict. Keep this one small
implementation scope on the existing work bead. Preserve retry decisions,
budgets, attempts, delays, write results and other telemetry behavior.

The predecessor is deploy `ga-0oh7fg94`, reviewed at
`95da871e403db500e002b5624bcc7020b6f70f44` in review `ga-29m0s0bd`. Its
`recordWriteBudget` success branch uses the constant `serialization`, although
the callers also retry transient and CAS conflicts. The code describes class
as a closed metric-label set. A final success supplies no error to classify.

## Decision before implementation

The reviewer suggests preserving the last retried error and classifying it,
or using a distinct fixed recovery token. These give the metric different
meanings. PM leaves that contract to architecture bead `ga-0x5jed28`.

The architect records the exact finite allowed values, their meaning for
serialization/transient/CAS/mixed histories, compatibility and cardinality
effects, and a concrete Build/RED-GREEN contract on the decision and source
beads. Design may proceed from the reviewed source before landing; it does
not commission implementation or changes to the predecessor head.

## Dependency graph and ownership

| Record | Purpose | Route or owner |
| --- | --- | --- |
| `ga-0x5jed28` | Approve recovered-final class contract | Architect; `needs-architecture` |
| `ga-0oh7fg94` | Existing predecessor deploy | Existing deploy/merge chain |
| `ga-bygzs8i0` | Verify predecessor actual main landing | Unassigned, unrouted landing record; never sling |
| `ga-4iy5mmmh` | Single future test/build scope | PM while the prerequisites are open |

The work has same-store `blocks` edges to all three prerequisites. The new
decision and landing records carry `discovered-from` provenance to the source,
not scheduling edges back to it. The scheduling graph is acyclic.

Fetched main `77143ecd18ff6f0199dfee3a412825f1fd9fa8a2` lacks the reviewed
write-budget contract. Deploy `ga-0oh7fg94` was open and routed to deployer at
intake. A gate PASS or closed build/deploy does not prove landing.

Mayor accepted landing-record closure in the existing merge chain in mail
`gcg-17205249` and recorded ownership on `ga-bygzs8i0`.
Closure requires the predecessor PR/main squash SHA, normal squash/work-record
evidence, and inspection of main's write-budget helper, retry-loop telemetry
wiring and contract-test coverage. Then notify PM with `--notify`, naming the
source and landing record. If abandoned or materially changed, leave the
record open and notify PM to revise the follow-up.

After both the approved decision and verified landing, PM re-reads the source,
main, contract and prerequisite outcomes, and routes this SAME P4 bead
`needs-tests` to validator. Validator hands the same bead to builder. There is
no second implementation bead, stacked `base_ref`, predecessor-head edit or
premature machinery admission.

## Acceptance

- Recovered-final class values match the architect-approved finite contract,
  including relevant serialization, transient, CAS and mixed retry histories.
  The PM plan does not select the mechanism or invent a token.
- A meaningful regression fails before the repair and passes after it, with
  named Bazel PASS evidence. Generic successful-write tests that do not assert
  class are insufficient. Keep existing contract coverage.
- Preserve counters, operation/outcome label shape and warning policy, except
  an explicitly approved bounded class-value change. Do not add per-bead
  high-cardinality metric labels or change retry/write semantics.
- Existing exhaustion classification, below-budget silence and permanent
  failure on the last lap remain green. `make check` and applicable owning
  package checks pass. The small PR carries end-to-end evidence.
- Complexity has zero growth against the immutable pinned base. Use gate
  `GATE_BASE_SHA`, or the recorded target merge base outside a gate. A green
  base requires a green merge check. Otherwise retain base/merge outputs with
  no worsened row and a successful diff measurement with no new, regressed or
  still-living deleted function. Never update the feature's complexity
  baseline or accept exit status alone as proof.

## PM disposition

Commit and publish this plan, verify architecture routing and landing-owner
notifications, then release PM's claim. Keep the source open with `needs-pm`
behind its prerequisites. This plan commissions a decision and records a
future fix; it claims no implementation or landing. Tracker import is a
skip-safe no-op because neither supported importer exists.
