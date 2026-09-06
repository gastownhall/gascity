# Proposal: Getting Gas City to 10K Changes/Day

**Gas City Engineering White Paper — Discussion Draft**  
**August 21, 2026**

> **Objective:** Design toward **10,000 agent-generated logical changes per
> day** without verification compute growing as change volume multiplied by a
> fresh full CI run, or routine maintainer attention growing with each change.
> The number is a directional design horizon and stress case, not a traffic
> forecast or universal release gate.

> **Thesis:** Admission should scale with the uncertainty a change introduces,
> not the number of changes produced. Durable work identity, integration,
> proof selection, and publication must remain separate so the system can
> batch, reuse, and recover without losing safety or accountability.

> **Operating principle:** _Agents make changes. The system establishes facts.
> Policy decides what ships. Humans govern policy and handle exceptions._

---

## Executive summary

Gas City is approaching a constraint that will become existential for every
agent-heavy software factory: code can be produced faster than it can be
admitted safely into the canonical branch. A pull request becomes green,
another change lands, `main` moves, and work that was already proved safe is
run again. As the arrival rate rises, the CI queue grows faster than the useful
work it protects.

Ten thousand logical changes per day is about **417 per hour, 6.94 per minute,
or one every 8.6 seconds**. If every change buys a fresh full CI run, even a
five-minute verification cycle requires roughly 35 concurrent validations to
keep up. A 20-minute cycle requires about 139. Those are zero-headroom floors,
before failures, flakes, retries, bursts, or queue churn; they are not safe
capacity targets.

The 10,000/day horizon is directional, not a hard operating requirement or a
forecast of the current factory's traffic. It forces the design to confront
four future shapes: one repository receiving most of the volume, work spread
across a fleet, bursts against a shared core, and coordinated changes spanning
repositories or services.

Runner capacity can keep up with a raw arrival rate, but capacity alone would
buy a fresh proof for every change and violate the objective. Gas City must
attack the problem at three points:

1. **Shape smaller changes.** Direct agents to make the smallest coherent
   change with the narrowest dependency and test blast radius. Split unrelated
   work before it reaches CI. Prefer independently hardened components, whether
   they live in one repository, several repositories, or separate services.
2. **Integrate without chasing `main`.** Speculatively test future integration
   states, batch compatible logical changes into fewer publication
   transactions, and isolate failures without serial maintainer intervention.
3. **Verify only what changed.** Compute affectedness from trusted dependency
   information, run the smallest truthful proof set, and reuse an exact result
   only when every input to that verifier is unchanged.

The order matters. A perfectly incremental CI system still wastes work when an
agent mixes five concerns into one change. A disciplined stream of small
changes still stalls when each one waits for `main` and consumes a separate PR
merge. Gas City needs all three controls.

The practical path starts with what the repository already has: the current
GitHub Actions/Blacksmith pipeline, path-sensitive suite selection, conservative
full-suite barriers, affected-package static analysis, timing data, sharding,
and the `CI / required` summary. Add change-shaping guidance to agent prompts,
measure the real queue, exercise GitHub's merge queue on `merge_group` states,
and harden the existing affectedness model. Pilot Pants, Bazel, Zuul, Refinery,
or a shared REAPI cache only where measurements expose a gap they can close.

If the experiments establish credible headroom toward that horizon within the
registered safety and cost bounds, stop. This proposal does not design a
general-purpose verification graph, a new forge, or a new build system.

## 1. Define the design horizon precisely

A **logical change** is one independently attributable behavior change that can
be selectively reversed or repaired subject to its dependency closure. It may
come from a human, one coding agent, a swarm, a codemod, a dependency bot, or an
optimizer. It does not have to map one-to-one to a commit or pull request, but
the system must retain its identity after batching.

Four units matter:

| Unit | Meaning |
| --- | --- |
| Parent outcome | The user-visible result requested by the originating Bead or formula run |
| Logical change | A publishable Bead, or dependency-closed Bead subgraph, that carries one attributable behavior change |
| Integration candidate | One or more logical changes assembled into a source state for verification |
| Publication transaction | The Git push or PR merge that makes accepted changes canonical |

The throughput metric counts **accepted logical changes**, not generated diffs.
Splitting one edit into ten commits does not create ten units of value. A useful
logical change has a stable identity, an owner, an intended outcome, dependency
relationships, verification results, and a path to individual rollback or
forward repair. Report completed parent outcomes beside logical-change
throughput so decomposition cannot manufacture apparent value.

Beads already provides the durable work identity. The originating Bead or
formula-run root identifies the parent outcome; a designated publishable Bead
or dependency-closed Bead subgraph identifies each logical change. The
publication record maps those IDs to their source commits or patch, declared
dependencies, proofs, candidate and batch membership, and final canonical SHA.
No new work ledger is required.

GitHub currently recommends no more than one merged PR per minute and six
pushes per minute per repository. A repository receiving the entire target
would need 6.94 logical changes per merged PR to stay near the first limit, or
1.16 logical changes per push to stay near the second. This proposal uses
reviewed PR publication as the primary pressure model. [1]

This makes batching a design requirement for a sufficiently hot repository,
not for every fleet topology. The registered compression target is at least
ten logical changes per publication transaction in the hot-repository shape —
the 6.94 floor plus thirty percent forge headroom
([`0001-bounds.toml`](0001-bounds.toml)). GitHub Merge Queue grouping is publication
scheduling: it does not create pre-PR logical batches or combine the underlying
builds. A Gas City batch is a transport unit and must not erase the identity,
dependency order, audit trail, or selective repair path of the logical changes
inside it.

### 1.1 The workload model

The number 10,000 is meaningless without the shape of those changes. The load
test and capacity model must include:

- logical changes per minute, including burst periods;
- changed components and the size of their transitive dependency closures;
- shared-core changes versus component-local changes;
- conflicts and explicit dependencies between concurrent changes;
- test, review, and integration failure rates;
- flakes and infrastructure failures;
- single-repository, cross-repository, and cross-service changes;
- publication batch size and failure-contamination rate;
- offered load and sustainable service capacity for each constrained resource;
  and
- queue depth, oldest-ready age, tail latency, and post-burst drain time.

Recent Gas City history should calibrate change shape, proof cost, failure rate,
and human work. It does not set the target volume. The burst profile and
workload-shape mix are registered values: the initial registration uses the
measured arrival distribution and a 55/20/15/10
hot-repository/fleet/shared-core/cross-repository mix. Synthetic cases then
exercise the four future workload shapes and stress the tails: a burst against
one shared package, many independent component changes, a cache outage, a
failing change at the front of the queue, and a coordinated change spanning
Gas City, T3 Code, and the beads backend.

Constrained resources split into two classes. **Measured resources** are owned
infrastructure — candidate construction, queues, runners, caches, the beads
work ledger, and the event bus — whose sustainable service curves Phase 0
establishes through baseline telemetry and controlled saturation tests.
**Modeled resources** cannot be saturation-tested — the forge and its API,
publication, routine review, and human exception handling — so each carries a
registered capacity model naming its evidence basis, the largest canary that
validated it, and an extrapolation factor capped at twenty times that canary.
For every resource, offered load must remain below measured or model-predicted
sustainable capacity with positive headroom.

Every registered bound, budget, assumption, and ratchet lives in the checked
ledger [`specs/proposals/0001-bounds.toml`](0001-bounds.toml). A bound is
registered for a phase run only when the commit recording it predates that
run; the ledger's git history is the audit trail, and a guard test in
`test/docsync` keeps the ledger and this document in sync. Because the
10,000/day figure is directional, bounds are ratchets anchored to measured
baselines — re-derived at each sustained doubling of accepted-change volume —
rather than fixed horizon values.

## 2. Start from Gas City's current system

Gas City is not starting with a serial, full-repository CI script. The current
workflow already has:

- path-sensitive jobs for mail, beads, packs, worker, provider, and integration
  coverage;
- a conservative `shared` barrier that forces the union of gated suites for
  cross-cutting paths;
- changed-package lint and formatting checks where the scope can be proved;
- explicit full-repository fallbacks where it cannot;
- sharded unit, process, and integration runners;
- Blacksmith runner selection and dependency caches; and
- a stable `CI / required` summary suitable for branch protection. [2]

The normative target in [`TESTING.md`](../../TESTING.md) is p95 protected PR
feedback under five minutes, including queueing. The current testing-efficiency
program has demonstrated one exact-`main` run at 4m59s, but the repository does
not yet have authoritative workflow telemetry proving that result at p95. The
timing planner is still measurement infrastructure rather than the authority
that selects the required graph. [3][4]

The current workflow does not listen for GitHub's `merge_group` event. That
means it cannot yet provide required checks for GitHub Merge Queue's predicted
future states. [2][5]

These facts change the near-term plan. The first job is to instrument and
finish the current system, not replace it. Any new scheduler, dependency engine,
or cache must beat that baseline on admission throughput, compute, or human
intervention while preserving its safety properties.

## 3. Shape changes before they enter CI

The cheapest verification is the work a well-shaped change never invalidates.
Gas City's formulas and agent prompts should ask for a narrow change before an
agent edits code. This is behavioral guidance supplied by the pack, not judgment
hardcoded into Go.

A useful default instruction is:

```text
Before editing:
- identify the smallest coherent behavior change that satisfies the assignment;
- identify the component that owns it and the interfaces it can affect;
- keep cleanup, refactoring, generated updates, and unrelated fixes separate;
- avoid shared-core changes when a component-local change is truthful;
- split work that can be verified and reverted independently.

Before handoff:
- list the logical change IDs and their dependencies;
- name the changed components and public contracts;
- name the smallest tests that own the changed behavior;
- call out any shared or cross-repository blast radius.
```

This declaration helps reviewers and the planner, but it is not trusted input
to admission. CI computes affectedness from the source tree, dependency data,
and checked policy. If the agent claims a component-local change but the diff
touches a shared dependency, the shared dependency wins.

### 3.1 Optimize blast radius, not line count

A five-line change to `go.mod`, shared configuration, or a central interface may
invalidate more work than a 500-line component-local implementation. Change
shaping should minimize the **effective invalidation surface**: the set of
components, contracts, and proofs that can be affected.

That gives the system a measurable feedback loop. For each logical change,
record:

- files and components touched;
- direct and transitive dependents;
- required proof set;
- whether the work could have been split into independent changes; and
- whether a shared-core edit was necessary.

The goal is not to punish broad changes. Some changes are inherently broad.
The goal is to stop creating broad changes accidentally.

### 3.2 Make component boundaries earn their independence

A directory, repository, or service is not independently hardenable merely
because it has a name. It needs an explicit contract, a known dependency edge,
and a focused proof that protects the boundary.

Gas City's current path groups and test owners should seed component names and
the proof inventory, not define dependency truth. Authoritative package and
build graphs plus explicit contracts define dependency edges; `shared` remains
the fail-closed answer when the graph cannot prove a narrower scope. Tighten
those boundaries where measurement shows that unrelated changes still force a
full union. When two components can be changed, tested, deployed, and repaired
independently, the scheduler can place them in parallel lanes. When a change
crosses the contract between them, it becomes one coordinated integration
candidate.

This model works at every scale:

```text
package → component → repository → service
```

The affectedness planner follows real dependencies across those boundaries. It
does not assume that a repository boundary makes two changes independent.

## 4. Integrate without serializing on `main`

The moving-main loop is proof churn:

```text
B passes against main@100
        ↓
A merges → main@101
        ↓
B is restaged and proved again
```

GitHub Merge Queue already constructs temporary `merge_group` branches from the
current base plus changes ahead in the queue. It can dispatch up to 100
concurrent builds, and it automatically reconstructs downstream states when an
earlier change fails. Its merge limits can group publication, although those
limits do not combine the underlying `merge_group` builds. [5]

That makes GitHub's queue the first scheduler to test. The pilot should:

1. inventory the live required contexts and ruleset that govern admission;
2. add `merge_group` handling to every required workflow, including CodeQL;
3. make changed-file and base-SHA logic work for both PR and merge-group events;
4. report the same stable `CI / required` check on predicted integration states;
5. migrate the ruleset through a canary with an operator rollback path; and
6. compare interventions, duplicate work, and time-to-merge against the current
   flow.

If GitHub's queue keeps the admission queue bounded, use it. Bring in Zuul or
extend Gas Town's Refinery only if measurements show that GitHub's state-level
rebuilds, queue policy, batching, or cross-repository coordination are the
remaining bottleneck. [10]

This pilot can prove whether predicted integration states reduce rebase churn.
It cannot prove logical batching, cross-repository atomicity, or incremental
proof reuse; those are separate experiments.

### 4.1 Batch logical changes without losing them

Batching is how 10,000 logical changes fit through a much smaller number of
publication transactions. A practical batcher should:

- retain the identity and dependency order of every logical change;
- group changes with low overlap or an intentional dependency relationship;
- build and test the combined tip or required union once where safe;
- publish the whole batch when it passes; and
- split or bisect a failing batch until the minimal failing subset is isolated,
  without violating dependency closure.

Gas Town's Refinery design already describes Bors-style batch-then-bisect
behavior. That is a plausible strategic home for Gas City-aware batching once
the simpler queue pilot establishes what GitHub does and does not solve. [6]

Independent component lanes matter as much as batch size. A failure in one
component should not block unrelated changes elsewhere. Shared-core changes
enter the broader lane whose dependents they invalidate.

The publication commit structure is part of the identity contract, and it is
registered rather than assumed. A single-change publication may squash: one
logical change, one canonical commit, identity trivially preserved. A
multi-change batch publishes as a merge commit whose first-parent chain
carries one rebased commit per logical change in dependency order, so every
logical change ID maps to exactly one canonical SHA and `git revert` of one
mid-batch commit is the default repair path. Batches are never squashed:
squashing collapses the batch to a single commit and degrades selective
repair to patch reconstruction. The repository ruleset already permits both
merge methods, so this is publication policy, not new forge machinery.

Selective revert also has an honest limit, and the batcher enforces it
rather than papering over it: changes that touch the same hunks share a
batch only when they are dependency-related, and a dependency-closed
subgraph reverts as a unit or not at all. Where a clean revert is impossible,
forward repair from the recorded patch is the fallback, and the publication
record states which repair path each logical change actually has instead of
promising one it does not.

### 4.2 Keep humans off routine happy paths

Ten thousand changes per day cannot require ten thousand human approvals. For a
bounded change class with trustworthy deterministic proofs, policy should admit
the routine cases and send exceptions to a person.

Start with one narrow class whose risks and required proofs are well understood,
after affectedness and merge behavior have passed their own audits. A versioned,
declarative policy names the admissible scope, provenance, dependency closure,
and required proofs. The controller evaluates those predicates mechanically; it
does not infer whether a change is safe. Missing or ambiguous evidence, a new
scope, failed proof, unexplained generated output, policy change, or unexpectedly
broad blast radius fails closed into human review. Humans govern the policy,
audit samples, and handle exceptions.

Expand automated admission one audited change class at a time. Track its
escalation rate, escaped-defect rate, rollback rate, and human touches per
accepted change.

The arithmetic makes admission coverage the load-bearing number, so the
design states it instead of implying it. At the registered compression
target, the directional horizon is roughly 1,000 publication transactions
per day; if each crossed a human reviewer at even five to fifteen minutes,
routine review alone would cost 80 to 250 person-hours per day. Routine
review therefore cannot scale with volume, and the plan must say how fast it
retires.

Define **admission coverage** as the fraction of accepted logical changes
admitted by declarative policy without a routine human review. Human work is
then two streams — the reviewed publications of the uncovered fraction, and
the exceptions escalated from automated lanes — and both draw down one
finite, registered daily human-touch budget (routine reviews plus exceptions
plus interventions), ratcheted from the measured capacity of the actual
team. That budget is what forces coverage upward: the registered milestones
require coverage of at least 0.5 before sustained volume passes 500 changes
per day, 0.9 before 2,000, and 0.99 before the 10,000/day shape, with the
exception rate under its registered bound and the stricter auto-admission
miss bound holding throughout. Volume growth that would breach the touch
budget waits for coverage, not the reverse.

Coverage expands class by class. Inventory change classes by provenance,
scope, and proof determinism; order them by volume share times proof
stability, so the classes that buy the most coverage graduate first —
dependency bumps, generated-file refreshes, and codemod output are the
plausible front of that queue, and judgment-heavy shared-core work is the
back. Reviewed lanes are not required to carry the compression target:
small human-reviewed publications and heavily batched automated lanes
coexist, because the forge limit binds the publication stream as a whole,
not each lane — batching optimizes the lanes humans no longer read.

## 5. Run the smallest truthful proof set

The planner's question is:

> Did any input to this verifier change?

For a deterministic package test, those inputs may include source files,
transitive Go dependencies, fixtures, generated files, module metadata, the Go
toolchain, environment, command, and flags. If the planner cannot account for
an input, it must over-invalidate.

This is an extension of Gas City's current policy: one risk, one smallest owning
proof. Narrow production boundaries and focused test ownership make affectedness
more precise. Coarse integration tests and hidden environment dependencies force
larger invalidation sets even when the scheduler is perfect.

### 5.1 Improve the current planner before importing another one

Use the existing path filters, package dependency graph, and `shared`
fail-closed fallback as the first affectedness model. Instrument which gates
they select and compare that selection with full-union audit runs. Replace a
coarse rule only when dependency or contract data can prove a narrower one.

Pants is a candidate for a shadow pilot because it offers Git-aware changed
targets, per-package Go test caching, and REAPI-compatible remote caching. Its
Go backend remains beta and has unsupported edges, so it is an experiment, not
the default architecture. [7][8]

Before the pilot, register the acceptable miss-rate bound, confidence target,
sample size, workload strata, and delayed observation window. The miss-rate
bound is two-tier: one bound for human-reviewed lanes and a stricter bound
plus an ongoing random full-union audit fraction for auto-admitted classes.
Every bound is restated at exposure — expected escapes per day at current
volume — and re-accepted at each sustained volume doubling. Exercise seeded
hidden-dependency cases and retain random full-union runs after rollout so drift
stays visible. Any missed failure broadens the rule and removes it from
admission until it passes again.

The pilot should answer concrete questions:

- Can it model Gas City's generated files, embedded resources, native
  dependencies, process-backed tests, and custom integration shards?
- Does it select a materially smaller proof set than the current planner?
- Does that reduction survive full-union audit runs with no observed missed
  failures?
- Is the setup and maintenance cost lower than improving the current scripts?

Evaluate Bazel or Buck2 only if the measurements justify a build-system
migration. Do not adopt one merely because its dependency model is elegant. [9]

### 5.2 Reuse exact work, with a trust boundary

Cache in layers:

1. Keep runner images, toolchains, module downloads, browser binaries, and other
   setup inputs warm.
2. Preserve Go build/test and static-analysis caches under exact tool and input
   keys.
3. Add a shared action cache only after the client can identify hermetic actions
   precisely enough for cross-runner reuse.

Coalesce in-flight misses too. When several candidates request the same exact
action key, run it once and attach every waiting candidate to that result.

`bazel-remote` is a reasonable cache-only REAPI pilot. Buildbarn becomes
interesting if distributed CAS, scheduling, or remote execution is needed.
Neither helps until Pants, Bazel, or another client emits trustworthy action
keys. [11][12]

A shared cache is part of the admission trust boundary. Authenticating a
producer does not authorize its results for every consumer. Reuse and in-flight
coalescing must be bound to a trust domain, workflow and policy revision,
platform and environment, and the exact action inputs. Protected candidates
must never consume results produced in an untrusted pull-request domain.
Untrusted work should be isolated or read-only. A miss, corrupt entry, or cache
failure causes recomputation, never false success.

Do not reuse time-sensitive or environment-sensitive checks as though they were
pure functions. Security scans, live-service tests, benchmarks, and probabilistic
reviews need explicit freshness and environment policy. When in doubt, run them.

## 6. Measure before choosing the final stack

The experiment is a funnel. Each phase must isolate one source of throughput
loss and has a stop condition. A control enters admission only after its own
safety gate passes. Phase 0 supplies the baseline used to set capacity, tail
latency, compute, human-work, and safety limits; those limits are registered
before the combined test rather than chosen after seeing its results.
Registered means committed to the bounds ledger
([`0001-bounds.toml`](0001-bounds.toml)): a bound counts for a run only when
its commit predates that run, and a guard test keeps the ledger and this
document in sync. Most bounds are ratchets — a baseline measured by the named
phase, a no-regression rule, and re-derivation at each sustained doubling of
accepted-change volume.

### Phase 0: establish identity and the baseline

Define the Beads projection for parent outcomes and logical changes, then carry
those IDs through candidates, proofs, batches, and publication transactions.
Use controlled load to find each measured resource's sustainable service
curve rather than extrapolating from an underloaded production sample; a
modeled resource is validated instead by the largest canary its owner permits,
and its capacity model records that canary and the extrapolation factor.

For every required check and ready logical change, record:

- creation, ready, queue-entry, verification, and publication times;
- parent outcome, logical change, integration candidate, batch, and publication
  transaction IDs;
- base SHA and patch identity for each run, so rerun causes classify
  mechanically: an identical patch on a new base is a base-advance rerun, and
  an identical patch and base that now passes is a flake;
- changed components and predicted proof set;
- suite mode (`filtered` or `full`) and actual jobs executed;
- wall time, compute time, queue time, and setup time;
- cache hits, misses, and bytes transferred;
- pass, failure, flake, and infrastructure-failure classification;
- prior successes discarded solely because the base advanced;
- human interventions (rerun, dequeue, manual rebase, forced merge) counted
  from forge audit records, with a quarterly sampled time study converting
  counts to minutes;
- queue depth, oldest-ready age, and post-burst drain time;
- offered load and sustainable service rate at each constrained resource; and
- merge order, rollback, and broken-`main` outcomes.

Compute:

| Metric | Definition |
| --- | --- |
| Admission throughput | Accepted logical changes / day |
| Outcome throughput | Completed parent outcomes / day |
| Publication compression | Accepted logical changes / publication transactions |
| Proof churn ratio | Verification compute repeated after a base advance / total verification compute |
| Invalidation surface | Proof nodes selected / proof nodes available |
| Verification cost | Full-suite-equivalent (FSE) compute / accepted logical change and completed parent outcome; one FSE is the full-union suite priced at the SHA pinned in the bounds ledger |
| Queue amplification | Integration states validated / accepted logical changes |
| Queue health | Queue depth, oldest-ready age, p95/p99 ready-to-canonical latency, and drain time |
| Capacity headroom | Sustainable service capacity minus offered load at each constrained resource |
| Human serialization cost | Maintainer interventions / accepted logical change; minutes come only from the sampled time study |
| Backlog latency | Creation-to-ready distribution; the hard bound applies to dependency-free changes only |
| External value anchor | User-facing issues closed and shipped release-notes entries per week, reported beside throughput |

The timing work should extend the existing `ga-80po0c.4` telemetry rather than
create a parallel measurement system. [3][4]

Phase 0 also registers the flake policy: a per-job flake-rate assumption held
only until measurement replaces it, the mechanical classification rule above,
and a quarantine rule for repeat offenders. No queue or batching phase begins
before that policy is registered.

### Phase 1: change shaping

Add the small-change guidance to the Gas City development formulas used by
agents. Compare invalidation surface, cross-component edits, proof-set size, and
rollback size with the baseline.

**Stop condition:** keep the guidance only if changes become narrower without
increasing escaped defects, partial fixes, or human decomposition work.

### Phase 2: affectedness shadow and audit

Shadow the current planner, then any Pants or repository-specific alternative,
against full-union runs. Include seeded hidden-dependency cases and retain
random full validation after rollout. Report the registered miss-rate bound,
confidence, sample size, workload strata, and delayed observation window.

**Safety gate:** a missed failure fails the pilot, broadens the rule, and keeps
it out of admission until the revised rule passes. A zero-observation result is
reported with its actual confidence and coverage, not as proof of impossibility.

### Phase 3: merge scheduling

Run GitHub Merge Queue on a bounded stream. Measure base-advance reruns, queue
latency, duplicate compute, and manual intervention. Exercise every required
workflow, the live ruleset, the canary, and operator rollback. The phase runs
under the registered queue-amplification bound, and its measured
ready-to-canonical latency and post-burst drain become the baselines the
admission-latency ratchet holds thereafter.

**Stop condition:** if moving-main churn becomes negligible, do not add Zuul or
build equivalent scheduling machinery in Refinery.

### Phase 4: logical batching

Replay and canary batches in the hot-repository workload. Measure publication
compression, combined-proof reuse, failure contamination, split cost, and time
to isolate the minimal failing dependency-closed subset. Bisection uses the
registered flake policy: a failure is attributed only when it reproduces under
the rerun criterion, which is also what makes the minimal failing subset
well-defined. The failure-contamination bound is registered before the phase
begins, and the first deliberate forge canary — one merged pull request per
minute sustained for sixty minutes — runs at this phase's entry to validate
the forge capacity model.

**Stop condition:** keep batching only where it reduces publication or proof
pressure without losing identity, dependency order, or failure containment.

### Phase 5: bounded automated admission

Choose one routine change class with stable, deterministic proof requirements.
Run its versioned declarative policy in shadow mode while humans continue
reviewing it, then compare decisions, exceptions, escaped defects, and review
effort.

**Stop condition:** automate only if every admission decision is explained by
explicit predicates and proofs. Missing or ambiguous evidence must fail closed;
an exception rate above the registered bound means the class is not ready.

### Phase 6: shared cache

Connect the selected client to a shared cache in an isolated trust domain.
Measure hit rate, transfer cost, compute avoided, poisoning resistance, and
behavior during cache loss.

**Stop condition:** keep the remote cache only if avoided compute and latency
outweigh transfer, storage, operations, and security cost.

### Phase 7: combined load test

Replay representative history plus synthetic versions of the four future
workload shapes through the complete planner and scheduler. Model large-volume
capacity offline and use smaller real GitHub canaries for forge behavior; do
not create 10,000 real pull requests.

Each success criterion is labeled by how it is established. **Measured**
criteria run on the real system or its canary. **Modeled** criteria are
predictions of the registered capacity model, valid only within its
extrapolation cap of the largest validating canary. **Imported** criteria are
earlier phases' evidence restated at target-volume exposure; Phase 7 adds no
proof of its own for them.

The directional 10,000/day scenario succeeds when:

**Measured, on the real system and canaries:**

- p95 protected PR feedback stays within the `TESTING.md` target, and the
  admission-latency SLO — ready-to-canonical p95/p99 over merge-group and
  batch builds — stays within its ratcheted bounds (two SLOs over two distinct
  run populations);
- the canary survives scheduler restart, cache loss, and a failing shared-core
  change within the registered recovery bounds;
- unrelated component lanes keep accepting changes during localized failures;
  and
- every logical change, proof result, batch membership, and publication
  outcome reconciles dual-entry: the count published per the Beads publication
  mapping equals the count derived independently from forge history.

**Modeled, by the registered capacity model within its extrapolation cap:**

- sustained 6.94 accepted logical changes per minute and the registered burst
  profile with positive headroom at every resource, measured or modeled;
- queue depth, oldest-ready age, ready-to-canonical tails, and post-burst
  drain within their registered bounds;
- the registered publication-compression target in the hot-repository shape,
  with the failure-contamination bound holding alongside it; and
- the FSE compute ratchet and the intervention ratchet, with per-change rates
  taken from canary measurement and scaled by the model.

**Imported, restated at exposure:**

- the Phase 2 and Phase 5 affectedness and admission bounds at their stated
  confidence and workload coverage, with expected escapes per day recomputed
  at target volume and re-accepted; and
- a broken-`main` and rollback rate no worse than baseline on the canary, with
  the model's failure assumptions calibrated from the baseline rather than fit
  to the result.

This scenario is evidence about the architecture's future headroom, not a claim
that today's workload needs 10,000 changes or that every deployment must clear
that volume before it can ship. The safety, truthfulness, and bounded-queue
outcomes remain requirements at every operating scale, and the modeled
criteria are claims about the model, disclosed as such.

## 7. The practical architecture

```text
agent formula / prompt
  shapes the smallest coherent logical change
                     │
                     ▼
Beads work graph
  parent outcome + logical change IDs + dependency closure
                     │
                     ▼
candidate builder / batcher
  preserves source, dependency, proof, and publication mappings
                     │
          ┌──────────┴──────────┐
          ▼                     ▼
 component-local lanes    shared/cross-repo lane
          │                     │
          ▼                     ▼
 per-repo Merge Queue     per-repo queues + cross-repo
 predicted states         coordination when required
          │                     │
          └──────────┬──────────┘
                     │
                     ▼
affected proof planner
  path filters + package graph + contracts + fail-closed fallback
                     │
          ┌──────────┴──────────┐
          ▼                     ▼
 trusted cache hit      isolated miss / recompute
 exact inputs + trust domain + workflow/policy revision
          │                     │
          └──────────┬──────────┘
                     ▼
             CI / required
                     │
                     ▼
      declarative admission policy
  admits proved cases; fails closed on missing evidence
                     │
                     ▼
publication transaction
  canonical SHA ↔ logical change IDs and dependency closure
```

GitHub remains the source of record for code and canonical publication. Beads
remains the source of record for work identity and dependencies; the
publication mapping connects the two. GitHub Actions and Blacksmith remain the
execution layer until a measured bottleneck justifies replacing them. Gas City
prompts shape the work; trusted CI code computes affectedness and establishes
the facts used for admission.

## 8. Immediate next steps

1. Define the Beads projection for parent outcomes and logical changes, then
   extend existing telemetry through candidates, proofs, batches, and canonical
   publication.
2. Keep the bounds ledger ([`0001-bounds.toml`](0001-bounds.toml)) current:
   the initial burst basis, shape mix, compression target, ratchets, safety,
   flake, and model values are registered; re-derive them at each sustained
   volume doubling and keep the guard test green.
3. Add small-change and blast-radius guidance to the development formulas used
   by Gas City agents.
4. Build the initial component and dependency model from authoritative package
   and build graphs plus explicit contracts; run affectedness in shadow mode
   with `shared` as the fail-closed fallback.
5. Inventory required contexts and add `merge_group` support to every required
   workflow, then canary the ruleset migration with an operator rollback path.
6. Pilot dependency-aware batching in the hot-repository shape, preserving
   logical identity and isolating minimal failing subsets.
7. Define one bounded change class for declarative admission and run its policy
   in shadow mode with explicit proofs and fail-closed escalation.
8. Run a Pants shadow pilot only if the audited affectedness model remains a
   material source of excess work.
9. Add a shared action-cache pilot only after a selected client can produce
   exact keys and authorization between trust domains is defined.
10. Run the combined directional 10,000/day scenario with each criterion
    labeled measured, modeled, or imported, publish the measurements,
   and choose the smallest stack that keeps queues bounded with positive
   headroom.

## Conclusion

Gas City designs toward the 10,000/day horizon by controlling the entire
admission path, beginning with the shape of the change itself.

Agents should produce the smallest independently useful changes. Component
boundaries should let unrelated work proceed and harden independently. The
merge scheduler should test future states and publish groups of logical changes
without making every PR chase `main`. CI should run the smallest truthful proof
set and reuse exact work behind a clear trust boundary.

The thesis is straightforward:

> **Each new change pays only for the uncertainty it creates. Work identity,
> integration, proof, and publication stay separate so queues and human effort
> remain bounded as volume grows.**

Measure each layer, add machinery only where the measurements require it, and
use the 10,000/day scenario to expose where the design runs out of headroom.

---

## References

1. [GitHub Docs: Repository limits](https://docs.github.com/en/repositories/creating-and-managing-repositories/repository-limits)

2. [Gas City repository: `.github/workflows/ci.yml`](https://github.com/gastownhall/gascity/blob/main/.github/workflows/ci.yml)

3. [Gas City testing policy](../../TESTING.md)

4. [Gas City testing-efficiency operating corpus](../../engdocs/contributors/testing-efficiency-workflow-corpus.md)

5. [GitHub Docs: Managing a merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue)

6. [Gas Town Architecture: Merge Queue — Batch-then-Bisect](https://github.com/gastownhall/gastown/blob/main/docs/design/architecture.md)

7. [Pants Docs: Go overview](https://www.pantsbuild.org/stable/docs/go)

8. [Pants Docs: Remote caching and execution](https://www.pantsbuild.org/stable/docs/using-pants/remote-caching-and-execution)

9. [Bazel Docs: Remote caching](https://bazel.build/remote/caching)

10. [Zuul Docs: Project gating](https://zuul-ci.org/docs/zuul/latest/gating.html)

11. [`bazel-remote`: REAPI-compatible remote cache](https://github.com/buchgr/bazel-remote)

12. [Buildbarn: distributed cache and remote execution](https://github.com/buildbarn)
