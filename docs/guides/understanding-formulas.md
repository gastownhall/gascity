---
title: "Understanding Formulas"
description: How to think about formulas, choose a contract, and apply the major patterns.
---

A formula is *how* work should be done. A [bead](/tutorials/06-beads) is the
work itself — a single unit of it — and a [convoy](/tutorials/06-beads) is a
graph of related work; a formula is neither. It is the reusable method you
apply to produce and organize that work. Instead of prompting an agent "do
this thing" and steering every step yourself, you write the method down once
in a TOML file — the steps, the dependencies between them, the variables that
parameterize them, and the control flow around them — and apply it whenever
that kind of work comes up.

Applying a formula is what turns the method into running work, in three
stages. The formula is the file on disk, resolved across pack layers so a city
can override what a pack ships. Compiling it produces a recipe: an in-memory
plan with namespaced step IDs and dependency edges. Instantiating the recipe
creates the beads — and from that moment the work is independent of both the
file and any agent session. Sessions crash, restart, and get recycled; the
beads persist, and whoever picks the work up next finds the same state. That
property — work survives sessions — is what every pattern in this guide builds
on.

![Applying a formula in three stages: the formula.toml on disk is compiled
into an in-memory recipe (flattened steps plus dependency edges), then
instantiated into beads in the store — the actual work, which then outlives
the file and any agent session.](/diagrams/excalidraw-rendered/formula-apply-pipeline.svg)

Because the method is written down rather than improvised in a prompt, you get
leverage: preview what a formula will produce before creating anything (`gc
formula show`), apply one method across many runs with different inputs
(`--var`), and detect when running work has drifted from the formula it came
from (`gc formula version-check`).

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

![Side-by-side comparison of the two contracts. Left, v1: a molecule root
that contains its step beads as parent-child children, so a step that needs
the root waits for all of them. Right, v2: a workflow root plus independent
step beads linked only by blocking-dependency edges, ending in a
workflow-finalize step that the root blocks on — the root goes ready only
when the whole graph completes.](/diagrams/excalidraw-rendered/formula-v1-vs-v2.svg)

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
lives. To keep the examples honest, each one points at the formula in the
[gascity pack](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas)
that uses the pattern in production. Those shipped files predate the current
canon in two ways worth knowing before you read them: every one opts into v2
with the deprecated top-level `contract = "graph.v2"` key instead of the
`[requires] formula_compiler = ">=2.0.0"` table shown throughout this guide,
and every one is named `<name>.formula.toml` rather than the canonical
`<name>.toml`. Both spellings still parse — `gc doctor` warns about the
contract key — and the fences here are adapted to today's canon. Where a note
says "the shipped file still uses the older spelling," that is the only
difference. Migrating the pack to the canonical spelling (and removing this
caveat) is tracked in
[gastownhall/gascity#3462](https://github.com/gastownhall/gascity/issues/3462).

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

**In the wild.** Real multi-step builds rarely live in one file. The gascity
pack's build pipeline is a chain of `extends` bases — a `prepare → requirements
→ plan → plan-review → decompose → implement → review → finalize → publish`
flow assembled across
[`build-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-base.formula.toml)
and the `build-from-*-base` family — and the entries you actually dispatch are
thin wrappers over those bases. Composition uses two `extends` rules. A child
formula's steps are appended to the parent's; but a child step that reuses a
parent step's `id` *overrides* the parent step in place, keeping its position
in the graph. That single rule is how a base can declare a skeleton and a
descendant can splice new steps into the middle of it — the descendant
redeclares an inherited step id with a new `needs` list, and the inserted
steps land before it.

The cataloged entrypoint adds almost nothing. The pack's
[`build-from-convoy`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-from-convoy.formula.toml)
is 21 lines with no `[[steps]]` at all — it extends the base and supplies the
catalog name plus methodology defaults:

```toml
formula = "build-from-convoy"
description = """
Default build continuation from an implementation convoy.

This cataloged entrypoint extends build-from-convoy-base with the
built-in methodology defaults.
"""
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

`gc formula show build-from-convoy` resolves the `extends` chain and prints
the base's full step graph under this name. The shipped file uses
`contract = "graph.v2"` and the `.formula.toml` extension; the `[requires]`
table above is the canonical opt-in. Build new methodology variants the same
way: extend the matching base and override defaults or routes, rather than
copying the step graph.

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

`{{placeholders}}` substitute into titles, descriptions, notes, assignee,
and metadata values. `required` and `enum` are enforced when the formula is
instantiated, so a missing or misspelled `env` fails before any bead exists.
Every interactive path takes `--var`: preview with
`gc formula show deploy --var env=prod`, dispatch with
`gc sling worker deploy --formula --var env=prod`, stage with
`gc formula cook deploy --var env=prod`, and `gc converge create` accepts
repeatable `--var` too.

**In the wild.** Variables are how the pack's build bases stay swappable
without forking the graph. They flow down the `extends` chain and substitute
into both routing metadata and child-formula names. The convoy base
(`build-from-convoy-base`) routes its drain step to
`"gc.run_target" = "{{implementation_target}}"`, so overriding that one var
re-points every drained unit at a different worker role; its
`{{drain_policy}}` var selects which drain step survives compilation (next
section); and the review bases' `{{code_review_formula}}` var (declared on
`build-base` and `build-from-review-base`, default `"review"`) names *which
formula* the review stage dispatches, so a methodology pack can swap the whole
reviewer by setting a default. Each var is declared once on the base and
inherited by every wrapper. The lesson for your own templates: put the
swappable decisions — target roles, sub-formula names, policy switches — in
vars on the base, and let descendants override the defaults instead of
editing steps.

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

A drain step forces a targeted invocation — sling an existing bead or convoy
at it (`gc sling gc.run-operator <convoy-id> --on build-from-convoy`); an
untargeted run fails with `v2 formula "build-from-convoy" requires a target
convoy`. Core injects the convoy as the reserved `convoy_id` target, so the
formula never declares that variable. At runtime the controller scatters the
input convoy into one-member unit convoys and runs the item formula — itself
a v2 formula — once per unit. `member_access = "exclusive"` reserves each
member so no second drain can claim it.

Two details make this the pack's real workhorse rather than a toy. First, the
**policy fork**: both drain steps `needs`-feed `review`, but `condition`
filters them at compile time, so exactly one survives per instance.
`gc formula show build-from-convoy` prints `implement` (the `context =
"separate"` branch, where every unit gets its own git worktree and they all
run in parallel); `--var drain_policy=same-session` prints
`implement-same-session` instead (the `context = "shared"`,
`item.single_lane = true` branch that runs units one at a time in one
session, marking the rest skipped after the first failure via
`on_item_failure`). The surviving step's name flows into `review`'s `needs`
automatically. Second, the item formula is itself swappable — the real base
points `separate` at
[`do-work`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/do-work.formula.toml)
(a checked `prepare-worktree → implement → close-source-anchor` flow) and
`shared` at `do-work-item`, both overridable by descendants. The shipped
files use `contract = "graph.v2"` and the `.formula.toml` extension.

The contrast ([#2947](https://github.com/gastownhall/gascity/issues/2947)):
`on_complete` also fans out, but over a collection in the *step's structured
output* rather than over convoy members, and `tally` can aggregate the
results. Fan-out driven by raw `gc.output_json_required` step metadata is
deprecated — `gc lint` warns `gc.output_json is deprecated; use drain in v2
formulas`. The whole gascity pack is pure-drain: zero `on_complete`, zero
`tally`. Prefer drain when the set is convoy members; reach for `on_complete`
only when the set exists solely in a step's output.

See [drain](/reference/specs/formula-spec-v2#33-drain) and
[on-complete and tally](/reference/specs/formula-spec-v2#34-on-complete-and-tally) in
the v2 spec.

### Multi-Lane Review Loops

You want several independent verdicts on the same work — an acceptance
reviewer, a test-evidence reviewer, a simplicity reviewer — and you want the
work to keep iterating until the combined verdict says it is done. This is
the "code review for best practices" pattern, and the gascity pack expresses
it not as a one-shot vote but as a loop: review lanes fan out, a synthesizer
fans them in, a fix step applies the findings, and the whole subtree repeats
until a verdict check passes.

The real example is
[`build-basic-review`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-basic-review.formula.toml),
an expansion formula. Adapted to canon, its loop core is a `[[template]]`
step carrying a `[template.check]` and a set of `[[template.children]]`:

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
expansion-template placeholders, distinct from the `{{var}}` substitution
used in ordinary steps; when the host expands this template, `{target}` is
rewritten to the host step's id. `gc formula show code-review` compiles the
template into an `iteration.1` scope: the three review children run in
parallel (no `needs` between them), each on a different reviewer role, fan in
to `synthesize-review`, then `apply-review-findings` makes the smallest fixes
and records a verdict in bead metadata. The controller — never an agent —
then runs `implementation-review-approved.sh`, which reads the verdict for
the current iteration and the report's severities; while it is `iterate` and
budget remains (`max_attempts = 6`), the controller re-spawns the *entire*
lanes-synthesize-fix subtree as the next iteration. The verdict travels as
bead metadata; no judgment lives in Go.

This is the canonical real-world fan-out review, with no vote-tally plumbing
at all — it loops on a check verdict instead of counting ballots. Two notes
on the shipped file. It uses `contract = "graph.v2"` and the `.formula.toml`
extension. And its host,
[`build-basic`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-basic.formula.toml),
combines `expand` with its own `[steps.check]` on the same step — a shape the
current compiler rejects (`check cannot be combined with expand`): the
expansion's `[template.check]` is the supported home for the loop, so keep the
check on the template step, as above, not on the step that expands it.

If you genuinely need quorum *voting* — independent verdicts reduced by
`majority`, `unanimous`, or `any-pass` — that is `on_complete` plus `tally`,
covered under [Fan-Out](#fan-out-over-a-runtime-discovered-set); the gascity
pack does not use it, preferring the synthesize-and-recheck loop above.

See [check](/reference/specs/formula-spec-v2#31-check) and
[loops](/reference/specs/formula-spec-v2#16-loops) in the v2 spec.

### Self-Checking Work And Transient Hardening

Two different failure modes, two different constructs — mutually exclusive
on the same step.

`check` is for work you can verify: the step is not done when the agent says
so, but when your script says so.

The pack's gap-analysis loop is the production version of this. The
[`gap-analysis`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/gap-analysis.formula.toml)
formula itself contains no loop — it is a two-step report producer. The
looping lives in
[`fix-loop-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/fix-loop-base.formula.toml),
which wraps a fix-and-re-verify cycle around it: plan the fixes, apply them,
re-review, and gate the re-review with a `[steps.check]` that runs an
artifact validator. Adapted to canon:

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

After each iteration of `re-review` closes, the controller runs the script.
Pass closes the step; fail with budget left spawns the next iteration —
`gc formula show fix-loop` shows `re-review` materialized into a `.spec`
sidecar, an agent-visible `iteration.1`, and a controller-owned control bead;
exhaustion closes the step as failed and blocks downstream work. The
validator resolves the artifact from the metadata keys named in
`gc.build.artifact_path_keys` and checks it against the schema in
`gc.build.artifact_schema`, so "done" means the schema validator agrees, not
the reviewer. Note the layering: this `[steps.check]` is the bounded,
controller-driven inner loop (`max_attempts`); the `{{max_iterations}}` var
bounds an *outer* loop that callers drive by re-dispatching the whole fix
formula on a still-failing verdict — judgment in the prompt, iteration in the
config. The shipped file uses `contract = "graph.v2"` and the `.formula.toml`
extension.

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
reads `review_mode` and decides what to do — in `report` mode it records
findings without touching the plan, in `agent` mode it also produces a fix
handoff, in `interactive` mode it applies safe fixes directly. That is Zero
Framework Cognition: the judgment is a sentence in the prompt, not a branch
in code. For a heavier plan review that loops review lanes until approved,
the pack reuses the same expansion-plus-`[template.check]` machinery shown in
[Multi-Lane Review Loops](#multi-lane-review-loops) — see
[`github-issue-fix-design-review-work`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/github-issue-fix-design-review-work.formula.toml).

**Decomposition shreds a plan into a convoy.** The work is split into beads
not by a formula construct but by an agent — the
[`build-from-decompose-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/build-from-decompose-base.formula.toml)
`decompose` step routes to a decomposer role whose prompt reads the approved
plan, creates an implementation convoy with its member beads, and stamps the
convoy id on the workflow root so the downstream drain can find it. The
formula contributes a bounded validation loop around that act:

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
with bounded repair, exactly as the [self-checking](#self-checking-work-and-transient-hardening)
pattern does — the decomposer's output is not trusted until it validates. The
smaller [`decomposition-base`](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas/decomposition-base.formula.toml)
is the swappable methodology contract: one checked step that packs override,
named through the `decomposition_formula` var so a build can swap the shredder
without touching the pipeline. The convoy this step produces is precisely the
runtime-discovered set the drain fans out over — decomposition and fan-out are
two ends of the same pipeline. The shipped files use `contract = "graph.v2"`
and the `.formula.toml` extension.

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
| Reuse a base, change one detail | either | `extends` + same-id override | child graph with the overridden step spliced in |
| Iterating multi-lane review | v2 | expansion with `[template.check]` | lanes → synthesize → fix subtree, re-run until the verdict passes |

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
- [The gascity pack](https://github.com/gastownhall/gascity-packs/tree/main/gascity/formulas)
  — real formulas to read. The `build-*` chain shows `extends` composition
  end to end, `do-work` / `do-work-item` show the drain item contract,
  `fix-loop-base` and `build-basic-review` show check loops, and
  `planning-base` / `decomposition-base` show review and decomposition. They
  predate the current `[requires]` opt-in and use `contract = "graph.v2"`
  with the `.formula.toml` extension.
