# Plan: aging contributor PR review queue

> **Root:** `ga-efgric`  
> **Decision date:** 2026-09-09  
> **Status:** PM decision complete; architecture boundary pending in `ga-7yfrca`

## Outcome

Do not mass-sling 28 pull requests, and do not add an overlapping
`pr-audit` predicate. The maintainer PR review (MPR) queue already enumerates
this population directly from GitHub. The work that remains is to make its
current-head state visible and to define an overdue tier that cannot starve
behind the general queue.

The review order should continue to put explicit review requests and active
contributor discussions first. For truly overdue work, use the oldest activity
band first and review-surface size only as a tie-breaker. Smallest-diff-first as
the primary key would improve short-term throughput at the cost of letting a
larger contribution wait indefinitely.

## Corrected live baseline

The original measurement used an empty GitHub `reviewDecision` as “never
reviewed” and compared that set with `order-queue-state.json` as though the file
were a pending queue. Neither interpretation matches the live system:

- MPR publishes review comments and records current-head outcomes even when
  GitHub leaves `reviewDecision` empty. PR #5357 is the concrete example: GitHub
  reports no `latestReviews` and an empty `reviewDecision`, while MPR records a
  processed `auto-merge` outcome and the PR contains the published MPR review
  comment.
- `order-queue-state.json` records heads MPR has already processed or attempted.
  Pending candidates are derived from GitHub on each order run; absence from the
  state file means unseen/current-head-due, not undiscovered.
- The side-effect-free queue check reported live work due and selected PR #6201
  at the time of this analysis.

Re-running MPR's own candidate and due logic over the 28 PRs from `ga-efgric`
produced this result:

| State | Count | Meaning |
|---|---:|---|
| Visible to MPR candidate enumeration | 28 | No discovery gap for this sample |
| Already processed at the current head | 8 | Not “never reviewed,” despite blank GitHub `reviewDecision` |
| Due at the current head | 20 | Existing MPR work; no manual review bead required |
| Due with active contributor discussion | 19 | Already promoted to due ranks 2–35 |
| Due without a higher-priority signal | 1 | PR #5365, due rank 117 |

PR #5365 is the useful edge case. It is open, non-draft, mergeable, `CLEAN`, and
green; its review surface is 2 files and 208 changed lines. It is visible and
due, but its lack of an explicit review request or recent discussion leaves it
deep in the general tier. That is a fairness/visibility gap, not a discovery
gap.

## Product decisions

1. **The 28-item count is not an acceptable backlog metric.** It mixes completed
   MPR work with unseen work because GitHub's aggregate review field does not
   represent the MPR lifecycle.
2. **Do not dispatch a parallel manual campaign.** Twenty current heads are
   already due in the autonomous queue, and 19 are near its front. Parallel
   review beads would duplicate work and create races with the queue.
3. **Keep review ownership in MPR.** `pr-audit` should continue to report the
   exceptional conditions it owns; it should not independently enumerate and
   dispatch MPR's ordinary review population.
4. **Preserve human-waiting priority.** Explicit review requests, active
   contributor discussions, and already-gated deploy PRs remain ahead of a new
   overdue tier.
5. **Make the overdue tier starvation-safe.** Eligibility and current-head
   completion must come from the canonical MPR contract. Within that tier,
   oldest activity band is primary and smaller review surface is a tie-breaker.
6. **Expose the live queue.** Operators need a side-effect-free report of
   candidate, due, processed/dispositioned, held/retry, excluded, and overdue
   counts, plus each PR's age, due rank, priority reason, and review surface.

## Work package

| ID | User outcome | Routing | Dependency |
|---|---|---|---|
| `ga-7yfrca` | Maintainers have one authoritative reviewed/due/overdue contract and a starvation-safe priority/reporting design | `needs-architecture` → `gascity/architect` | discovered from `ga-efgric` |

Only the architecture bead is created now. The owning implementation lives in
the city-management pack rather than this Gas City source tree, and its state
contract spans GitHub, MPR artifacts, and queue state. Creating validator and
builder beads before that boundary is settled would pre-decide repository
ownership and acceptance details that belong to the architect. When
`ga-7yfrca` returns with `source:actual-architect`, PM will decompose its design
into the required test and build beads in the store it identifies.

## Acceptance for this PM round

- [x] The 28 sampled PRs were checked against MPR's live candidate enumeration.
- [x] Current-head processed and due states were separated.
- [x] The false equivalence between blank GitHub `reviewDecision` and “never
      reviewed” was rejected with a live example.
- [x] The false interpretation of the queue state file as a pending manifest was
      rejected against the source contract and side-effect-free due check.
- [x] Mass manual dispatch was rejected because it would duplicate active MPR
      work.
- [x] The sequencing decision is explicit: human-waiting lanes first; overdue
      work oldest-band-first; size only breaks ties.
- [x] The unresolved ownership/state design is isolated in `ga-7yfrca` with
      measurable acceptance criteria.

## Risks

- A report based only on GitHub review fields will continue to overcount work
  that MPR already dispositioned.
- A report based only on queue state will undercount unseen due work because the
  queue is derived live from GitHub.
- A smallest-diff-first primary order can permanently defer large contributions.
- Adding the same predicate to `pr-audit` and MPR creates two dispatchers for one
  PR population and makes duplicate review races likely.
