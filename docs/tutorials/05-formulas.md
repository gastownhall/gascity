---
title: Tutorial 05 - Formulas
sidebarTitle: 05 - Formulas
description: Capture how multi-step work should be done — steps, dependencies, variables, and control flow — in a reusable formula, then dispatch it to agents.
---

So far you've been giving agents work one piece at a time — `gc sling my-agent
"do this thing"`. That works, but real workflows have multiple steps with
dependencies between them. This tutorial shows how to define multi-step
workflows as _formulas_ and dispatch them as a unit.

One of the main reasons agent orchestration engines like Gas City exist is to
coordinate various pieces of work without a human or shell script trying to feed
the right prompts at the right times. In Gas City, we use _formulas_ to write
down all of the things we want to happen, and then hand them off to the agent to
do our bidding.

A formula describes the steps that need to take place, but it's not _quite_ step
by step instructions. As with many things in life, some things need to happen
one after another, but a lot of things can happen in parallel.

A formula is a TOML file that captures _how_ a piece of work should be done — a
collection of steps with dependencies, variables, and optional control flow.
It isn't the work itself (that's a bead); it's the reusable method, and running
it produces the work. To run a formula, you `gc sling` it to an agent just as
you would any other work.

## A simple formula

Formula files use the `.toml` extension and live in your city's
`formulas/` directory. To follow along, write a pancakes recipe into that
directory:

```shell
~/my-city
$ cat > formulas/pancakes.toml << 'EOF'
formula = "pancakes"
description = "Make pancakes from scratch"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "dry"
title = "Mix dry ingredients"
description = "Combine flour, sugar, baking powder, salt in a large bowl."

[[steps]]
id = "wet"
title = "Mix wet ingredients"
description = "Whisk eggs, milk, and melted butter together."

[[steps]]
id = "combine"
title = "Combine wet and dry"
description = "Fold wet ingredients into dry. Do not overmix."
needs = ["dry", "wet"]

[[steps]]
id = "cook"
title = "Cook the pancakes"
description = "Heat griddle to 375F. Pour 1/4 cup batter per pancake."
needs = ["combine"]

[[steps]]
id = "serve"
title = "Serve"
description = "Stack pancakes on a plate with butter and syrup."
needs = ["cook"]
EOF
```

The `[requires]` block opts this formula into Gas City's graph compiler — the
**formulas v2** contract. Under v2, every step becomes its own independently
routable bead, and runtime constructs like checks and retries (coming later in
this tutorial) become available. Every formula in this tutorial declares it,
and yours should too. The reference explains the contracts side by side:
[Choosing a Compiler Contract](/guides/understanding-formulas#choosing-a-compiler-contract).

The `needs` field declares dependencies between sibling steps.

- `dry` and `wet` can run in parallel
- `combine` needs both `dry` and `wet` to complete before it runs
- `cook` waits for `combine`
- `serve` waits for `cook`

Once all of these steps are complete, the formula is done.

Without these `needs` declarations, everything could happen at any time, which
would yield a messy kitchen, not a stack of delicious pancakes.

## Inspecting formulas

The `formulas` directory contains many formula files. You can `ls` the directory
or you can ask `gc` to enumerate them for you.

```shell
~/my-city
$ gc formula list
mol-do-work
mol-dog-stale-db
mol-polecat-base
mol-polecat-commit
mol-polecat-report
mol-prompt-synth
mol-review-quorum
mol-scoped-work
pancakes
```

Your fresh city composes several `mol-*` formulas from the explicit imports
that `gc init` wrote: bundled system packs such as `core` and `bd`/`dolt`,
plus the default `gascity` methodology pack. Those imports provide the stock
worker workflow, Dolt maintenance workflows, and planning or implementation
workflows.
The list above shows imported formulas alongside the `pancakes` you just
defined — the exact set may grow as pack releases change, so expect your
output to include additional `mol-*` entries.

To see the compiled recipe for a specific formula:

```shell
~/my-city
$ gc formula show pancakes
Formula: pancakes
Description: Make pancakes from scratch

Steps (6):
  ├── pancakes.dry: Mix dry ingredients
  ├── pancakes.wet: Mix wet ingredients
  ├── pancakes.combine: Combine wet and dry [needs: pancakes.dry, pancakes.wet]
  ├── pancakes.cook: Cook the pancakes [needs: pancakes.combine]
  ├── pancakes.serve: Serve [needs: pancakes.cook]
  └── pancakes.workflow-finalize: Finalize workflow [needs: pancakes.serve]
```

`gc formula show` _compiles_ the formula by arranging the steps and the
dependencies, then displaying it to you. You wrote five steps, but the recipe
shows six: the graph compiler appends a `workflow-finalize` **control step**
that depends on the last steps in your graph. Agents never work on it — Gas
City's controller completes it once everything upstream is done and uses it to
record the workflow's outcome and close the workflow. The `(6)` count matches
the steps shown.

For the next few examples, keep using the `mayor` from the earlier tutorials
and add a generic worker so you have a second execution target besides the
reviewer:

```shell
~/my-city
$ gc agent add --name worker
Scaffolded agent 'worker'

~/my-city
$ cat > agents/worker/prompt.template.md << 'EOF'
# Worker Agent
You are a general-purpose Gas City worker. Execute assigned work carefully and report the result.
EOF
```

Because the city already defaults to `claude`, this city-scoped worker does not
need an `agent.toml` yet. Add one later if you want provider, model, or
directory overrides.

## Instantiating a formula

The whole reason we write formulas is because we want to see them do things. The
simplest way to see your formula do things is to sling it to an agent.

```shell
~/my-city
$ gc sling mayor pancakes --formula
Started workflow mc-btj (formula "pancakes") → mayor
```

That one command handles the whole lifecycle: it compiles the formula,
materializes the root and every step as beads in the store, and routes the
resulting **workflow** to the `mayor` agent. The root bead stays open — blocked
on that finalize step — until every step completes. Sling doesn't prompt the
agent by default; pass `--nudge` if you want the target poked immediately.

The verb differs from Tutorial 01's sling on purpose: there, sling created a
bead from your prompt text and _attached_ a workflow instantiated from the
agent's default formula (`Attached workflow ...`); here the formula itself is
the work, so sling _starts_ the workflow directly.

(With the v1 compiler, slinging a formula creates a **wisp** — an ephemeral
_molecule_, a container bead that holds its steps as children — instead.
You'll still see that vocabulary in older formulas and messages; v2 formulas
start workflows. [Tutorial 06](/tutorials/06-beads) shows how both shapes
land in the bead store.)

Sometimes you want to create the workflow's beads _without_ routing them to
anyone yet — to inspect them first, or to route the work yourself. That's `gc
formula cook`: same compilation, same beads, no routing.

```shell
~/my-city
$ gc formula cook pancakes
Root: mc-79s
Created: 7
pancakes -> mc-79s
pancakes.combine -> mc-265
pancakes.cook -> mc-nia
pancakes.dry -> mc-b8g
pancakes.serve -> mc-k3q
pancakes.wet -> mc-0ez
pancakes.workflow-finalize -> mc-9vb

~/my-city
$ gc sling worker mc-79s
Auto-convoy mc-ygc
Slung mc-79s → worker
```

`Created: 7` is the root plus your five steps plus the finalize step, each with
its own independent bead ID. Slinging the cooked root afterward routes it like
any other bead — and because that's a plain bead sling, you also get an
auto-convoy tracking it (the `--formula` path doesn't create one).

One thing to watch: cook in the scope where the work belongs. Running `gc
formula cook` inside a rig directory creates the beads in that rig's store
(you'd see `mp-` prefixes for `my-project`), and `gc sling` refuses to route a
rig's bead to an agent that reads a different store. We cooked in the city
here, so the city-scoped `worker` can take it.

The cook-versus-sling distinction is just routing. Both materialize every step
as a bead; `gc sling --formula` creates and routes in one motion, while `cook`
leaves the routing to you.

## Variables

Like a function, a formula can be parameterized. You declare the parameters as
variables in a `[vars]` section and reference them as `{{name}}` inside your
formula in step titles, descriptions, and other text fields.

All variables are expanded at cook or sling time — the placeholders in your
formula become concrete values in the resulting beads.

In the simplest case, a variable is just a name with a default value:

```toml
formula = "greeting"

[requires]
formula_compiler = ">=2.0.0"

[vars]
name = "world"

[[steps]]
id = "say-hello"
title = "Say hello to {{name}}"
```

```shell
~/my-city
$ gc formula cook greeting --var name="Alice"
Root: mc-tmf
Created: 3
greeting -> mc-tmf
greeting.say-hello -> mc-oyr
greeting.workflow-finalize -> mc-5x2

~/my-city
$ gc formula cook greeting
Root: mc-h2g
Created: 3
greeting -> mc-h2g
greeting.say-hello -> mc-9qb
greeting.workflow-finalize -> mc-gxo
```

`cook` doesn't echo the substituted titles. To preview the expansion, use `gc
formula show`:

```shell
~/my-city
$ gc formula show greeting --var name="Alice"
Formula: greeting

Variables:
  {{name}}:  (default=world)

Steps (2):
  ├── greeting.say-hello: Say hello to Alice
  └── greeting.workflow-finalize: Finalize workflow [needs: greeting.say-hello]
```

When you write `name = "world"` in `[vars]`, `"world"` is the default value.
Without `--var name`, it falls back to that default. If a variable has no
default and isn't marked `required`, the placeholder stays as the literal text
`{{name}}` in the output — which is usually not what you want, so it's good
practice to always provide either a default or mark it required.

Variables can also have richer definitions — descriptions, required flags,
validation:

- `description` — human-readable explanation
- `required` — must be provided at instantiation time
- `default` — used when the caller doesn't supply a value
- `enum` — restrict to a set of allowed values
- `pattern` — regex validation

Here's a more complete example using those:

```toml
formula = "feature-work"

[requires]
formula_compiler = ">=2.0.0"

[vars.title]
description = "What this feature is about"
required = true

[vars.branch]
description = "Target branch"
default = "main"

[vars.priority]
description = "How urgent is this"
default = "normal"
enum = ["low", "normal", "high", "critical"]

[[steps]]
id = "implement"
title = "Implement {{title}}"
description = "Work on {{title}} against {{branch}} (priority: {{priority}})"
```

You pass variables with `--var`. Here's what the expansion looks like:

```shell
~/my-city
$ gc formula cook feature-work --var title="Auth overhaul" --var branch="develop"
Root: mc-qnf
Created: 3
feature-work -> mc-qnf
feature-work.implement -> mc-35h
feature-work.workflow-finalize -> mc-2fp

~/my-city
$ gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"
Root: mc-d1s
Created: 3
feature-work -> mc-d1s
feature-work.implement -> mc-ej5
feature-work.workflow-finalize -> mc-6gi
```

You can preview the substituted recipe (and the declared variables) with `show`.
Required variables get their own section in the output:

```shell
~/my-city
$ gc formula show feature-work --var title="Auth system"
Formula: feature-work

Required vars:
  {{title}}: What this feature is about

Optional vars:
  {{branch}}: Target branch (default=main)
  {{priority}}: How urgent is this (default=normal)

Steps (2):
  ├── feature-work.implement: Implement Auth system
  └── feature-work.workflow-finalize: Finalize workflow [needs: feature-work.implement]
```

The important thing to know: variables stay as placeholders through the entire
compilation pipeline. They're only substituted when you actually create beads —
via `cook` or `sling`. That's late binding, and it's what makes formulas
reusable across different contexts.

## The dependency graph

You've already seen `needs` in the pancakes example. It gets more interesting as
formulas grow. Steps can fan out — multiple steps depending on the same
predecessor run in parallel:

```toml
[[steps]]
id = "design"
title = "Design the feature"

[[steps]]
id = "implement"
title = "Implement it"
needs = ["design"]

[[steps]]
id = "test"
title = "Test it"
needs = ["implement"]

[[steps]]
id = "review"
title = "Review the PR"
needs = ["implement"]
```

Here `test` and `review` both wait for `implement` but can run in parallel with
each other. The dependency graph is a DAG — the v2 compiler rejects
cycles at compile time.

### Nested steps

When a formula gets large, you can group related steps under a parent:

```toml
[[steps]]
id = "backend"
title = "Backend work"

[[steps.children]]
id = "api"
title = "Build the API"

[[steps.children]]
id = "db"
title = "Set up the database"

[[steps]]
id = "frontend"
title = "Frontend work"
needs = ["backend"]
```

In the compiled recipe, the parent is promoted to an **epic** and its children
are namespaced under it (`backend.api`, `backend.db`). The grouping is
organizational, though — v2 dependencies connect exactly the steps you
name. `needs = ["backend"]` waits for the `backend` step itself, not for its
children. If `frontend` should wait for the sub-steps, list them directly:
`needs = ["api", "db"]`. You always reference other steps by their raw `id`;
the compiler maps those references to the namespaced recipe IDs.

That raw `id` is also why step IDs must be unique across the whole formula,
children included. The namespacing applies to compiled recipe IDs, not to
authoring — two different parent steps can't each have a child called `test`;
validation rejects the duplicate ID.

## Control flow

It's hopefully clear by now that the steps in a formula often execute in
non-sequential, even non-deterministic order. The `needs` field is what sets up
dependencies and allows us to make order out of the chaos. The `children` field
allows us to wrangle that chaos across a lot of steps.

There are several other constructs that control whether a step executes at all,
and if so, how many times.

### Conditions

A step can be conditionally included/excluded based on the value of a variable
specified at sling or cook time.

```toml
[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
```

Conditions use simple expressions: equality (`{{var}} == value` or `{{var}} !=
value`), plus truthiness — a bare `{{var}}` includes the step unless the value
is empty, `false`, `0`, `no`, or `off`, and `!{{var}}` inverts that. The
variable is substituted first, then compared as a string. There's no complex
expression language here — if you need more sophisticated branching, use
multiple variables and conditions across different steps.

You can see conditions take effect with `gc formula show`. Here `deploy-flow`
is a two-step formula: an unconditional `build` step plus the conditional
`deploy` step above.

```shell
~/my-city
$ gc formula show deploy-flow --var env=dev
Formula: deploy-flow

Variables:
  {{env}}:  (default=dev)

Steps (2):
  ├── deploy-flow.build: Build
  └── deploy-flow.workflow-finalize: Finalize workflow [needs: deploy-flow.build]

~/my-city
$ gc formula show deploy-flow --var env=staging
Formula: deploy-flow

Variables:
  {{env}}:  (default=dev)

Steps (3):
  ├── deploy-flow.build: Build
  ├── deploy-flow.deploy: Deploy to staging
  └── deploy-flow.workflow-finalize: Finalize workflow [needs: deploy-flow.build, deploy-flow.deploy]
```

### Loops

A step can wrap a body of sub-steps that execute multiple times:

```toml
[[steps]]
id = "retries"
title = "Attempt deployment"

[steps.loop]
count = 3

[[steps.loop.body]]
id = "attempt"
title = "Try to deploy"
```

Save that as a formula named `retry-deploy` — with the `formula` line and
`[requires]` block, like every formula in this tutorial. The body is expanded
at cook time into three sequential iterations:

```shell
~/my-city
$ gc formula show retry-deploy
Formula: retry-deploy

Steps (4):
  ├── retry-deploy.retries.iter1.attempt: Try to deploy
  ├── retry-deploy.retries.iter2.attempt: Try to deploy [needs: retry-deploy.retries.iter1.attempt]
  ├── retry-deploy.retries.iter3.attempt: Try to deploy [needs: retry-deploy.retries.iter2.attempt]
  └── retry-deploy.workflow-finalize: Finalize workflow [needs: retry-deploy.retries.iter3.attempt]
```

Each iteration is materialized as its own step, chained sequentially. With
`count`, every iteration is baked into the recipe up front — the loop can't end
early. When what you really mean is "keep trying until it works," reach for
the runtime **Check** construct below. The formula language also has an
`until` loop — worth knowing, with one big caveat we'll get to.

An `until` loop expands just one iteration at compile time and records the
condition (plus a required `max` budget) on that iteration. Here's the loop
from a formula named `poll-until` (assembled the same way):

```toml
[[steps]]
id = "poll"
title = "Poll for readiness"

[steps.loop]
until = "probe.status == 'complete'"
max = 5

[[steps.loop.body]]
id = "probe"
title = "Probe the endpoint"
```

```shell
~/my-city
$ gc formula show poll-until
Formula: poll-until

Steps (2):
  ├── poll-until.poll.iter1.probe: Probe the endpoint
  └── poll-until.workflow-finalize: Finalize workflow [needs: poll-until.poll.iter1.probe]
```

Now the caveat: nothing re-runs the body yet. Cooking validates the
condition and stamps it on the iteration, but no component in the current
release — v1 or v2 — reads it back at runtime, so an `until` loop runs
exactly one iteration. Treat it as declared intent, and use Check (next)
when you need actual keep-trying-until-it-passes behavior today. Also note
the condition is _not_ the `{{var}}` syntax from Conditions: it's an
expression over step state, like `probe.status == 'complete'` (did the
`probe` step complete?). The [Loops](/reference/specs/formula-spec-v2#16-loops) section
of the formula spec covers the grammar and this caveat.

A loop takes exactly one of `count`, `until`, or `range`. `range` is a
compile-time cousin of `count`: `range = "1..3"` with `var = "n"` expands the
iterations up front, with `{n}` available for substitution in the body's titles
and descriptions.

### Check

Once a formula is cooked, conditions have been evaluated and loop iterations
have been laid out — all of that is decided up front. But sometimes you need a
decision at runtime: did this step actually work?

Check runs a validation script after the agent finishes a step. If the script
passes, the step is done. If not, the agent tries again.
The check runs after each attempt, while the formula is still executing — it's a
runtime feedback loop, not a compile-time expansion.

```toml
formula = "checked"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "implement"
title = "Implement the feature"

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = "scripts/verify.sh"
timeout = "30s"
```

Check is a v2-only construct: a formula that uses it must declare the
`[requires]` block, like every formula in this tutorial — without it,
compilation fails with an error telling you to add the declaration.

You can see the runtime loop in the compiled recipe:

```shell
~/my-city
$ gc formula show checked
Formula: checked

Steps (4):
  ├── checked.implement.spec: Step spec for Implement the feature (spec)
  ├── checked.implement.iteration.1: Implement the feature
  ├── checked.implement: Implement the feature [needs: checked.implement.iteration.1]
  └── checked.workflow-finalize: Finalize workflow [needs: checked.implement]
```

The compiler unrolled your one step into a little runtime machine: a spec
sidecar that records the original instructions, a first iteration for the agent
to work on, and a control step that keeps the original `implement` ID. When an
iteration closes, Gas City runs `scripts/verify.sh`. If the script exits 0, the
step is done. If it exits non-zero, another iteration is spawned for the agent
— up to `max_attempts` times total. If all attempts fail, the step fails.

### Retry

Check decides pass/fail with a script. Its sibling `[steps.retry]` handles the
simpler case — a step that sometimes fails for transient reasons and should
just be re-dispatched:

```toml
[[steps]]
id = "fetch"
title = "Fetch the dataset"

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

There's no script: when an attempt fails for a transient reason, the controller
dispatches another, up to `max_attempts`. `on_exhausted` decides what happens
when the budget runs out — `"hard_fail"` (the default) fails the step,
`"soft_fail"` records the failure but lets the workflow continue.

Check and retry are two of v2's runtime constructs, and there are more:
`drain` scatters a convoy of work items into per-item runs, and
`on_complete`/`tally` fan out follow-up work over a step's output and aggregate
the results. The tutorial stops here —
the v2 spec's [Runtime section](/reference/specs/formula-spec-v2#3-runtime)
covers the full set.

---

That covers the core of formulas — defining steps, wiring dependencies,
parameterizing with variables, and controlling execution with conditions,
loops, checks, and retries.

## What's next

- **[Formula spec (v2)](/reference/specs/formula-spec-v2)** — the complete surface: every
  top-level key, every step field, and the v2 runtime constructs
- **[Beads](/tutorials/06-beads)** — the universal work primitive underneath
  formulas, sessions, and everything else
- **[Orders](/tutorials/07-orders)** — formulas with scheduling triggers for
  periodic dispatch
