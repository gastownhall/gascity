# Testing Efficiency Operating Corpus

- **Status:** Living operational companion
- **Checkpoint:** 2026-07-24, after merged PR
  [#4599](https://github.com/gastownhall/gascity/pull/4599)
- **Program:** `ga-80po0c`
- **Audience:** Coordinators, architects, implementers, and reviewers improving
  Gas City's test architecture

## Purpose and authority

This document captures the operating method used to make Gas City's tests
faster without weakening their ability to catch regressions. It explains how
to find a candidate, choose the truthful test edge, design a bounded slice,
delegate it, review it, merge it, and divide the remaining program among a
team.

It is deliberately not another testing policy:

1. [`TESTING.md`](../../TESTING.md) is the canonical, normative source for
   test design, placement, doubles, conformance, timing, flakes, and resource
   policy. It wins every conflict.
2. The
   [testing pyramid audit and hardening plan](testing-pyramid-hardening-plan.md)
   is the dated audit, rationale, proposed dependency graph, and program
   backlog. Its counts and task status are historical unless a checked source
   says otherwise.
3. This corpus is the operational playbook, evidence catalog, and team
   handoff. It explains how to execute the policy and plan; it does not amend
   either one.

Live checked ledgers, manifests, and tests outrank prose about their current
contents. Do not copy a live ceiling, waiver, owner, or timing status from this
document into a decision without querying its checked source.

## Mission and non-negotiables

The outcome is developer-visible protected PR feedback at p95 under five
minutes while preserving or improving defect detection. The normative rules
for smallest owners, seams, failure edges, asynchronous waits, doubles,
conformance, E2E admission, flakes, and timing are in
[`TESTING.md`](../../TESTING.md). This workflow never overrides them.

Speed is a constraint on architecture, not permission to delete confidence.
A slow test is evidence of a design question: either it owns a real boundary,
or production logic is trapped behind an unnecessarily expensive boundary.

The clean-architecture test is simple: if a domain rule can be proved only by
starting a process, changing the working directory, waiting for a timer, or
opening a database, first look for the missing use-case boundary. The normal
answer is dependency inversion and a smaller proof, followed by one retained
adapter or composition proof.

## Working vocabulary

| Term | Meaning in this program |
|---|---|
| **Risk sentence** | One sentence stating the condition, observable promise, and regression the proof must catch. |
| **Assertion owner** | The smallest test that primarily owns one invariant. A higher test may separately own wiring. |
| **Real-boundary proof** | A focused proof using the real process, protocol, store, filesystem, runtime, browser, or other boundary because that composition is the risk. |
| **Fast substitute** | A conformant in-memory implementation, strict protocol executable, fake clock, scripted collaborator, or similarly deterministic dependency. |
| **Invariant map** | The reviewable mapping from every old assertion to its new primary owner and any retained real-boundary proof. |
| **Slice** | One independently measurable and reviewable architecture change, with one writer and explicit non-goals. |
| **Council** | Three independent delegated reviews of the same exact staged tree before commit. |
| **Ratchet** | Checked policy that prevents known resource or architecture debt from growing and lowers when debt is removed. |

## Where the program started

The 2026-07-13 audit found broad coverage but inverted cost and unclear
ownership. Its point-in-time census reported 1,548 Go test files and 735,331
lines of test Go. `cmd/gc` alone held 318,018 test lines, produced a 293 MB
test binary, used about 4.1 GB to compile, and took 55 seconds merely to compile
in a warm local measurement.

That shape produced five recurring problems:

| Problem | Architectural cause | Correct response |
|---|---|---|
| Long package and shard floors | Domain decisions exercised through the `cmd/gc` monolith | Extract cohesive use cases; do not merely split files or add shards. |
| Slow or flaky asynchronous tests | Sleep and polling substituted for lifecycle signals | Publish or expose completion, readiness, exit, events, barriers, or virtual time. |
| Repeated process/store/provider startup | A real dependency was used for a non-boundary assertion | Move the branch matrix to a conformant substitute and retain one real wiring proof. |
| False confidence from fakes | Reusable substitutes did not share production contracts | Repair contract honesty before deleting broad coverage. |
| Overlapping E2E families | Multiple journeys repeated lower-level behavior | Map assertions, strengthen lower owners, and keep only the unique composition. |

Merged PR [#4193](https://github.com/gastownhall/gascity/pull/4193)
established the immediate latency baseline: its exact-main run completed in
4m59s workflow wall time including queueing and 4m15s from runner-policy start
to `CI / required`. The hardening program exists to make that result
repeatable through better test and production architecture, not one-time CI
topology.

## What has shipped

The following are representative milestones, grouped by the reusable pattern
they established. Timing labels mean:

- **Observed timing:** comparable before/after wall or execution measurements
  on the same environment/profile.
- **Observed work:** causal counts such as command roots, builds, writes, or
  sleeps. These do not by themselves prove a wall-time improvement.
- **Projected:** a timing effect was extrapolated rather than measured on the
  claimed layer.
- **Neutral:** correctness, determinism, or policy improved without a claimed
  timing improvement. This includes measured “no material timing change”
  outcomes.

| Pattern | Representative merged work | Evidence at merge | Durable lesson |
|---|---|---|---|
| Canonical policy | [#4413](https://github.com/gastownhall/gascity/pull/4413) | **Neutral:** made `TESTING.md` authoritative | Policy belongs in one place; operational docs link to it. |
| Direct use-case seams | [#4309](https://github.com/gastownhall/gascity/pull/4309), [#4324](https://github.com/gastownhall/gascity/pull/4324), [#4333](https://github.com/gastownhall/gascity/pull/4333), [#4336](https://github.com/gastownhall/gascity/pull/4336) | **Observed timing:** representative package/body floors fell from 82.28s to 1.94s, 32.78s to immediate, 54.61s to 0.42s, and 29.12s to immediate | Put decisions on direct typed seams; retain a named CLI/store/controller or provider lifecycle proof. |
| Event-driven in-memory watchers | [#4340](https://github.com/gastownhall/gascity/pull/4340) | **Observed timing:** conformance repetitions improved 6.15x and 9.11x | A generation channel can broadcast a fact without a polling fallback. |
| Live request correlation | [#4369](https://github.com/gastownhall/gascity/pull/4369) | **Neutral:** removed 15 sleeps; no demonstrated E2E speedup | Events improve determinism and diagnostics even when end-to-end wall time stays flat. |
| Exact production conformance | [#4326](https://github.com/gastownhall/gascity/pull/4326), [#4403](https://github.com/gastownhall/gascity/pull/4403), [#4404](https://github.com/gastownhall/gascity/pull/4404), [#4407](https://github.com/gastownhall/gascity/pull/4407) | **Observed work; no measurable timing change:** duplicate raw/wrapper runs removed or avoided with negligible runtime change | Run shared conformance once through the actual production constructor; keep raw-only behavior focused. |
| Contract honesty before speed | [#4350](https://github.com/gastownhall/gascity/pull/4350) | **Observed timing:** suite grew 58.63s to 62.80s while archive semantics became executable | A truthful contract may cost time. Quality work is not required to manufacture a speed claim. |
| Strict protocol doubles | [#4344](https://github.com/gastownhall/gascity/pull/4344) | **Observed timing:** Docker CI coverage fell from 218s to 48s while retaining all 35 real-container assertions in one boundary proof | Use a strict executable for argument/protocol branches and one real platform composition. |
| Crisp real-boundary journeys | [#4423](https://github.com/gastownhall/gascity/pull/4423), [#4426](https://github.com/gastownhall/gascity/pull/4426) | **Observed timing:** worktree consistency fell 58.87s to 7.01s; mail testscript fell 54.41s to 9.15s locally and 18.32s to 5.43s on Blacksmith | Preserve the few invariants unique to composition; remove repeated command journeys. |
| Deterministic timing planner | [#4400](https://github.com/gastownhall/gascity/pull/4400) | **Neutral:** added pure p75/p95 planning in dry-run authority | A planner is measurement infrastructure until activation is explicitly checked; do not claim adaptive sharding is live. |
| Static work scoped fail-closed | [#4339](https://github.com/gastownhall/gascity/pull/4339) | **Projected:** about 132s of net static work and 125s of PR-job time | Scope ordinary PR work through checked reverse dependencies while preserving protected full runs. |
| Resource-policy ratchets | [#4571](https://github.com/gastownhall/gascity/pull/4571), [#4573](https://github.com/gastownhall/gascity/pull/4573), [#4599](https://github.com/gastownhall/gascity/pull/4599) | **Neutral:** exact tmux, typed listener, and helper-backed listener ownership became checked with flat scanner timing | Make hidden resource debt visible by exact syntax/import identity; do not claim universal inference. |

The July 20–22 optimization wave repeated the same patterns at smaller edges:

| Merged PR | Change | Evidence at merge |
|---|---|---|
| [#4472](https://github.com/gastownhall/gascity/pull/4472) | Replace wait-list filesystem setup with an in-memory coordination owner | **Observed work:** 2,002 `FileStore` writes replaced by one real wiring proof |
| [#4502](https://github.com/gastownhall/gascity/pull/4502) | Retire duplicate close-limit composition | **Observed timing:** package 14.05s to 2.35s |
| [#4505](https://github.com/gastownhall/gascity/pull/4505) | Reuse the tagged metrics build | **Observed timing:** 27.69s to 3.99s |
| [#4506](https://github.com/gastownhall/gascity/pull/4506) | Put cleanup branches behind a direct coordinator | **Observed work:** deterministic five-second timeout removed; shard effect projected |
| [#4510](https://github.com/gastownhall/gascity/pull/4510) | Retire duplicate mail acceptance journeys | **Observed work:** 43.66s of test bodies removed; only that causal saving was claimed |
| [#4515](https://github.com/gastownhall/gascity/pull/4515) | Replace integration discovery builds with AST discovery | **Observed work:** 42.72s of gross validator builds removed; job result projected |
| [#4517](https://github.com/gastownhall/gascity/pull/4517) | Put tmux diagnostics under harness ownership | **Neutral:** determinism and diagnostics improved; no timing claim |
| [#4518](https://github.com/gastownhall/gascity/pull/4518) | Check one runtime-tmux manifest | **Observed work:** six discovery builds and 171.96 aggregate seconds removed |
| [#4528](https://github.com/gastownhall/gascity/pull/4528) | Remove duplicate real-city mail checks | **Observed timing:** test execution 85.06s to 18.04s; total PR wall rose 11s because setup changed |
| [#4530](https://github.com/gastownhall/gascity/pull/4530) | Inject a readiness frame | **Observed timing:** 10.06s to 0.06s |
| [#4531](https://github.com/gastownhall/gascity/pull/4531) | Trigger promotion manually | **Observed timing:** 4.09s to 0.11s average |
| [#4532](https://github.com/gastownhall/gascity/pull/4532) | Use `testing/synctest` for shutdown timing | **Observed timing:** 2.70s to about 1ms |
| [#4533](https://github.com/gastownhall/gascity/pull/4533) | Assemble doctor checks directly | **Observed timing:** 16.24s to about 78ms per pair |
| [#4535](https://github.com/gastownhall/gascity/pull/4535) | Inject liveness instead of waiting for kill fallback | **Observed timing:** 5.13s to 0.13s |
| [#4537](https://github.com/gastownhall/gascity/pull/4537) | Double a successful sleep path | **Observed timing:** about 7.2s to 1.7–1.8s |
| [#4541](https://github.com/gastownhall/gascity/pull/4541) | Use a conformance-tested `MemStore` opener | **Observed timing:** 4.80s to 0.59s |
| [#4544](https://github.com/gastownhall/gascity/pull/4544) | Observe process exit and virtualize timer policy | **Observed timing:** 2.39s to 0.49s |
| [#4546](https://github.com/gastownhall/gascity/pull/4546) | Factor a product-metrics matrix | **Observed work/timing:** 175 command roots to 25; 12.88s to 0.95s |
| [#4550](https://github.com/gastownhall/gascity/pull/4550) | Factor eager/lazy initialization cases | **Observed work/timing:** 40 command roots to 4; 15.52s to 1.52s |
| [#4556](https://github.com/gastownhall/gascity/pull/4556) | Conformance-test project identity through a double and real adapter | **Observed work/timing:** 16 real Dolt starts to 1; three-run bundle 29.85s to 12.48s |
| [#4558](https://github.com/gastownhall/gascity/pull/4558) | Test managed recovery through a direct coordinator | **Observed timing:** owner set 30.53s to 8.63s |
| [#4559](https://github.com/gastownhall/gascity/pull/4559) | Inject a clock for nudge budget accounting | **Observed timing:** execution 20.28s to 1.40s |
| [#4564](https://github.com/gastownhall/gascity/pull/4564) | Retire a duplicate deferred-lifecycle journey | **Observed timing:** three-run owner set 30.07s to 5.45s |
| [#4567](https://github.com/gastownhall/gascity/pull/4567) | Preserve Worker Core report controls | **Neutral:** correctness prerequisite; no timing claim |
| [#4568](https://github.com/gastownhall/gascity/pull/4568) | Share Worker Core phase-two builds | **Observed work/timing:** four Go invocations to two; isolated cold run 176.79s to 112.64s |

These measurements are evidence for patterns, not permanent suite baselines.
The relevant PR body and checked timing artifacts remain the source for each
claim.

## The operating loop

```text
measure a cost or reliability defect
              ↓
state one risk and inventory its current owners
              ↓
choose the smallest truthful edge and retained real boundary
              ↓
write a bounded design brief and invariant map
              ↓
delegate one writer → RED → GREEN → refactor → measure
              ↓
refresh on current main → stage exact tree → three council reviews
              ↓
commit → push → PR → required CI
              ↓
verify merge → close bead → remove worktree → choose the next candidate

If the base moves after council:
rebase → restage → recompute tree/patch ID → repeat all three reviews
```

The architect completes the design before delegating implementation. The next
optimization is not started while the current one is awaiting correctness
review, CI repair, or merge unless it has disjoint ownership and the
coordinator explicitly assigns a separate lane.

## How to select a candidate

Start from evidence, not intuition. A candidate needs at least one of:

- a measured latency floor;
- deterministic elapsed-time waiting;
- first-attempt flakiness or weak timeout diagnostics;
- repeated real process, listener, database, provider, or filesystem startup;
- duplicate assertions across high-level journeys;
- repeated command-root, build, discovery, or persistence work; or
- a policy blind spot that allows resource debt to grow.

Prioritize in this order:

1. Authored wall-time floors and flaky polling.
2. Real dependencies used for assertions that do not concern that boundary.
3. Duplicate E2E or acceptance journeys.
4. Cartesian command, provider, and error matrices.
5. Repeated persistence, compilation, and discovery.
6. Cohesive production extraction that reduces package compile/global-state
   tax.
7. Shard balancing after the runnable work is truthful.

Then use this decision tree:

```text
Can the exact regression risk be stated in one sentence?
├─ no  → inventory assertions before editing
└─ yes
   ├─ does a smaller existing proof already own every assertion?
   │  ├─ yes, no unique composition → consolidate or delete with an invariant map
   │  ├─ yes, unique composition    → retain one reduced real-boundary proof
   │  └─ no                         → create or strengthen the smaller owner
   │
   ├─ what makes the proof expensive?
   │  ├─ sleep, timer, backoff      → fake clock or testing/synctest
   │  ├─ async completion           → event, channel, exit, readiness, or barrier
   │  ├─ persistence                → conformant memory owner + one real wiring proof
   │  ├─ process/provider startup   → strict/scripted double + one lifecycle proof
   │  ├─ command matrix             → pure decision table + minimal compositions
   │  ├─ repeated discovery/build   → checked manifest or shared invocation
   │  └─ package compile tax        → extract cohesive production logic
   │
   └─ can one writer measure and review the slice independently?
      ├─ no  → split it
      └─ yes → design, delegate, and implement
```

Stop rather than optimize when:

- the candidate is slow only because its real boundary is the exact risk and no
  duplicate work is present;
- no trustworthy lower owner or conformant substitute exists yet;
- the proposed seam is broader than the consumer's need;
- the change would mix several behaviors or file-ownership domains;
- comparable measurement is unavailable and no deterministic cost can be
  counted; or
- the only plan is to relabel, skip, retry, or move the test to another lane.

## Policy checkpoints for the design

Do not restate test policy in a task. Link the canonical rule and record the
decision it produced:

| Decision to record | Canonical source | Slice artifact |
|---|---|---|
| Smallest truthful owner | [Authoring rule](../../TESTING.md#the-authoring-rule-one-risk-one-smallest-owning-proof) | Named primary owner and unique higher-level risk |
| Production seam | [Fast-proof design](../../TESTING.md#design-production-code-for-fast-proofs) | Existing port, function injection, private bundle, or justified narrow port |
| Failure classes | [Meaningful failure edges](../../TESTING.md#choose-meaningful-failure-edges-not-cartesian-products) | Applicable equivalence classes and intentionally omitted combinations |
| Asynchronous wake | [Wait for facts](../../TESTING.md#asynchronous-tests-wait-for-facts-not-elapsed-time) | Signal/correlation identity, durable reread, and timeout diagnostics |
| Substitute honesty | [Doubles and conformance](../../TESTING.md#test-doubles-and-conformance-are-one-contract) | Applicable shared suite and exact production construction |
| Real journey admission | [E2E portfolio](../../TESTING.md#keep-the-critical-end-to-end-portfolio-deliberately-small) | Unique composition risk, lower owners, lane, budget, and owner |
| Timing claim | [Timing objectives](../../TESTING.md#timing-objectives-and-resource-ratchets) | Comparable commands, environment, layer, samples, and claim class |

## Design the slice before delegation

Every implementation starts with a design brief containing:

1. **Problem and measured cost.** Identify the exact test, package, shard, or
   policy gap and the evidence.
2. **Risk sentence.** Name the condition, observable outcome, and escaping
   regression.
3. **Current assertion owners.** Inventory what each current proof actually
   asserts.
4. **Smallest new owner.** Name the unit, conformance, coordination,
   testscript, integration, or E2E owner.
5. **Seam decision.** State the selected seam and why a broader abstraction is
   unjustified.
6. **Invariant map.** Map every moved or removed assertion.
7. **Retained real boundary.** Name the singular composition proof and what
   only it can catch.
8. **TDD sequence.** Define the RED, GREEN, refactor/removal, and mutation or
   negative proof.
9. **Measurement.** Name exact base/candidate commands and causal counters.
10. **Scope.** List expected files, explicit non-goals, and stop conditions.

Use this risk form:

```markdown
Risk: If <condition>, <observable contract> must <outcome>; otherwise
<specific regression> escapes.
```

Use this invariant map:

```markdown
| Invariant | Old owner | New primary owner | Retained real boundary | Failure evidence |
|---|---|---|---|---|
| ... | ... | ... | ... | RED, mutation, or negative fixture |
```

One primary owner does not forbid composition coverage. It prevents the
composition test from repeating the lower owner's branch matrix.

### Design stop conditions

The implementer stops and returns to the architect when:

- the intended RED fails for a different reason;
- production semantics must change in a behavior-neutral slice;
- a second public abstraction appears necessary;
- expected files or ownership domains expand materially;
- another writer is touching the same files;
- the retained real-boundary proof cannot be named;
- the proposed double cannot share the production contract; or
- measurements contradict the assumed bottleneck.

## Correctness evidence

Follow the canonical
[RED, GREEN, refactor, measure loop](../../TESTING.md#red-green-refactor-measure).
The handoff must retain the failing output or mutation, the minimal passing
change, the invariant map, and results for the primary and real-boundary
owners.

For a behavior-neutral migration, acceptable evidence includes a missing-seam
compile failure, a failing architecture/resource ratchet, a deliberate
mutation caught by the new owner, a negative policy fixture, or
characterization of stable public behavior. Do not manufacture a meaningless
semantic failure.

State neutrality precisely. A policy-scanner slice, for example, may be
production/runtime and test-execution neutral while intentionally changing
policy enforcement.

## Measurement and claim discipline

Measure the thing the slice claims to improve:

| Layer | What it answers |
|---|---|
| Test body | Did deterministic work or waiting leave this test? |
| Package wall time | Did compile, link, setup, and test execution improve together? |
| Owning shard | Did the runnable unit improve in its normal peer set? |
| Workflow/job wall | Did setup, queueing, dependencies, and runner variance preserve the gain? |
| Causal work | Did process starts, writes, command roots, builds, listeners, or retries actually decline? |

Capture exact SHAs, commands, environment/profile, cache condition, sample
count, and layer according to the
[timing policy](../../TESTING.md#timing-objectives-and-resource-ratchets).
Use an isolated cache for cold builds; never clear the shared build cache.

If variance dominates, report “no measurable regression” or “deterministic
wait removed,” not a speedup. PR #4528 is the canonical warning: test execution
dropped about 67 seconds while total PR wall rose because cold setup changed.
PR #4599 correctly reported flat scanner timing as no measurable regression.

## Delegated implementation protocol

Parallelism is used asymmetrically:

- research and candidate audits may run in parallel;
- one architect owns the design and invariant map;
- one implementer is the sole writer for one slice; and
- independent reviewers inspect the finished staged tree in parallel.

Do not assign multiple implementers to overlapping production or ledger files.
When two slices touch the same construction path, fake, manifest, generated
table, or `TESTING.md` section, serialize them or explicitly stack them.

The implementation assignment must include:

- bead, exact base SHA, branch, and isolated worktree;
- statement that the delegate is the sole writer for the slice;
- risk sentence and invariant map;
- chosen test edge, seam, and retained real-boundary owner;
- expected files and explicit non-goals;
- required RED/GREEN or mutation evidence;
- baseline and candidate commands;
- focused, conformance, race, shard, ledger, and docs checks;
- prohibition on broad interfaces, global hooks, hidden polling, retries, and
  unrelated cleanup;
- instruction to stage but not commit or push before council; and
- the stop conditions above.

### Copyable implementation assignment

```markdown
Implement `<bead>` from base `<sha>` in isolated worktree `<path>`.
You are the sole writer for this slice.

Risk: ...

Primary owner: ...
Retained real boundary: ...
Seam decision: ...

Invariant map:
| Invariant | Old owner | New owner | Retained boundary | Evidence |
|---|---|---|---|---|

Expected files:
- ...

Non-goals:
- ...

TDD:
1. RED or mutation proof: ...
2. Minimal GREEN: ...
3. Refactor/removal: ...

Measure:
- test body: ...
- package: ...
- owning shard: ...
- workflow/job, if claimed: ...
- causal work: ...

Run:
- focused owner
- applicable shared conformance
- focused race/repetition
- retained real boundary
- owning shard and checked ledgers/docs

Stop on scope expansion, file collision, changed production semantics, or an
uncontracted substitute. Stage the exact final tree; do not commit or push.
```

## Exact-tree review council

Before commit, delegate three independent, read-only review tasks. The default
council uses three capable SOL review tasks through ordinary task delegation;
it does not require a separate workflow product.

All reviewers receive:

- the same base SHA;
- the same isolated worktree path and expected `HEAD`;
- the same staged tree hash from `git write-tree`;
- the same stable patch ID;
- the design brief and invariant map;
- measurements and exact commands; and
- the instruction to return `APPROVE` or severity-ranked findings.

The three lanes are:

| Lane | Required review |
|---|---|
| Semantic correctness | Read tests first; validate every moved invariant, failure edge, behavior-neutral claim, and retained real-boundary proof. |
| Testing architecture and performance | Validate smallest-owner placement, seam size, double conformance, event-versus-polling choice, E2E admission, and every timing claim. Ensure work was removed rather than moved. |
| Repository and policy integrity | Validate checked ledgers/manifests, generated tables, docs, build tags, CI placement, maintainability, upstream alignment, and bounded scanner/runtime cost. |

For resource-policy changes, the repository lane independently checks
all-source, untagged-source, and effective Small inventories. PR #4599 showed
why: the council caught newly landed tagged helper calls that an
untagged-only audit would have missed.

Any content or base change invalidates the exact-tree approval. Restage,
compute a new tree and patch ID, and rerun all three reviews. The committed
tree must equal the approved tree.

### Copyable council prompt

```markdown
Read-only review in `<worktree>`.
Expected HEAD/base: `<base-sha>`
Expected staged tree: `<tree-hash>`
Expected stable patch ID: `<patch-id>`
Do not edit files or tracker state.

Verify before review:

git rev-parse HEAD
git write-tree
git diff --cached | git patch-id --stable
git diff --quiet
test -z "$(git ls-files --others --exclude-standard)"

Design brief: ...
Invariant map: ...
Evidence: ...

Review lane: `<semantic | architecture/performance | repository/policy>`.

Inspect the tests before implementation. Verify the stated behavior,
ownership, exclusions, and evidence rather than trusting the PR narrative.
Return either:

APPROVE — with the tree hash and checks performed

or severity-ranked findings:
- Critical: ...
- Important: ...
- Suggestion: ...
```

## Commit, PR, and merge shepherding

Use this order:

1. Start from current `origin/main` in an isolated worktree.
2. Finish the slice, fetch `origin/main`, and refresh the slice onto that base
   before freezing it. If the base moved, rerun affected checks and inspect
   the complete candidate diff; recompute rather than hand-resolve generated
   ledgers.
3. Stage only the slice's files. Manually run the repository pre-commit hook
   and relevant focused checks,
   then restage if a tool changed anything.
4. Record `git write-tree` and run the three-lane council.
5. Address findings and repeat all three lanes for every content or base
   change.
6. Commit the approved tree and verify the commit tree matches it.
7. Push, open a focused PR, enable squash auto-merge only after review and
   verification are complete, and watch required CI on the current SHA.
8. If another rebase becomes necessary, recompute the full tree and patch ID,
   inspect `git range-diff`, rerun affected checks, and repeat all three
   council lanes before updating the PR. A cleanly applied patch still has a
   new full-tree context.
9. Treat deterministic test failures as product or test defects. Diagnose and
    fix them; never rerun them into green.
10. Verify the merge SHA on `main`, close the bead, remove the clean worktree
    and branch, and confirm nothing remains unpushed.

If a local gate fails on unchanged base code, reproduce it in a detached
worktree at the exact base SHA. Record both commands and outputs. Bypassing a
hook is exceptional: it requires exact base-failure evidence, no affected
failure in the patch, and successful required remote CI. Keep the base defect
separate from the optimization.

### Copyable PR body

```markdown
## Outcome

<One sentence describing the improved ownership, determinism, or cost.>

## Risk and ownership

Risk: ...

| Invariant | Old owner | New primary owner | Retained real boundary |
|---|---|---|---|

## Design

- Seam: ...
- Double/conformance: ...
- Production behavior: unchanged / intentionally changed as follows
- Explicit exclusions: ...

## Evidence

| Layer | Base `<sha>` | Candidate `<sha>` | Interpretation |
|---|---:|---:|---|
| Test body | ... / N/A | ... / N/A | Observed timing / N/A |
| Package | ... / N/A | ... / N/A | Observed timing / N/A |
| Owning shard | ... | ... | ... |
| Workflow/job | ... / N/A | ... / N/A | Observed / projected / N/A |
| Causal work | ... | ... | ... |

## Verification

- [ ] RED, mutation, or negative proof
- [ ] Focused owner
- [ ] Applicable conformance and race/repetition
- [ ] Retained real-boundary proof
- [ ] Owning shard
- [ ] Checked ledgers/docs
- [ ] Three-lane council approved tree `<tree-hash>`

Bead: `<id>`
```

## Failure handling

| Situation | Response |
|---|---|
| Focused test fails deterministically | Stop and repair or revise the design. Do not retry for a green sample. |
| Result changes with no code change | Investigate test/infra reliability and attach evidence; one owner remains until resolved. |
| Base branch fails the same gate | Reproduce at exact base SHA and separate the base defect from the slice. |
| Measurement is noisy | Interleave samples, count causal work, and narrow the claim. |
| Rebase changes policy inventory | Recompute from source and rerun policy review. |
| New main invalidates the seam or owner | Return to design; do not force the old patch through. |
| CI finds a missing assertion owner | Add the smallest owner first, then decide whether the high-level reproduction is unique. |
| Contributor branch or worktree collides | Stop one writer, preserve both diffs, and reassign explicit file ownership. |

## Dividing the program safely

The coordinator owns dependency ordering, not implementation. Each lane owns a
cohesive boundary and may contain only one active writer per overlapping file
set.

The following is the verified 2026-07-24 allocation snapshot. Query GitHub,
`gc bd`, and current checked ledgers before reusing it. It intentionally
excludes documentation-only bead `ga-80po0c.28`.

| Lane | Current work | Parallelism rule |
|---|---|---|---|
| Resource critical path | `ga-80po0c.2.2.4`, then `.2.2.5` | Strictly serial: both own the same census, ledger, and policy files |
| Timing and race | Residual `ga-80po0c.4`, then `.5` | Independent from scanner implementation; serialize workflow and `TESTING.md` integration |
| Runtime conformance | Remaining `ga-80po0c.3` provider waivers | Package-local proofs may run in parallel; one coordinator serializes provider-ledger and docs changes |
| Docker | Remaining `ga-80po0c.23` real-matrix consolidation | Independent package/design work; land before E1 if possible to avoid immediate manifest churn |
| E1 inventory | Read-only Large-test/provider census for `ga-80po0c.6` | Research may run now; implementation waits for `.2.2.5` and parent completion |
| Tracker reconciliation | Leaves with a verified merge SHA but stale status | Administrative only; do not close aggregate parents merely because some children shipped |

The known listener-policy chain at this checkpoint is:

```text
ga-80po0c.2.2.3 / PR #4599
    → ga-80po0c.2.2.4
        → ga-80po0c.2.2.5
            → E1 / ga-80po0c.6
```

The resource critical path exclusively owns these files while active:

- `internal/testpolicy/resourcecensus/census.go`
- `internal/testpolicy/resourcecensus/census_test.go`
- `internal/testpolicy/resourcecensus/hermetic.go`
- `internal/testpolicy/resourcecensus/hermetic_test.go`
- `test/test-resources.toml`
- `TESTING.md`

Treat `TESTING.md` as a single-writer integration file. Other lanes may prepare
package-local changes concurrently, but canonical-policy updates land
serially.

The old `.2.2.4` branch `test/resource-listener-indirect` at `608299f5` was 216
commits behind this checkpoint and included already-merged predecessor
commits. It is design and test evidence only. Reconstruct its semantic delta on
current `main`; do not merge or cherry-pick the stack wholesale.

The hierarchy audit also found four other leaves still recorded
`in_progress` despite authoritative merged code:

| Bead | Merged evidence |
|---|---|
| `ga-80po0c.22` | [PR #4340](https://github.com/gastownhall/gascity/pull/4340), `afe9d3a3` |
| `ga-80po0c.26` | [PR #4414](https://github.com/gastownhall/gascity/pull/4414), `38910235` |
| `ga-80po0c.27` | [PR #4417](https://github.com/gastownhall/gascity/pull/4417), `6f0aa5c8` |
| `ga-80po0c.3.3` | [PR #4407](https://github.com/gastownhall/gascity/pull/4407), `e3439b29` |

Reconcile those leaves before using ready-queue output as a program plan.
`ga-80po0c.23`, `.3`, `.4`, `.2`, `.2.2`, `.2.2.4`, `.2.2.5`, `.5`, `.6`,
and the root remained legitimately live at the audit checkpoint. Their status
is a snapshot, not policy.

Snapshot provenance is a read-only 2026-07-24 hierarchy/dependency audit of
`ga-80po0c`, reconciled against merged PR SHAs and recorded in
`ga-80po0c.28` notes. The merge evidence establishes code state; future
coordinators must query the tracker for current administrative state.

Do not start a downstream child merely because the predecessor's code appears
present. Verify its merge SHA, close or reconcile stale tracker state, reread
the child against current `main`, and reserve its shared policy files.

### Coordinator protocol

1. Query GitHub and the bead graph before assigning work.
2. Treat a merged GitHub SHA as authoritative for code state; tracker status
   may be stale and must be reconciled before another worker is assigned.
3. Use `gc bd` for scoped tracker operations rather than ambient raw `bd`.
4. Select one candidate through the decision tree and write its design brief.
5. Reserve files and name the sole implementation writer.
6. Delegate independent research in parallel only where it cannot mutate the
   slice.
7. Hold the next dependent slice until merge, bead closure, and cleanup.
8. Keep unrelated lanes parallel only when their production, test-support,
   ledger, generated, and documentation ownership is disjoint.

### Copyable handoff

```markdown
## Outcome
- PR / merge SHA:
- Bead:
- Exact behavior or policy change:

## Ownership
- New primary assertion owners:
- Retained real-boundary proof:
- Contract-backed doubles:

## Evidence
- Test body:
- Package:
- Owning shard:
- Workflow/job:
- Causal counters:
- Claims explicitly not made:

## Repository state
- Main SHA verified:
- Worktree/branch removed:
- Tracker reconciled:
- Known base failures:

## Next schedulable work
- Bead:
- Dependencies satisfied:
- Reserved files:
- Design questions still open:

## Do not duplicate
- ...
```

## Case studies

### 1. Split-store wait: move policy, keep composition

PRs [#4309](https://github.com/gastownhall/gascity/pull/4309) and
[#4333](https://github.com/gastownhall/gascity/pull/4333) moved readiness and
registration decisions onto injected stores, identity, clock, and poke seams.
The representative package result fell from 82.28s to 1.94s, and a later
composition fell from 54.61s to 0.42s. The work did not erase the risk that
CLI/config/file-store and managed-provider pieces compose: one named real
split-store proof and the managed hard-kill/port-rebind proof remained.

Pattern: branch matrices belong to direct use cases; store selection and
provider recovery retain focused composition owners.

### 2. Event-driven waits: determinism is a first-class result

PR [#4340](https://github.com/gastownhall/gascity/pull/4340) replaced an
in-memory watcher polling fallback with broadcast generation channels and
produced 6.15x–9.11x improvements in repeated contract runs. PR
[#4369](https://github.com/gastownhall/gascity/pull/4369) removed 15 sleeps
from live API contracts by capturing SSE cursors and correlating request IDs,
but correctly claimed no demonstrated end-to-end speedup.

Pattern: subscribe before the action, wake on identity-correlated facts, and
reread durable state. Determinism and diagnostics are valid outcomes even when
wall time is flat.

### 3. Exact constructor conformance: remove duplication after truth

PRs [#4326](https://github.com/gastownhall/gascity/pull/4326),
[#4403](https://github.com/gastownhall/gascity/pull/4403),
[#4404](https://github.com/gastownhall/gascity/pull/4404), and
[#4407](https://github.com/gastownhall/gascity/pull/4407) established one
shared conformance run through each exact production composition rather than
duplicating raw implementation and wrapper suites. PR
[#4350](https://github.com/gastownhall/gascity/pull/4350) is the guardrail:
mail contract honesty added about 4.17 seconds, and that cost was accepted
before later consolidation.

Pattern: prove the substitute and actual constructor first. Speed obtained from
an untruthful double is negative progress.

### 4. E2E consolidation: delete journeys, not invariants

PR [#4426](https://github.com/gastownhall/gascity/pull/4426) reduced 34 mail
commands to a five-command bidirectional journey after focused owners covered
the command semantics. PRs
[#4502](https://github.com/gastownhall/gascity/pull/4502),
[#4510](https://github.com/gastownhall/gascity/pull/4510),
[#4528](https://github.com/gastownhall/gascity/pull/4528), and
[#4564](https://github.com/gastownhall/gascity/pull/4564) applied the same
assertion-ownership method to close, mail, real-city, and lifecycle families.

Pattern: inventory every assertion, strengthen the lower owner where needed,
retain the unique composition, then remove the duplicate journey.

### 5. Compile/discovery work: execute once, then ratchet

PRs [#4339](https://github.com/gastownhall/gascity/pull/4339),
[#4505](https://github.com/gastownhall/gascity/pull/4505),
[#4515](https://github.com/gastownhall/gascity/pull/4515),
[#4518](https://github.com/gastownhall/gascity/pull/4518), and
[#4568](https://github.com/gastownhall/gascity/pull/4568) removed repeated
static work, nested builds, runtime discovery, and duplicate Go invocations.
PRs [#4571](https://github.com/gastownhall/gascity/pull/4571),
[#4573](https://github.com/gastownhall/gascity/pull/4573), and
[#4599](https://github.com/gastownhall/gascity/pull/4599) then made resource
ownership harder to regress.

Pattern: replace repeated discovery with checked identity/manifest policy,
share build work, and keep the guard bounded. Sharding cannot remove a giant
package's compile tax.

## Failure patterns observed

The detailed prohibitions live in `TESTING.md`. These program-level mistakes
caused the most churn:

- optimizing a file without first naming its assertion owners;
- moving or deleting a journey before lower owners and the retained boundary
  were explicit;
- using a real dependency for a decision that did not concern that boundary;
- hiding elapsed-time waiting inside a generic helper;
- trusting an uncontracted fake or an all-skipped conformance run;
- adding shards while preserving the same package compile floor;
- reporting a test-body gain as a package, shard, or workflow gain;
- retrying a deterministic failure instead of diagnosing it;
- assigning overlapping writers or reviewing different trees; and
- replaying stale branches or resolving checked ledgers without recomputing
  current source evidence.

## Checkpoint and restart procedure

At the 2026-07-24 checkpoint:

- PR [#4599](https://github.com/gastownhall/gascity/pull/4599) merged as
  `97e1cb5272a41f21efd7e137a143c35cf34cc713`; 51 executed checks passed
  and 28 path-gated checks skipped.
- `ga-80po0c.2.2.3` was reconciled closed against that merge.
- The listener-helper ratchet recorded 38 calls in 13 untagged files, 20 calls
  in 10 tagged files, and 58 calls in 23 files across all source. These are a
  dated evidence snapshot; query the live census for current values.
- No next optimization was started. This corpus is documentation-only work
  performed during that pause.

To restart the program:

1. fetch current `origin/main`;
2. reconcile merged PRs with the `ga-80po0c` graph;
3. verify predecessor closure and current checked inventories;
4. reread the next task against current production and tests;
5. run a fresh candidate/design audit rather than copying an old patch; and
6. assign one implementation writer only after the design brief is complete.

## Primary sources

- [`TESTING.md`](../../TESTING.md) — normative policy and checked live tables.
- [Testing pyramid audit and hardening plan](testing-pyramid-hardening-plan.md)
  — audit evidence, rationale, target architecture, and proposed backlog.
- [`internal/testpolicy/resourcecensus/`](../../internal/testpolicy/resourcecensus/)
  — source-resource census and code-owned policy.
- [`test/test-resources.toml`](../../test/test-resources.toml) — checked
  resource ledger.
- [`internal/testutil/providerledger/`](../../internal/testutil/providerledger/)
  — provider-constructor/conformance ownership.
- [`internal/testpolicy/timingplan/`](../../internal/testpolicy/timingplan/) —
  deterministic timing planner.
- [`scripts/test-go-test-shard`](../../scripts/test-go-test-shard) and
  [`scripts/test-integration-shard`](../../scripts/test-integration-shard) —
  local/CI shard execution.

When evidence in this corpus becomes stale, update the evidence or link to its
new owner. Do not weaken `TESTING.md` to preserve this document.
