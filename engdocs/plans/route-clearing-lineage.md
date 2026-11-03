# Route-clearing lineage: retain the capability and finish its existing repair

Decision for ga-jkw3wo, 2026-11-03: revive PR #6217 through existing repair
carrier ga-l69b9m, then run the identity-hardening scope against the feature
that actually lands on main. A reviewed successor PR is an acceptable delivery
vehicle. Preserve the existing architecture rulings and operator review gates.

## Evidence and user impact

Stale executor stamps can direct subsequent work toward a previous executor's
session or worktree. The existing review reproduced that failure through the
real route-recovery lane using conditional writes, so the desired behavior is
still useful. No verified main landing or replacement establishes supersession.

- Main inspected: `76843b86452109d9e7d150958cf220da81b9d01d`; neither
  `internal/beads/route_clear_store.go` nor `clearIfGenuine` exists there.
- [PR #6217](https://github.com/gastownhall/gascity/pull/6217) is open and
  conflicting at `9cabffe0d1f17e449de3b5afa450768a32032116`. Its old deploy
  bead ga-2tmd48 is closed as superseded by ga-fpilow.
- ga-fpilow's source branch is `builder/ga-fpilow`, last recorded at
  `a1bdbb039cfb33cebc371d12e7fc04a62ad60bc4`. Its existing review/rework
  carrier ga-l69b9m is open, `ready-to-build`, routed to gascity/builder.
- ga-l69b9m's review demonstrated that CAS and conditional-write discovery
  bypass the decorator. The September 30 ruling requires stamp clearing in
  the same conditional write, with tests through the real recovery lane.
- The PR's operator comments require discussion of the exported wrapping
  contract and correction of hidden optional store capabilities. This plan
  grants no approval of that surface and changes neither requirement.

## Work graph

| Scope | Carrier | Route and completion condition |
|---|---|---|
| Finish the route-clearing feature | Existing ga-l69b9m | Builder, fresh review and MPR; retain existing source history and repair current-main integration |
| Verify actual main delivery | ga-8bst96kg | Unrouted landing guard; PM watches the delivery chain and records the main SHA and source mapping |
| Harden encoded-name restoration | Existing ga-jkw3wo | Native blocks dependency on ga-8bst96kg; PM routes this same scope to validator only after verified landing |

There are two implementation scopes already represented by existing beads.
The new guard records their ordering; it is not another implementation task.
Do not create parallel test/build beads or stack hardening on either old branch.

## Acceptance and handoff requirements

The existing feature carrier retains ga-l69b9m's required changes: genuine
conditional reroutes clear stale executor stamps atomically, same-executor
restores retain them, the conformance check observes conditional route writes,
and real production store composition preserves optional capabilities. If the
conditional interface cannot express the atomic change, use the existing
stop-and-route instruction to architect; PM does not choose a new interface.

Consolidate the separate root-mirror defect into that carrier: a first route of
step B must preserve sibling A's live root stamps; a genuine reroute of A must
still clear the root stamps. ga-eu9495.1's blanket empty-old-target early return
must not be replayed as a separately approved fix: ga-u5okvo's later ruling
requires detecting genuine mismatches in that case. Retire old deploy carrier
ga-irs348 as superseded by this consolidated scope. Retain ga-eu9495 as its
unrouted historical ruling record.

Every review/deploy handoff must carry ga-8bst96kg's landing condition and notify
PM on landing or retirement. A build close, review PASS, branch push, or PR
creation does not clear the guard. Verify the merged PR or successor, its source
mapping, and the shipped owner paths against freshly fetched main. Only then
close the guard, refresh ga-jkw3wo against the shipped code, and route that same
bead for tests and implementation from main. PM keeps no in-progress watch claim.

Preserve the existing ancestry/publication constraints on ga-l69b9m until the
delivery owner reconciles them with the current MPR path. This plan authorizes
no direct main push, self-merge, or bypass of the operator's talk-first requirement.
If publication needs a successor PR, its description must link #6217 and the
reviewed source; close #6217 only once the replacement is concrete and verified.

Hardening follows the architect's existing configured-identity cross-check,
with regression coverage for ambiguous `--` and `__` segments, genuine
mismatches, same-executor pool-slot restores, and nonempty-old-target behavior.
Keep currently valid names valid and document the architect's residual collision
risk rather than claiming lossless recovery. A new identity contract or stronger
guarantee requires architect review.

For both feature scopes, complexity must show zero growth against the pinned
base under the full complexity rule; never refresh the feature's baseline.
Use current Bazel gates through the isolation wrapper, and record named test
execution. The plan itself changes no production code or tests.

## Recovery and verification

Retire ga-s3ftn2 and all seven of its steps, including RED reminder ga-c8m25z,
as abandoned stale build context. They were cooked on main before the target
existed; retiring them is not a claim that RED or GREEN ran. A fresh cycle is
created only after the landing guard clears.

Before releasing PM's claim, commit this exact plan path and verify its scoped
git status is clean. Re-read the native dependency, carrier routes, released
claims, and each sent mail in its system of record. No external tracker was
imported for this bd-native lineage decision.
