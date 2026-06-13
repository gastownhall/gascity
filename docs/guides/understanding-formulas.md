---
title: "Understanding Formulas"
description: How to think about formulas, choose a contract, and apply the major patterns.
---

A formula is a declarative definition of work. Instead of prompting an agent
with "do this thing," you write a TOML file that describes the work itself —
the steps, the dependencies between them, the variables that parameterize
them, and the control flow around them — and Gas City turns that description
into durable work items.

The pipeline has three stages. The formula is the file on disk, resolved
across pack layers so a city can override what a pack ships. Compiling it
produces a recipe: an in-memory plan with namespaced step IDs and dependency
edges. Instantiating the recipe creates beads in the store — and from that
moment the work is independent of both the file and any agent session.
Sessions crash, restart, and get recycled; the beads persist, and whoever
picks the work up next finds the same state. That property — work survives
sessions — is what every pattern in this guide builds on.

Because the unit is data, you get leverage you would never get from a prompt:
preview a formula before creating anything (`gc formula show`), parameterize
one definition across many runs (`--var`), and detect when a running instance
has drifted from the file it came from (`gc formula version-check`).

This guide is about judgment: which compiler contract to declare, which
instantiation verb to use, and how the major patterns fit together. The
hands-on walkthrough is the [formulas tutorial](/tutorials/05-formulas); the
exact format rules live in the two specifications, the
[v1 formula spec](/reference/specs/formula-spec-v1) and the
[v2 formula spec](/reference/specs/formula-spec-v2).

## Choosing a Compiler Contract

Two compiler contracts are live, and both are supported. They are peers, not
a version ladder — each is the right answer to a different question.

The **v1 contract** — the default when a formula declares nothing — compiles
steps into a parent-child molecule tree. Everything dynamic is resolved at
cook time: conditions filter steps in or out, loops unroll into concrete
iterations. After cooking, the molecule is inert data. Agents advance the
work from inside their own sessions, and nothing else moves the workflow
along.

The **formulas v2 contract** compiles the same steps into a flat graph of
independently routable step beads connected only by blocking dependency
edges. The compiler appends a `workflow-finalize` control step after all sink
steps and makes the root block on it, so the root never surfaces as ready
work while the graph is running; the controller closes the root, pass or
fail, when the graph completes. The structural change carries an
execution-model change: at runtime the controller executes every control
bead — check and retry evaluation, fan-out, tally, drain, scope checks,
finalize — while agents only ever run plain work beads. Per-step routing
(`gc.run_target`) is resolved at dispatch, so one workflow can spread its
steps across agents and pools.

In reader terms: under v1, the agent you sling to is the engine. Under v2,
the controller is the engine and agents are interchangeable workers that the
engine feeds.

For new work, choose v2. Two edges of the v1 surface have not finished
converging, but neither is a reason to start on v1:

- **`gc converge` currently accepts only v1 formulas** (it rejects v2 until
  it has an explicit input convoy target). For iterate-until-it-passes
  behavior, use a v2 [check loop](#self-checking-work-and-transient-hardening)
  instead of `gc converge`.
- **Container dependencies have a known v2 gap.** Under v1, a step that
  `needs` a parent waits for all of that parent's children; the v2 compiler
  creates no parent-child edges yet, so the same dependency gates only on
  the parent step itself
  ([#3451](https://github.com/gastownhall/gascity/issues/3451)). Until that
  lands, list the children you depend on explicitly in `needs`.

Base constructs — `steps`, `needs`, `children`, `condition`, `loop`, `vars`,
`extends` — are common to both contracts and mean the same thing in both.
Graph-only constructs — `check`, `retry`, `drain`, `on_complete`, `tally`,
and certain reserved `gc.*` step metadata — require an explicit v2
declaration; compiling without one fails with `requires: formulas that use
graph-only constructs must declare [requires] formula_compiler = ">=2.0.0"
or the deprecated contract = "graph.v2" explicitly`.

The opt-in is one table:

```toml
[requires]
formula_compiler = ">=2.0.0"
```

That is the entire mechanism. The deprecated `contract = "graph.v2"` key
still parses (and `gc doctor` warns about it), and the host-side
`[daemon] formula_v2` switch defaults to on. The full rules — how
requirements compose through `extends`, what conflicts look like, and what
doctor reports — live in the specs: see
[v2 conformance and compatibility](/reference/specs/formula-spec-v2#5-conformance-and-compatibility)
and its [v1 counterpart](/reference/specs/formula-spec-v1#5-conformance-and-compatibility).

## Wisp or Molecule, Cook or Sling

Once the contract is chosen, you face two more decisions: the **verb** (how
the instance gets created and routed) and the **shape** (what lands in the
bead store). They are related but separate.

Three verbs create formula instances:

- **Cook creates without routing.** `gc formula cook <name>` compiles the
  formula, writes its beads into the current scope's store, and stops.
  Nothing is assigned; nothing wakes up. Cook when you want to inspect the
  beads first, route the work yourself, or graft a sub-DAG onto existing
  work with `--attach <bead-id>`.
- **Sling creates and routes.** `gc sling <target> <name> --formula` does
  the cook and the routing in one motion: a v2 formula starts a workflow
  routed to the target, a v1 formula becomes a wisp routed to the target.
  Sling is the one-shot dispatch verb.
- **Orders are scheduled dispatch.** An order names a formula (or a shell
  command — never both) and a trigger; the controller instantiates the
  formula each time the trigger fires and routes it to the order's pool. You
  never run a verb at all — the schedule does.

Three shapes land in the store:

| Shape | How you get it | Per-step beads | Root is visible work |
|---|---|---|---|
| Root-only wisp (v1-era) | `phase = "vapor"` formula (no `pour`) — a holdover from when bead writes were expensive; not a shape to design for | No — steps stay in the recipe | Yes — the root is the work |
| v1 molecule | v1 formula with steps | Yes, as children of the container root | No — the root is a container |
| v2 workflow | v2 formula | Yes, independently routable | No — the root blocks on finalize |

The tradeoffs behind that table:

- **Visibility and debugging.** Materialized steps are real beads you can
  list, show, and watch move through statuses — a per-step audit trail. A
  root-only wisp keeps the store lean but gives you a single bead and no
  step-level record.
- **Routing.** v2 workflow steps are each routable to a different agent or
  pool; a v1 molecule is typically worked end-to-end by the one agent it was
  slung to. Pools add a constraint: a pool wakes only for Ready-visible
  work, so slinging a v1 molecule at a pool is refused outright — convert
  the formula to v2 first.
- **Cleanup.** Wisps are ephemeral by design: the core pack's reaper order
  exists to reap stale wisps and purge closed molecules, and its cleanup
  edges cover v2 workflows too. Use wisps for fire-and-forget activity you
  do not need a durable record of; use materialized molecules and workflows
  when the step history is the point.

One rule cuts across all of it: **cook and sling in the store the worker
reads.** Each rig has its own bead store, and the city has one too. Cook
materializes into the scope you run it from (`--rig` flag, else the
enclosing rig directory, else the city), and sling refuses a cross-store
route — a bead in one rig's store slung at an agent that reads a different
store fails with `refusing cross-store route`, telling you to re-file the
bead or pick a reachable target. City-scoped agents are the exception: they
are cross-store eligible and may serve work in any store.

## Major Use Cases

The patterns below cover most of what formulas get used for. Each shows the
minimal shape, what happens at runtime, and where the normative detail
lives.

### Multi-Step Feature Workflows

You have one unit of work with ordered phases — design it, build it, review
it, ship it — and you want the phases tracked and gated instead of trusted
to an agent's memory.

```toml
formula = "feature-flow"
description = "Design, implement, and review {{feature}}"

[requires]
formula_compiler = ">=2.0.0"

[vars]
feature = "the feature"

[[steps]]
id = "design"
title = "Design {{feature}}"

[[steps]]
id = "implement"
title = "Implement {{feature}}"
needs = ["design"]

[[steps]]
id = "review"
title = "Review the implementation"
needs = ["implement"]

[[steps]]
id = "submit"
title = "Submit the change"
needs = ["review"]
```

At runtime each step becomes a bead, `needs` edges gate readiness so
`implement` only surfaces once `design` closes, and the appended finalize
step closes the root when the last step completes. Sling it at an agent or a
pool and the steps flow in order. The same file without the `[requires]`
table compiles under v1 into a molecule instead — declare v2 when you want
per-step routing and runtime control.

See [steps](/reference/specs/formula-spec-v2#13-steps) and
[compilation](/reference/specs/formula-spec-v2#2-compilation) in the v2 spec.

### Parameterized Templates

You want one definition to serve many runs — same workflow, different
feature, environment, or target. Declare variables, constrain the dangerous
ones, and supply values at instantiation.

```toml
formula = "deploy"
description = "Deploy {{env}} from {{branch}}"

[vars]
branch = "main"

[vars.env]
description = "Deployment environment"
required = true
enum = ["dev", "staging", "prod"]

[[steps]]
id = "deploy"
title = "Deploy {{env}} from {{branch}}"
```

`{{placeholders}}` substitute into titles, descriptions, notes, assignee,
and metadata values. `required` and `enum` are enforced when the formula is
instantiated, so a missing or misspelled `env` fails before any bead exists.
Every interactive path takes `--var`: preview with
`gc formula show deploy --var env=prod`, dispatch with
`gc sling worker deploy --formula --var env=prod`, stage with
`gc formula cook deploy --var env=prod`, and `gc converge create` accepts
repeatable `--var` too.

<Note>
Orders are the exception: order TOML has no variable mechanism and
`gc order run` has no `--var` flag
([#1813](https://github.com/gastownhall/gascity/issues/1813)), so a formula
with required variables cannot be dispatched by an order. Give every
variable a default if the formula must run on a schedule.
</Note>

See [variables](/reference/specs/formula-spec-v2#14-variables) in the v2 spec.

### Fan-Out Over a Runtime-Discovered Set

You do not know the work items until runtime — a convoy holds however many
review requests, failing tests, or stale databases exist right now, and you
want one workflow instance per item. `drain` is the canonical v2 fan-out
for this.

```toml
formula = "review-batch"
description = "Run the item formula for each convoy member"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "scatter"
title = "Process every member"

[steps.drain]
formula = "mol-do-work"
context = "separate"
max_units = 20
```

A drain step forces a targeted invocation — sling an existing bead or
convoy at it with `gc sling worker <convoy-id> --on review-batch` (an
untargeted run fails with `v2 formula "review-batch" requires a target
convoy`). At runtime the controller scatters the input convoy into
one-member unit convoys and runs the item formula — which must itself
declare the v2 contract — once per unit, up to `max_units` (1–100),
applying `on_item_failure` when a unit fails.

The contrast ([#2947](https://github.com/gastownhall/gascity/issues/2947)):
`on_complete` also fans out, but over a collection in
the *step's structured output* rather than over convoy members, and `tally`
can aggregate the results. Fan-out driven by raw `gc.output_json_required`
step metadata is deprecated — `gc lint` warns `gc.output_json is
deprecated; use drain in v2 formulas`. Prefer drain when the set is convoy
members; reach for `on_complete` when the set only exists in a step's
output.

See [drain](/reference/specs/formula-spec-v2#33-drain) and
[on-complete and tally](/reference/specs/formula-spec-v2#34-on-complete-and-tally) in
the v2 spec.

### Review And Vote Quorums

You want several independent verdicts on the same work — multiple reviewers,
multiple models — and a single pass/fail outcome computed from their votes.
The shape is a fan-out plus a `tally`.

```toml
formula = "vote"
description = "Fan out reviewers and tally their verdicts"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "survey"
title = "Survey reviewers"

[steps.on_complete]
for_each = "output.reviewers"
bond = "mol-do-work"
parallel = true

[steps.on_complete.vars]
reviewer = "{item.name}"

[steps.tally]
vote_field = "verdict"
mode = "majority"
```

At runtime, when `survey` completes, the controller instantiates the bonded
formula once per element of `output.reviewers`, then a tally control step
reads `verdict` from each voter's structured output and resolves the quorum:
`majority`, `unanimous`, or `any-pass`. Downstream steps wait on the tally
result, not on the individual voters.

The real bundled example is `mol-review-quorum`, which fans out two reviewer
lanes with per-lane provider and model variables and retries transient lane
failures with `on_exhausted = "soft_fail"`. It predates `drain` and persists
lane output through the deprecated `gc.output_json_required` metadata that
`gc lint` flags — it is the grandfathered example. Borrow its lane and
retry ideas, but model new fan-out on `drain` and `tally`, not on its output
plumbing.

See [on-complete and tally](/reference/specs/formula-spec-v2#34-on-complete-and-tally)
in the v2 spec.

### Self-Checking Work And Transient Hardening

Two different failure modes, two different constructs — mutually exclusive
on the same step.

`check` is for work you can verify: the step is not done when the agent says
so, but when your script says so.

```toml
formula = "checked"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "implement"
title = "Implement the feature"

[steps.check]
max_attempts = 3

[steps.check.check]
mode = "exec"
path = "scripts/verify.sh"
timeout = "2m"
```

After each iteration closes, the controller runs the script. Pass closes
the step; fail with budget left spawns the next iteration; exhaustion closes
the step as failed.

`retry` is for steps that fail for boring reasons — provider hiccups,
timeouts — where re-running is the fix:

```toml
formula = "retry-fetch"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "fetch"
title = "Fetch the dataset"

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

The controller re-runs only attempts it classifies as transient failures.
When the budget runs out, `hard_fail` (the default) closes the step as
failed; `soft_fail` closes it as passed with
`gc.final_disposition=soft_fail` so downstream work continues with degraded
coverage — the choice `mol-review-quorum` makes for its reviewer lanes.

<Warning>
The control plane is idempotent; the data plane is not
([#3005](https://github.com/gastownhall/gascity/issues/3005)). A check
iteration or retry attempt re-runs the whole step with no record of
irreversible side effects the failed attempt already landed — a pushed
commit, a posted PR comment, sent mail. Keep checked and retried step
bodies idempotent, or budget `max_attempts` knowing each attempt may repeat
its side effects.
</Warning>

See [check](/reference/specs/formula-spec-v2#31-check) and
[retry](/reference/specs/formula-spec-v2#32-retry) in the v2 spec.

### Scheduled And Maintenance Work Via Orders

Recurring work — digests, sweeps, health checks — should not depend on
anyone remembering to sling it. An order binds a formula to a trigger:

```toml
[order]
description = "Pour the nightly digest workflow"
formula = "nightly-digest"
trigger = "cron"
schedule = "0 6 * * *"
pool = "worker"
```

An order names a formula *or* an `exec` shell command, never both —
deterministic maintenance belongs in `exec`, judgment work in a formula.
Triggers are `cooldown`, `cron`, `condition`, `event`, or `manual`. When the
trigger fires, the controller instantiates the formula and routes the
instance to the pool. Pool readiness matters here the same way it does for
sling: a pool wakes only for Ready-visible roots, so order formulas routed
to pools should be v2 — the dispatcher warns otherwise. Test any order immediately with `gc order run <name>`, which
bypasses the trigger.

See the [orders tutorial](/tutorials/07-orders) for triggers, layering, and
overrides.

### Choosing The Shape: A Recap

The two frameworks above compose. Find your situation, read across:

| You want | Contract | Verb | Resulting shape |
|---|---|---|---|
| Ordered steps worked by one agent | v1 or v2 | `gc sling --formula` | molecule (v1) or workflow (v2) |
| Steps spread across agents or pools | v2 | `gc sling --formula` | workflow |
| Inspect or route the beads yourself | either | `gc formula cook` | unrouted molecule or workflow |
| A sub-DAG grafted onto existing work | either | `gc formula cook --attach` | steps blocking the given bead |
| One run per convoy member | v2 | `gc sling --on` (targeted) | workflow with drain units |
| Verified or hardened steps | v2 | any | workflow with check or retry controls |
| Recurring work on a trigger | either; v2 for pools | order | one instance per firing |
| Bounded iterative refinement | v2 | `[steps.check]` loop (or v1 `gc converge create`) | controller re-runs until the check passes |

### Convergence Loops

Some work is not a pipeline but a loop: draft, evaluate, refine, repeat
until good enough. The recommended way to express this is a v2 **check
loop** — `[steps.check]` re-runs the work until a verification script
passes, as covered in
[Self-Checking Work](#self-checking-work-and-transient-hardening) — because
it keeps the loop inside the formula where the controller drives it.

Gas City also has a dedicated command, `gc converge`, which predates the v2
runtime and currently accepts only v1 formulas — there are no
convergence-specific formula keys:

```toml
formula = "refine-doc"
description = "Revise the draft against the evaluation feedback"

[[steps]]
id = "revise"
title = "Revise the draft"
description = "Apply the feedback from the previous iteration."
```

`gc converge create --formula refine-doc --target worker --evaluate-prompt "..."`
creates the loop, bounded by `--max-iterations` (default 5); each iteration
cooks the formula as a convergence wisp with your `--var` values plus the
evaluate prompt injected as the `evaluate_prompt` variable, and a gate —
manual approval or a condition script — decides whether to iterate again or
stop. `gc converge` rejects v2 formulas until it gains an explicit input
convoy target, so reach for it only when you specifically want its
gate-and-evaluate machinery; otherwise prefer the v2 check loop above.

See [conformance and compatibility](/reference/specs/formula-spec-v1#5-conformance-and-compatibility)
in the v1 spec.

## Where Next

- [Tutorial 05: Formulas](/tutorials/05-formulas) — write, inspect, and
  dispatch your first formulas hands-on.
- [Formula spec (v2)](/reference/specs/formula-spec-v2) — the normative format,
  compilation, and runtime rules for formulas v2.
- [Formula spec (v1)](/reference/specs/formula-spec-v1) — the normative rules for the
  v1 contract.
- [Tutorial 07: Orders](/tutorials/07-orders) — scheduled dispatch in
  depth.
