# Ready visibility: intended holds and handoff verification

PM disposition for `ga-1luqsjhm`, 2026-11-10: drop the proposed Gas City
readiness fix for the five reported ordinary tasks. Build one pack-level
handoff-verification improvement, `gm-y2a50`, instead. The tasks have
unreleased prerequisites; admission and routing did not establish that a
builder could claim them.

Planning base: `e349bc2703ec750ab14cc5a7d4bc695c2b21dfb7`, freshly synchronized
with `origin/main` before claiming the intake. Work is on the separate
`docs/ga-1luqsjhm-ready-visibility` branch.

## Diagnosis

Every reported task has a `blocks` dependency on a closed bead whose
`gc.work_outcome` is `blocked`:

| Reported task | Holding prerequisite | Prerequisite status | Work outcome |
| --- | --- | --- | --- |
| `be-g7q3id` | `be-52l7s9` | closed | blocked |
| `be-nqtwx7` | `be-vgb5sd` | closed | blocked |
| `be-lnp8rj.1` | `be-5d1aly` | closed | blocked |
| `be-lnp8rj.2` | `be-5d1aly` | closed | blocked |
| `be-83c7g3` | `be-f18qoa` | closed | blocked |

All four prerequisites lack `gc.step_ref` and a passed control-step outcome.
Their close reasons distinguish local build/review or public reply completion
from shipment. This triage does not independently decide whether any later
landing should change their records.

The existing rule is explicit in
[DependencySatisfied and ReadinessWorkOutcome](../../internal/beads/beads.go):
an ordinary closed bead with blocked work outcome does not satisfy its
dependents. A passed formula step has a narrowly defined exception; none of
these prerequisites has that shape.
[BdStore.filterReadyByWorkOutcome](../../internal/beads/bdstore.go) documents
the difference from raw `bd ready`, which considers the closed status alone.
The change entered history as `1928ef64cec` (#6100). The
[cross-store reader](../../internal/readywork/crossstore.go) preserves the
same outcome requirement on a split city.

Fresh API-backed and `GC_NO_API=1` invocations of:

```sh
gc ready --metadata-field gc.routed_to=beads/builder \
  --unassigned --limit 0 --json
```

each returned five other rows and neither `be-g7q3id` nor `be-nqtwx7`.
The mayor's recorded full-reader comparison supplies the five-task population;
direct live reads of those tasks and prerequisites establish the table above.
Original evidence remains at
`/home/jaword/mayor-claude/state/gc-ready-gap-20261110/`.

Claim residue, molecule metadata, open `discovered-from` links, and open epic
membership are unnecessary explanations for these five omissions. The three
PM records also carry `parent-record`, so a target work query excluding that
label has an additional reason to leave them out. The 161 graph-shaped rows
in the broader comparison were not investigated and are not declared correct
or defective by this decision.

Existing regressions already express the intended rule:
`TestCachingStoreReadyDefersToAPassedStep`,
`TestReadHoldsARowUntilItsBlockerReleasesInEveryStore`, and
`TestInProgressHoldsAClaimWhoseBlockerIsClosedBlockedInEveryCopy`. This
planning task changes no implementation or tests.

## Work package

`gm-y2a50` is a P2 HQ task with `needs-pack-work`, routed to the city-scoped
`pack-author.pack-author`. The direct route is the documented singleton
handoff path, with no sling behind it. Its cross-store `discovered-from`
reference to `ga-1luqsjhm` records provenance and imposes no blocking gate;
the raw two-ID dependency reader verifies the edge.

The pack task owns PM admission/resume receipt guidance and a bounded
read-only visibility check at the lowest appropriate pack layer. Acceptance:

1. Report claimability only after the exact work bead appears under the
   target's real eligible-work predicates and canonical reader. An already
   claimed bead requires an independent current status/holder read before
   reporting it held by the target.
2. Record admission, routing, claimability and target-held state separately.
   For absent work, name holding prerequisites using both status and work
   outcome, or report an unknown cause. A failed query cannot become an
   empty-queue success. The pack consumes readiness rather than reproducing
   its passed-step exception or dependency predicate.
3. A visibility miss authorizes diagnosis and an accurate receipt. It does
   not authorize changing prerequisites, outcomes, exact bases, workflows,
   publication limits or local-only restrictions, claiming shipment or a
   wake, retrying admission, or executing held work.
4. Hermetic pack fixtures cover admitted/routed-but-absent work, actual
   claimability, a target claim during verification, query failure and
   nonblocking provenance/parent links. Assert the query, receipt and absence
   of unauthorized dispatch or mutation. Preserve existing handoff and
   budget tests and run the owning `gc-management` gates.
5. Audit the affected PM receipt sites and show how the two builder examples
   would be reported after the change. Historical admission remains a fact;
   its meaning is corrected rather than erased.

Related work stays separate: `gm-owmpq` aligns the builder-to-reviewer READY
query; `gm-kof3ol` owns hold continuity through landing; `ga-yd2i8qx` owns
deacon eligibility before dispatch. The new task's same-HQ related edges
identify the first two without making them prerequisites.

## Delivery boundaries

The reported five source tasks and their prerequisites remain unchanged.
The source builder tasks are subject to the existing machinery budget and
their source-specific restrictions. This intake performs no source-task
sling, build, release, live-state repair or upstream write. Actual prerequisite
repair or landing recognition belongs to the current authorized owner.

The plan is the Gas City artifact; the resulting pack change belongs in a
focused `packs/**` MR from the pack-author lane. No Gas City implementation
child, validator scope or new readiness surface is needed. The PM intake
closes after this artifact is committed and verified, rather than retaining
a claim while the pack work proceeds.

Tracker import is a no-op: this session has no tracker-to-beads importer or
sibling tracker skill. Repository verification follows the
[existing gates](../../AGENTS.md); the pack task has its own repository gates.
