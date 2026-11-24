# Codex usage completeness

PM decision: pursue this at P3, beginning with a controlled reproduction.
Operators need recorded invocation counts and tokens they can reconcile with
Codex transcripts before using formula costs to compare work. The observed gap
justifies investigation, but it does not establish which mechanism caused it.

Root: `ga-1ppuzht5`. Source: `ga-ayaq0w8`. Public context:
[issue #8527](https://github.com/gastownhall/gascity/issues/8527), particularly
its bounded Codex usage-reader case. PM inspected main at
`9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c` on 2026-11-24.

## Evidence and its limits

The architect compared three 72-hour rollout windows with a usage-sink
snapshot. An invocation is a distinct cumulative `total_tokens` value within
one rollout, after excluding zero-usage events. The supplied analysis matched
facts by the millisecond timestamp of one of that invocation's events.

| Window ends before the sink snapshot | All-rollout completeness | Completeness among rollouts with a fact |
| --- | --- | --- |
| 2 hours | 73.6% | 79.1% |
| 12 hours | 75.9% | 79.7% |
| 32 hours | 77.0% | 81.1% |

The first window contains 9,439 default-home invocations, 7,046 matched facts,
and another 129 invocations in 25 mayor-home rollouts with no matched fact.
The lower endpoint includes rollouts not proven to belong to gc; the upper
endpoint omits a session that lost every fact. These are brackets from the
source investigation, not a verified percentage of lost gc-managed usage.
Older windows show a similar gap, making collection delay alone an inadequate
explanation. The controlled study must establish a session-bound denominator
and check timestamp ambiguity before claiming a measured loss rate.

Current source confirms that `SessionLogAdapter.CodexTailUsage` accepts and
discards its cursor, and that Codex extraction uses a fixed 64 KiB tail.
`usagesAfterCursor` retains only the newest entry when its cursor is absent
from the returned window. The comments on `SweepSessionModelUsage` explicitly
describe Codex loss and require a separate correctness argument before scan
growth. These facts establish plausible mechanisms; they do not identify the
cause of each missing fact in the measured population.

The original measurement script is temporary:
`/var/tmp/ga-ayaq0w8-architect-evidence/scripts/codex-fact-completeness-bracket.py`.
Downstream work must retain sanitized, reproducible evidence beyond that
temporary directory. This plan preserves the supplied numbers and limitations,
and does not claim PM repeated the fleet measurement.

Issue #8527 already covers these paths. Its prior triage bead `ga-jgb101k`
closed because the fleet reportedly used Claude rather than Codex at the time;
the new measured Codex workload changes that product-impact premise. No
duplicate issue or external comment was created. PM found no installed
tracker-to-beads or sibling tracker skill, so tracker import was a no-op.

## Work packages and order

All three work packages live in the gascity store, carry
`source:actual-pm` and `plan:codex-usage-completeness`, and trace to the root
through `discovered-from`. The root's completion records PM planning only.

| Bead | Deliverable | Intake / target | Same-store blockers |
| --- | --- | --- | --- |
| `ga-77fu5xfs` | Controlled, identity-bound evidence identifying the loss mechanism | `needs-architecture` / architect | None |
| `ga-9w924f1h` | Reviewed recovery contract with a correctness argument and finite resource bounds | `needs-architecture` / architect | `ga-77fu5xfs` |
| `ga-8l1qtl82` | Regression tests and implementation of the approved capture contract | `needs-tests` / validator, then the same bead to builder | Both preceding beads |

The evidence and design are separate checkpoints: a source-code hypothesis
cannot authorize an implementation. If reproduction fails or the approved
design needs wider scope, return to PM with the evidence and keep downstream
work gated. Do not close an unresolved prerequisite with an outcome implying
successful completion.

The first bead compares known eligible invocations and facts across first and
subsequent reads, including a collection gap larger than 64 KiB. It distinguishes
tail loss, cursor fallback and discovery failure, classifies custom-home files
as managed, external or unknown, and reports missing and duplicate counts.
An invocation with an empty model still counts as captured for this study.

The second bead selects the recovery design. A cumulative-total cursor and
`total <= cursor` stop condition remain proposals. The architect must assess
compaction/reset, repeated totals, model changes, rollout isolation, collector
overlap, persistence failure, first-read history policy and bounded I/O before
approving them. The design specifies what an incomplete bounded read reports;
it cannot silently certify complete capture.

The third bead contains the full test-first scope and a `Build:` contract in
its notes. The validator authors the approved regression cases, proves RED
with Bazel, then relabels and slings that same bead to the builder. There is
no separate build bead for this scope. Its acceptance criteria require:

- Exactly one deduplicated fact per eligible retained invocation within the
  approved recovery bound, including first collection and later backlogs
  larger than the old tail, through both collection seams.
- Original timestamps and per-invocation tokens, with expected/captured
  counts equal and missing/duplicate counts zero in the controlled window.
  Exclusions and beyond-bound incomplete results are reported separately.
- Stable deduplicated counts and token totals across repeated events,
  overlapping collectors and two recovery/restart passes; failed recording
  cannot advance progress past an invocation that was not recorded.
- Conversation isolation and regression coverage for the approved handling
  of malformed input, compaction/reset and other provider families.

## Scope and risk

This chain does not block acceptance of `ga-ihdclm3`, the deployed formula
attribution feature, pricing work, or the independent empty-model repair
`ga-1e41ov04`. Completeness and pricing are distinct: correctly priced facts
can still omit invocations, and a fact with an unknown price still exists.

Preserve the current fact schema, RunID/idempotency semantics and attribution
contract unless separately authorized architecture explicitly supersedes them.
No new primitive, billing policy, broad historical backfill, other-provider
collection expansion or subagent-collection project is approved here. A
custom-home discovery change requires evidence of a managed session affected
and an explicit in-scope architecture decision.

The main risks are duplicate accounting when recovery widens, losing progress
on sink failures, attributing equal totals to the wrong conversation, and
moving transcript I/O onto an unbounded reconciler path. The design owns their
resolution. PM selects no cursor encoding, scan algorithm or size cap.

P3 work remains subject to normal admission and machinery budgets. A successful
sling establishes admission and routing history; a separate target-query
visibility check establishes claimability. Do not retry refused admission,
remove blockers or bypass budgets because a routed child is not visible.

## Validation and shipment

The implementation's gates are the affected sessionlog/worker Bazel targets,
named PASS evidence for filtered runs, `make check`, and the matching
integration tier when lifecycle/sweep behavior changes. Follow `TESTING.md`
resource rules. Run `make bazel-sync` when imports, files or packages change,
verify active hooks, and update the owning `AGENTS.md` for changed invariants.

Complexity must show zero growth against the pinned base using the full rule
in `packs/actual/all/template-fragments/complexity-zero-growth.md.tmpl` in the
city checkout. A green base requires a green merge check; an already-red base
requires base/merge evidence with no worsening rows and a measured
`COMPLEXITY_BASE_REF=<base> make complexity-diff` with no growth. The feature
does not edit `engdocs/complexity-baseline.json`.

Carry the controlled reconciliation recipe through normal review and deploy.
After verified landing, deployer/mayor owns the live check once the running gc
contains the fix. This plan authorizes no fleet binary build, installation or
restart. Preserve implementation/review/landing distinctions in the receipt;
do not retain an implementation claim solely to watch a deployment chain.

PM authors only this plan and the bead requirements. Handoff results,
claimability timestamps and verification evidence belong in the root notes
after their live readbacks.
