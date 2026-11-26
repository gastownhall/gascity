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

## Approved class contract

PM accepted the architect's decision and B0-B6 Build/RED-GREEN contract on
2026-11-26, after reading mail `gcg-17218561` and verifying the contract text
matches verbatim on `ga-0x5jed28` and `ga-4iy5mmmh`. The decision bead is
closed as a design deliverable with outcome `no-op`; nothing has shipped.

For `recovered-final`, class names the most recent conflict the retry loop
retried. For `exhausted`, class continues to name the final error. Mixed
histories use the last retried conflict, not a census of the whole history.

| Most recent retried conflict | Recovered-final class |
| --- | --- |
| Dolt/MySQL serialization conflict | `serialization` |
| Metadata-CAS revision move | `cas` |
| Other retried error, such as catalog failure or SQLite busy | `transient` |

The exact allowed values remain these three. No new recovery/mixed token,
label, outcome or metric is introduced. Sums by operation or outcome are
unchanged; serialization-only recovery queries lose the misclassified
points. Historical points are not backfilled. Retry/write behavior and
warning policy stay unchanged.

The authoritative contract covers the helper and four retry loops in
`internal/beads`, class-pinned RED rows R1-R4, guards G1-G7 plus G3b, and
removal of the test helper's empty-class wildcard. At most one explanatory
doc sentence changes in `internal/telemetry`; its behavior is unchanged.
The builder re-locates the sites on landed main and follows B0-B6 rather
than improvising if the predecessor changes their shape.

## Dependency graph and ownership

| Record | Purpose | Route or owner |
| --- | --- | --- |
| `ga-0x5jed28` | Approved recovered-final class contract | Closed design deliverable; PM accepted |
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

The architecture gate is satisfied. A fresh fetch at PM acceptance still
shows main `77143ecd18ff` without `recordWriteBudget`; the landing record and
deploy remain open. Keep their dependencies and retain the closed design
edge as trace. After verified landing, PM re-reads the source, main, contract
and prerequisite outcomes, and routes this SAME P4 bead
`needs-tests` to validator. Validator hands the same bead to builder. There is
no second implementation bead, stacked `base_ref`, predecessor-head edit or
premature machinery admission.

## Acceptance

- Recovered-final class follows the last retried conflict, with exactly
  `serialization`, `cas` or `transient`; mixed histories use the last conflict.
  Exhausted writes retain final-error classification.
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

Publish this accepted-contract update and keep PM's claim released. The source
stays open with `needs-pm` behind the remaining landing prerequisites, whose
verification mayor owns. Approval does not authorize a premature validator or
builder admission. This plan records a future fix and claims no implementation
or landing. Tracker import was a skip-safe no-op because neither supported
importer exists.
