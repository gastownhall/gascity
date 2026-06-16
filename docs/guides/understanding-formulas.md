---
title: "Understanding Formulas"
description: How to think about formulas, choose a contract, and apply the major patterns.
---

A formula is *how* a job gets done — written down once as a method instead of
steered live in a prompt. You write the steps, the dependencies between them,
the variables that parameterize them, and the control flow around them into a
TOML file, then apply it whenever that kind of work comes up. Under the v2
contract that method becomes a graph the orchestrator runs for you: it decomposes
the job into beads, fans the ready ones out to as many agents as you have,
gates each step on its dependencies, retries transient failures, and drives the
whole thing to completion outside any single session.

Because the method is written down rather than improvised in a prompt, you get
leverage: preview what a formula will produce before you create anything (`gc
formula show`), apply one method across many runs with different inputs
(`--var`), and catch when a run has drifted from the formula it came from (`gc
formula version-check`).

A [bead](/tutorials/06-beads) is one unit of work; a
[convoy](/tutorials/06-beads) is a graph of related work; a formula is the
reusable method that produces and orders it. Applying a formula materializes its
steps as beads, and from that moment the work is independent of both the formula
file and any agent session: sessions crash and recycle, but the work persists,
so whoever picks it up next finds the same state.

![Applying a formula in three stages: the formula.toml on disk is compiled
into an in-memory recipe (flattened steps plus dependency edges), then
instantiated into beads in the store — the actual work, which then outlives
the file and any agent session.](/diagrams/excalidraw-rendered/formula-apply-pipeline.svg)

This guide is about judgment: which compiler contract to declare, which
instantiation verb to use, and how the major patterns fit together. The
canonical model is [the primitives](/getting-started/how-gas-city-works); the hands-on
walkthrough is the [formulas tutorial](/tutorials/05-formulas); the exact
format rules live in the
[v1](/reference/specs/formula-spec-v1) and
[v2](/reference/specs/formula-spec-v2) specs.

## Choosing a Compiler Contract

Both contracts are live and supported. They are peers, not a version ladder —
each makes a different thing the engine.

| | **v1** (default) | **v2** (`[requires]`) |
|---|---|---|
| Engine | the agent you sling to | the orchestrator |
| Steps | resolved at apply, then inert | independently routable units |
| Control flow | none after apply | check/retry/drain/tally, scope checks, finalize |
| Routing | one agent, one session | many agents and pools (`gc.run_target` per step) |
| Shape | parent-child molecule tree | flat graph of blocking edges + appended finalize |

![Side-by-side comparison of the two contracts. Left, v1: a molecule root
that contains its step beads as parent-child children, so a step that needs
the root waits for all of them. Right, v2: a workflow root plus independent
step beads linked only by blocking-dependency edges, ending in a
workflow-finalize step that the root blocks on — the root goes ready only
when the whole graph completes.](/diagrams/excalidraw-rendered/formula-v1-vs-v2.svg)

For new work, choose v2. The opt-in is one table:

```toml
[requires]
formula_compiler = ">=2.0.0"
```

Base constructs (`steps`, `needs`, `children`, `condition`, `loop`, `vars`,
`extends`) mean the same in both contracts. Graph-only constructs (`check`,
`retry`, `drain`, `on_complete`, `tally`, and reserved `gc.*` step metadata)
require the v2 declaration; compiling without it fails with `requires:
formulas that use graph-only constructs must declare [requires]
formula_compiler = ">=2.0.0" or the deprecated contract = "graph.v2"
explicitly`.

Two v1-only edges remain, neither a reason to start on v1:

- **`gc converge` accepts only v1 formulas** (it rejects v2 until it has an
  explicit input convoy target). For iterate-until-it-passes behavior, use a
  v2 [check loop](#self-checking-work-and-transient-hardening) instead.
- **Container dependencies have a v2 gap.** Under v1 a step that `needs` a
  parent waits for all of that parent's children; the v2 compiler creates no
  parent-child edges yet, so the dependency gates only on the parent step
  ([#3451](https://github.com/gastownhall/gascity/issues/3451)). Until that
  lands, list the children you depend on explicitly in `needs`.

<Note>
The deprecated `contract = "graph.v2"` key still parses (`gc doctor` warns),
and the host-side `[daemon] formula_v2` switch defaults to on. How
requirements compose through `extends` and what doctor reports live in the
[v2](/reference/specs/formula-spec-v2#5-conformance-and-compatibility) and
[v1](/reference/specs/formula-spec-v1#5-conformance-and-compatibility) specs.
</Note>

## Cook, Sling, or Order — and What Lands in the Store

Two more decisions follow the contract: the **verb** (how the instance gets
created and routed) and the **outcome** (what lands in the store, which
follows from the contract).

Three verbs create formula instances:

- **Cook creates without routing.** `gc formula cook <name>` compiles the
  formula, writes its beads into the current scope's store, and stops.
  Nothing wakes up. Cook to inspect the beads first, route the work
  yourself, or graft a sub-DAG onto existing work with `--attach <bead-id>`.
- **Sling creates and routes.** `gc sling <target> <name> --formula` cooks
  and routes in one motion: a v2 formula starts a workflow, a v1 formula
  starts a single-bead run (a *wisp*), both routed to the target. The
  one-shot dispatch verb.
- **Orders are scheduled dispatch.** An order names a formula (or a shell
  command — never both) and a trigger; the orchestrator instantiates and
  routes the formula to the order's pool each time the trigger fires. The
  schedule runs the verb for you.

The outcome follows from the contract, not from a separate choice:

| Outcome | From | Per-step beads | Root is visible work |
|---|---|---|---|
| Single-bead run | v1, no steps (`phase = "vapor"`) | No — steps stay in the recipe | Yes — the root is the work |
| v1 run with steps | v1 with steps (a *molecule*: container root + step children) | Yes, as children | No — the root is a container |
| v2 workflow | v2 | Yes, independently routable | No — the root blocks on finalize |

<Accordion title="Visibility, routing, and cleanup tradeoffs">
- **Visibility.** Materialized steps are real beads you can list, show, and
  watch move through statuses — a per-step audit trail. A single-bead run
  keeps the store lean but gives one bead and no step-level record.
- **Routing.** v2 steps each route to a different agent or pool; a v1 run is
  worked end-to-end by the one agent it was slung to. A pool wakes only for
  Ready-visible work, so slinging a v1 run at a pool is refused outright —
  convert to v2 first.
- **Cleanup.** A single-bead run is ephemeral by design — fire-and-forget
  activity you need no record of. v1-with-steps and v2 workflows leave a
  per-step record. The core pack's cleanup order tidies all three: it reaps
  stale ephemeral runs and purges closed step records, with cleanup edges
  covering v2 workflows too.
</Accordion>

One rule cuts across all of it: **cook and sling in the store the worker
reads.** Each rig has its own bead store; the city has one too. Cook
materializes into the scope you run it from (`--rig` flag, else the enclosing
rig directory, else the city), and sling refuses a cross-store route with
`refusing cross-store route`, telling you to re-file the bead or pick a
reachable target. City-scoped agents are the exception: they are cross-store
eligible and may serve work in any store.

## Major Use Cases

The patterns below cover most of what formulas get used for. Each shows the
minimal shape, what happens at runtime, and where the normative detail lives,
and points at the formula in the
[gascity pack](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas)
that uses the pattern in production.

<Accordion title="Note: the shipped pack predates the current canon">
Every pack formula opts into v2 with the deprecated top-level
`contract = "graph.v2"` key (not the `[requires]` table) and is named
`<name>.formula.toml` (not `<name>.toml`). Both spellings still parse —
`gc doctor` warns about the contract key — and every fence below is adapted to
today's canon. Migrating the pack is tracked in
[#3462](https://github.com/gastownhall/gascity/issues/3462).
</Accordion>

### The Whole Job, End To End

Most real formulas are a pipeline of patterns, not a single one. The clearest
way to see what a formula is *for* is to follow one body of work all the way
through.

![One body of work, end to end: a left-to-right pipeline of stages —
decompose, drain, review (loops until the verdict passes), gap analysis, fix
gaps, check, ship — with a convoy of implementation beads hanging beneath the
drain stage. The stages are the method (the formula); the beads are the work
(the items).](/diagrams/excalidraw-rendered/formula-whole-job.svg)

A typical build decomposes an approved plan into a convoy, drains it so every
ready member is worked at once in dependency-ordered waves, reviews the result
across lanes that loop until the verdict passes, runs a gap analysis, and gates
a final check before shipping. The division of labor is three-sided: the
**work** is the items themselves (independent beads that survive sessions), the
**formula** is the method that declares them and their order, and the
**orchestrator** is the engine that runs the method as a live graph — all
outside any single agent's session.

The sections below are the individual moves in that pipeline —
[decomposition](#planning-reviews-and-decomposition),
[fan-out](#fan-out-over-a-runtime-discovered-set),
[review loops](#multi-lane-review-loops), and
[self-checking](#self-checking-work-and-transient-hardening) — and the pack's
`build-*` chain wires them together with
[`extends`](#multi-step-feature-workflows) into one composed method, not one
giant file.

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

Each step becomes an independent unit of work; `needs` gates readiness so
`implement` stays invisible until `design` closes. You declare the ordering
and the runtime runs whatever is ready — work flows in dependency-ordered
waves, the same mechanism that lets a
[drain](#fan-out-over-a-runtime-discovered-set) run a whole convoy's ready
members in parallel. The appended finalize step closes the workflow when the
last step completes. The same file without `[requires]` compiles under v1 into
a molecule instead.

<Accordion title="In the wild: the build pipeline composes with extends">
Real multi-step builds rarely live in one file. The pack's build pipeline is a
chain of `extends` bases — a `prepare → requirements → plan → plan-review →
decompose → implement → review → finalize → publish` flow assembled across
[`build-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-base.formula.toml)
and the `build-from-*-base` family — and the entries you dispatch are thin
wrappers over those bases.

Composition uses two `extends` rules: a child's steps are appended to the
parent's, but a child step that reuses a parent step's `id` *overrides* it in
place, keeping its position. That rule is how a base declares a skeleton and a
descendant splices new steps into the middle — the descendant redeclares an
inherited step id with a new `needs` list, and the inserted steps land before
it.

The cataloged entrypoint adds almost nothing. The pack's
[`build-from-convoy`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-from-convoy.formula.toml)
is 21 lines with no `[[steps]]` — it extends the base and supplies the catalog
name plus methodology defaults:

```toml
formula = "build-from-convoy"
extends = ["build-from-convoy-base"]

[requires]
formula_compiler = ">=2.0.0"

[catalog]
name = "build-from-convoy"
description = "Continue a build from an implementation convoy through implementation, review, and finalization."

[metadata.gc.methodology]
allowed_drain_policies = ["separate", "same-session"]
implementation_strategy = "drain"
review_modes = ["report", "agent", "interactive"]
```

`gc formula show build-from-convoy` resolves the `extends` chain and prints the
base's full step graph under this name. Build new methodology variants the same
way: extend the matching base and override defaults or routes, rather than
copying the step graph.
</Accordion>

See [steps](/reference/specs/formula-spec-v2#13-steps),
[compilation](/reference/specs/formula-spec-v2#2-compilation), and
[composition and inheritance](/reference/specs/formula-spec-v2#17-composition-and-inheritance)
in the v2 spec.

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

`{{placeholders}}` substitute into titles, descriptions, notes, assignee, and
metadata values. `required` and `enum` are enforced at instantiation, so a
missing or misspelled `env` fails before any bead exists. Every interactive
path takes repeatable `--var` — `gc formula show`, `gc sling --formula`, `gc
formula cook`, and `gc converge create`.

<Accordion title="In the wild: vars keep build bases swappable">
Variables are how the pack's build bases stay swappable without forking the
graph. They flow down the `extends` chain and substitute into both routing
metadata and child-formula names. The convoy base (`build-from-convoy-base`)
routes its drain step to `"gc.run_target" = "{{implementation_target}}"`, so
overriding that one var re-points every drained unit at a different worker
role; its `{{drain_policy}}` var selects which drain step survives compilation
(next section); and the review bases' `{{code_review_formula}}` var (default
`"review"`) names *which formula* the review stage dispatches, so a methodology
pack can swap the whole reviewer by setting a default.

The lesson for your own templates: put the swappable decisions — target roles,
sub-formula names, policy switches — in vars on the base, and let descendants
override the defaults instead of editing steps.
</Accordion>

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
review requests, failing tests, or implementation beads exist right now, and
you want one workflow instance per item, running in parallel. `drain` is the
canonical v2 fan-out for this, and it is the pack's single load-bearing
parallelism pattern: every build entrypoint drains an implementation convoy
into per-member units.

![drain fanning out a convoy: each member of the input convoy is scattered
into its own one-member unit convoy, and the item formula runs for each unit
in parallel. context=separate gives every item its own root; member_access=
exclusive reserves the member while its item
runs.](/diagrams/excalidraw-rendered/formula-drain-fanout.svg)

The real shape, adapted from
[`build-from-convoy-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-from-convoy-base.formula.toml),
declares *two* drain steps guarded by a policy variable and lets compilation
pick one:

```toml
formula = "build-from-convoy"
description = "Drain an implementation convoy, one unit per member."

[requires]
formula_compiler = ">=2.0.0"

[vars.drain_policy]
description = "Drain policy: separate sessions or one shared session."
default = "separate"

[vars.implementation_target]
description = "Role target for implementation work."
default = "gc.implementation-worker"

[[steps]]
id = "prepare-convoy"
title = "Validate the implementation convoy"
metadata = { "gc.run_target" = "gc.run-operator" }

[[steps]]
id = "implement"
title = "Drain the convoy into separate sessions"
needs = ["prepare-convoy"]
condition = "{{drain_policy}} == separate"
metadata = { "gc.run_target" = "{{implementation_target}}" }

[steps.drain]
context = "separate"
formula = "do-work"
member_access = "exclusive"

[[steps]]
id = "implement-same-session"
title = "Drain the convoy in one shared session"
needs = ["prepare-convoy"]
condition = "{{drain_policy}} == same-session"
metadata = { "gc.run_target" = "{{implementation_target}}" }

[steps.drain]
context = "shared"
formula = "do-work-item"
on_item_failure = "skip_remaining"
member_access = "exclusive"

[steps.drain.item]
single_lane = true

[[steps]]
id = "review"
title = "Review the drained work"
needs = ["implement", "implement-same-session"]
metadata = { "gc.run_target" = "gc.implementation-reviewer" }
```

A drain step forces a targeted invocation — sling a bead or convoy at it
(`gc sling gc.run-operator <convoy-id> --on build-from-convoy`); an untargeted
run fails with `requires a target convoy`. Core injects the convoy as the
reserved `convoy_id` target, so the formula never declares it. At runtime the
orchestrator scatters the input convoy into one-member unit convoys and runs the
item formula (itself a v2 formula) once per unit; `member_access = "exclusive"`
reserves each member so no second drain can claim it.

<Accordion title="In the wild: the policy fork and swappable item formula">
Two details make this the pack's workhorse. First, the **policy fork**: both
drain steps `needs`-feed `review`, but `condition` filters them at compile
time, so exactly one survives. `gc formula show build-from-convoy` prints
`implement` (the `context = "separate"` branch, where every unit gets its own
git worktree and they all run in parallel); `--var drain_policy=same-session`
prints `implement-same-session` instead (`context = "shared"`,
`item.single_lane = true`, running units one at a time and skipping the rest
after the first failure via `on_item_failure`). The surviving step's name flows
into `review`'s `needs` automatically.

Second, the item formula is swappable — the real base points `separate` at
[`do-work`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/do-work.formula.toml)
(a checked `prepare-worktree → implement → close-source-anchor` flow) and
`shared` at `do-work-item`, both overridable by descendants.
</Accordion>

`on_complete` also fans out, but over a collection in a *step's structured
output* rather than over convoy members, and `tally` aggregates the results
([#2947](https://github.com/gastownhall/gascity/issues/2947)). The whole
gascity pack is pure-drain — zero `on_complete`, zero `tally`. Prefer drain
when the set is convoy members; reach for `on_complete` only when the set
exists solely in a step's output. (Raw `gc.output_json_required` fan-out is
deprecated; `gc lint` warns `gc.output_json is deprecated; use drain in v2
formulas (see: engdocs/drain-fanout.md)`.)

See [drain](/reference/specs/formula-spec-v2#33-drain) and
[on-complete and tally](/reference/specs/formula-spec-v2#34-on-complete-and-tally) in
the v2 spec.

### Multi-Lane Review Loops

You want several independent verdicts on the same work — acceptance, test
evidence, simplicity — and you want it to keep iterating until the combined
verdict says it is done. The pack expresses this not as a one-shot vote but as
a loop: review lanes fan out, a synthesizer fans them in, a fix step applies
the findings, and the whole subtree repeats until a verdict check passes.

The real example is
[`build-basic-review`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-basic-review.formula.toml),
an expansion formula whose loop core is a `[[template]]` step carrying a
`[template.check]` and a set of `[[template.children]]`:

```toml
formula = "code-review"
description = "Run review lanes until the verdict is done."
type = "expansion"

[requires]
formula_compiler = ">=2.0.0"

[vars.implementation_target]
description = "Role target for fix work."
default = "gc.implementation-worker"

[[template]]
id = "{target}.review-loop"
title = "Review until approved"
needs = ["{target}.setup-review"]
metadata = { "gc.run_target" = "gc.run-operator" }

[template.check]
max_attempts = 6

[template.check.check]
mode = "exec"
path = ".gc/scripts/checks/implementation-review-approved.sh"
timeout = "10m"

[[template.children]]
id = "{target}.acceptance-review"
title = "Review: acceptance criteria"
metadata = { "gc.run_target" = "gc.implementation-reviewer" }

[[template.children]]
id = "{target}.test-evidence-review"
title = "Review: test evidence"
metadata = { "gc.run_target" = "gc.gap-analyst" }

[[template.children]]
id = "{target}.simplicity-review"
title = "Review: simplicity"
metadata = { "gc.run_target" = "gc.design-implementation-reviewer" }

[[template.children]]
id = "{target}.synthesize-review"
title = "Synthesize the three reviews"
needs = ["{target}.acceptance-review", "{target}.test-evidence-review", "{target}.simplicity-review"]
metadata = { "gc.run_target" = "gc.review-synthesizer" }

[[template.children]]
id = "{target}.apply-review-findings"
title = "Apply the synthesized findings"
needs = ["{target}.synthesize-review"]
metadata = { "gc.run_target" = "{implementation_target}", "gc.continuation_group" = "review-fixes" }
```

The single-brace `{target}` and `{implementation_target}` are
expansion-template placeholders, distinct from `{{var}}` substitution; when the
host expands the template, `{target}` is rewritten to the host step's id. `gc
formula show code-review` compiles it into an `iteration.1` scope: the three
review children run in parallel (no `needs` between them), each on a different
reviewer role, fan in to `synthesize-review`, then `apply-review-findings`
makes the smallest fixes and records a verdict in bead metadata. The orchestrator
— never an agent — then runs `implementation-review-approved.sh`; while the
verdict is `iterate` and budget remains (`max_attempts = 6`), it re-spawns the
*entire* lanes-synthesize-fix subtree as the next iteration. The verdict
travels as bead metadata; no judgment lives in Go.

<Warning>
A step cannot combine `expand` with its own `[steps.check]` — the compiler
rejects it with `check cannot be combined with expand`. The expansion's
`[template.check]` is the supported home for the loop, so keep the check on the
template step, not on the step that expands it.
</Warning>

For quorum *voting* — verdicts reduced by `majority`, `unanimous`, or
`any-pass` — use `on_complete` plus `tally` (under
[Fan-Out](#fan-out-over-a-runtime-discovered-set)); the gascity pack does not,
preferring the synthesize-and-recheck loop above.

See [check](/reference/specs/formula-spec-v2#31-check) and
[loops](/reference/specs/formula-spec-v2#16-loops) in the v2 spec.

### Self-Checking Work And Transient Hardening

Two different failure modes, two different constructs — mutually exclusive
on the same step.

`check` is for work you can verify: the step is done when your script says so,
not when the agent says so.

The pack's
[`fix-loop-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/fix-loop-base.formula.toml)
is the production version — it plans fixes, applies them, re-reviews, and gates
the re-review with a `[steps.check]` that runs an artifact validator:

```toml
formula = "fix-loop"
description = "Plan fixes, apply them, re-review until the artifact validates."

[requires]
formula_compiler = ">=2.0.0"

[vars.implementation_target]
description = "Role target for implementation work."
default = "gc.implementation-worker"

[vars.max_iterations]
description = "Maximum fix/review attempts."
default = "10"

[[steps]]
id = "plan-fixes"
title = "Plan review fixes"
metadata = { "gc.run_target" = "gc.review-synthesizer" }

[[steps]]
id = "apply-fixes"
title = "Apply review fixes"
needs = ["plan-fixes"]
metadata = { "gc.run_target" = "{{implementation_target}}" }

[[steps]]
id = "re-review"
title = "Re-review the fixed work"
needs = ["apply-fixes"]
metadata = { "gc.run_target" = "gc.implementation-reviewer", "gc.build.artifact_schema" = "gc.build.review.v1", "gc.build.artifact_path_keys" = "gc.build.review_report_path" }

[steps.check]
max_attempts = 3

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/build-artifact-valid.sh"
timeout = "5m"
```

After each `re-review` closes, the orchestrator runs the script: pass closes the
step; fail with budget left spawns the next iteration; exhaustion closes the
step failed and blocks downstream work. The validator resolves the artifact
from the keys named in `gc.build.artifact_path_keys` and checks it against the
`gc.build.artifact_schema`, so "done" means the schema validator agrees, not
the reviewer. Note the layering: this `[steps.check]` is the bounded,
orchestrator-driven inner loop (`max_attempts`); the `{{max_iterations}}` var
bounds an *outer* loop that callers drive by re-dispatching the whole formula
on a still-failing verdict — judgment in the prompt, iteration in the config.

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

The orchestrator re-runs only attempts it classifies as transient failures.
When the budget runs out, `hard_fail` (the default) closes the step as
failed; `soft_fail` closes it as passed with
`gc.final_disposition=soft_fail` so downstream work continues with degraded
coverage — the right choice for an optional reviewer lane whose absence
should not block the build.

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

### Planning Reviews And Decomposition

Before a build implements anything, two upstream stages do judgment work:
someone reviews the plan, and someone shreds the approved plan into the beads
that the [drain](#fan-out-over-a-runtime-discovered-set) later parallelizes.
The pack models both as ordinary steps whose authority lives in the prompt
and whose artifacts are schema-checked — not in Go.

**Plan review by variable, not by code.** The pack's
[`planning-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/planning-base.formula.toml)
makes the review authority a variable. A single `plan-review` step routes to
a reviewer role, and a `review_mode` var tells the prompt how much authority
it has:

```toml
formula = "planning"
description = "Plan then review the plan; review authority is a variable."

[requires]
formula_compiler = ">=2.0.0"

[vars.review_mode]
description = "Plan-review authority: report, agent, or interactive."
default = "report"

[[steps]]
id = "plan"
title = "Write the implementation plan"
metadata = { "gc.run_target" = "gc.planner" }

[[steps]]
id = "plan-review"
title = "Approve the implementation plan"
needs = ["plan"]
metadata = { "gc.run_target" = "gc.review-synthesizer" }
```

The formula only routes and orders; the description file behind `plan-review`
reads `review_mode` and decides what to do — `report` records findings,
`agent` also produces a fix handoff, `interactive` applies safe fixes
directly. The judgment is a sentence in the prompt, not a branch in code. For a
heavier plan review that loops review lanes until approved, the pack reuses the
expansion-plus-`[template.check]` machinery from
[Multi-Lane Review Loops](#multi-lane-review-loops) — see
[`github-issue-fix-design-review-work`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/github-issue-fix-design-review-work.formula.toml).

**Decomposition shreds a plan into a convoy** — done by an agent, not a formula
construct. The
[`build-from-decompose-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-from-decompose-base.formula.toml)
`decompose` step routes to a decomposer role whose prompt reads the approved
plan, creates an implementation convoy, and stamps the convoy id on the
workflow root so the downstream drain can find it. The formula contributes a
bounded validation loop around that act:

```toml
formula = "decompose"
description = "Shred an approved plan into a convoy of child beads."

[requires]
formula_compiler = ">=2.0.0"

[vars.decomposition_formula]
description = "Decomposition methodology formula used to create implementation beads."
default = "decomposition-base"

[[steps]]
id = "prepare-decompose"
title = "Validate decompose inputs"
metadata = { "gc.run_target" = "gc.run-operator" }

[[steps]]
id = "decompose"
title = "Create the implementation convoy"
needs = ["prepare-decompose"]
metadata = { "gc.run_target" = "gc.task-decomposer", "gc.build.artifact_schema" = "gc.build.decomposition.v1", "gc.build.artifact_path_keys" = "gc.build.decomposition_path" }

[steps.check]
max_attempts = 3

[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/build-artifact-valid.sh"
timeout = "5m"
```

The `[steps.check]` validates the decomposition artifact against its schema
with bounded repair, exactly as the
[self-checking](#self-checking-work-and-transient-hardening) pattern does. The
smaller [`decomposition-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/decomposition-base.formula.toml)
is the swappable methodology contract, named through the
`decomposition_formula` var so a build can swap the shredder without touching
the pipeline. The convoy this step produces is the runtime-discovered set the
drain fans out over — decomposition and fan-out are two ends of the same
pipeline.

See [check](/reference/specs/formula-spec-v2#31-check) and
[variables](/reference/specs/formula-spec-v2#14-variables) in the v2 spec.

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
trigger fires, the orchestrator instantiates the formula and routes it to the
pool. As with sling, a pool wakes only for Ready-visible roots, so order
formulas routed to pools should be v2 (the dispatcher warns otherwise). Test
any order with `gc order run <name>`, which bypasses the trigger.

See the [orders tutorial](/tutorials/07-orders) for triggers, layering, and
overrides.

### Choosing The Shape: A Recap

The two frameworks above compose. Find your situation, read across:

| You want | Contract | Verb | Resulting outcome |
|---|---|---|---|
| Ordered steps worked by one agent | v1 or v2 | `gc sling --formula` | v1 run (a *molecule*) or v2 workflow |
| Steps spread across agents or pools | v2 | `gc sling --formula` | workflow |
| Inspect or route the beads yourself | either | `gc formula cook` | unrouted run (v1) or workflow (v2) |
| A sub-DAG grafted onto existing work | either | `gc formula cook --attach` | steps blocking the given bead |
| One run per convoy member | v2 | `gc sling --on` (targeted) | workflow with drain units |
| Verified or hardened steps | v2 | any | workflow with check or retry controls |
| Recurring work on a trigger | either; v2 for pools | order | one instance per firing |
| Bounded iterative refinement | v2 | `[steps.check]` loop (or v1 `gc converge create`) | orchestrator re-runs until the check passes |
| Reuse a base, change one detail | either | `extends` + same-id override | child graph with the overridden step spliced in |
| Iterating multi-lane review | v2 | expansion with `[template.check]` | lanes → synthesize → fix subtree, re-run until the verdict passes |

### Convergence Loops

Some work is not a pipeline but a loop: draft, evaluate, refine, repeat until
good enough. Express it as a v2 **check loop** — `[steps.check]` re-runs the
work until a verification script passes (see
[Self-Checking Work](#self-checking-work-and-transient-hardening)) — which
keeps the loop inside the formula where the orchestrator drives it.

<Accordion title="If you need `gc converge` (v1 only)">
The dedicated `gc converge` command predates the v2 runtime and accepts only v1
formulas, with no convergence-specific formula keys:

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
</Accordion>

## Where Next

- [The primitives](/getting-started/how-gas-city-works) — Formula alongside
  Agent, Bead, Rig, Pack, and Event.
- [Tutorial 05: Formulas](/tutorials/05-formulas) — write, inspect, and dispatch
  your first formulas hands-on.
- [Formula spec — v2](/reference/specs/formula-spec-v2) and
  [v1](/reference/specs/formula-spec-v1) — the normative format, compilation,
  and runtime rules.
- [Tutorial 07: Orders](/tutorials/07-orders) — scheduled dispatch in depth.
- [The gascity pack](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas)
  — real formulas to read: the `build-*` chain (`extends` composition),
  `do-work` (the drain item contract), `fix-loop-base` and `build-basic-review`
  (check loops), and `planning-base` / `decomposition-base` (review and
  decomposition).
