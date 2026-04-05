---
title: Tutorial 04 - Formulas
description: Write declarative workflow templates with steps, dependencies, variables, and control flow, then dispatch them to agents.
---

# Tutorial 04: Formulas

So far you've been giving agents work one piece at a time — `gc sling helper "do this thing"`. That works, but real workflows have multiple steps with dependencies between them. This tutorial shows how to define multi-step workflows as *formulas* and dispatch them as a unit.

We'll pick up where Tutorial 03 left off. You should have `my-city` running with `my-project` and `my-api` rigged, and agents for `mayor`, `helper`, `worker`, and `reviewer`.

One of the main reasons agent orchestration engines like Gas City exist is to coordinate various pieces of work without a human or shell script trying to feed the right prompts at the right times. In Gas City, we use *formulas* to write down all of the things we want to happen, and then hand them off to the agent to do our bidding.

A formula describes the steps that need to take place, but it's not *quite* step by step instructions. As with many things in life, some things need to happen one after another, but a lot of things can happen in parallel. Parallelism is generally good, as it scales well to machines, and can shorten the path from beginning to end.

A formula is a TOML file that describes a collection of steps with dependencies, variables, and optional control flow. To run a formula, you `gc sling` it to an agent just as you would any other work. 

## A simple formula

Formula files use the `.formula.toml` extension and live in your city's `formulas/` directory


```toml
# formulas/pancakes.formula.toml
formula = "pancakes"

[[steps]]
id = "dry"
title = "Mix dry ingredients"

[[steps]]
id = "wet"
title = "Mix wet ingredients"

[[steps]]
id = "combine"
title = "Combine batter"
needs = ["dry", "wet"]

[[steps]]
id = "cook"
title = "Cook pancakes"
needs = ["combine"]
```

The `needs` field declares dependencies between sibling steps. 
- `dry` and `wet` can run in parallel 
- `combine` needs both `dry` and `wet` to complete before it runs, 
- `cook` waits for `combine` to complete before it runs.

Once all of these steps are complete, the formula is done. 

Without these `needs` declarations, everything could happen at any time, which would yield a messy kitchen, not a stack of delicious pancakes.

## Inspecting formulas

The `formulas` directory contains many formula files. While you can `ls` the directory, it's more interesting to ask `gc` to enumerate them for you.

```shell
~/my-city
$ gc formula list
NAME              STEPS  SOURCE
pancakes          4      formulas/
mol-feature       5      packs/gastown/formulas/
health-check      2      packs/maintenance/formulas/
```

To see the compiled recipe for a specific formula:

```shell
~/my-city
$ gc formula show pancakes
Formula: pancakes
Steps (4):
  ├── pancakes.dry: Mix dry ingredients [needs: —]
  ├── pancakes.wet: Mix wet ingredients [needs: —]
  ├── pancakes.combine: Combine batter [needs: dry, wet]
  └── pancakes.cook: Cook pancakes [needs: combine]
```

`gc formula show` compiles the formula through the full pipeline and displays the step tree with types, priorities, and dependency edges.

## Instantiating a formula

The whole reason we write formulas is because we want to see them do things. The simplest way to see your formula do things is to sling it to an agent.
```shell
~/my-city
$ gc sling mayor pancakes --formula
Dispatched wisp gc-20 (pancakes) → mayor
```

This compiles the formula, creates work items in the store, routes them to the `mayor` agent, and creates a convoy to track the grouped work. Sling handles the full lifecycle: compile, instantiate, route, convoy, and optionally nudge the target agent.

When you sling a formula, the result is a **wisp** — a lightweight, ephemeral bead tree. Only the root bead is materialized in the store, and the steps are read inline from the compiled recipe. Wisps are garbage-collected after they close. This is the right choice most of the time.

For long-lived workflows where multiple agents work on different steps independently, you want a **molecule** instead. A molecule materializes every step as its own bead, each independently trackable and routable. Use `gc formula cook` to create a molecule, then sling individual steps wherever they need to go:

```shell
~/my-city
$ gc formula cook pancakes
Cooked formula 'pancakes' → root gc-10 (4 steps)
  pancakes.dry    → gc-11
  pancakes.wet    → gc-12
  pancakes.combine → gc-13
  pancakes.cook   → gc-14

~/my-city
$ gc sling alice gc-10
Dispatched gc-10 → alice

~/my-city
$ gc sling bob gc-10
Dispatched gc-10 → bob
```

Cook once, sling to different agents. The distinction between wisps and molecules is just about how much state gets materialized — wisps are light and fast, molecules give you per-step visibility and routing.

## Variables

Like a function, a formula can be parameterized. You declare the parameters as variables in a `[vars]` section and reference them as `{{name}}` inside your formula in step titles, descriptions, and other text fields.

All variables are expanded at cook or sling time — the placeholders in your formula become concrete values in the resulting beads.

In the simplest case, a variable is just a name with a default value:

```toml
formula = "greeting"

[vars]
name = "world"

[[steps]]
id = "say-hello"
title = "Say hello to {{name}}"
```

```shell
~/my-city
$ gc formula cook greeting --var name="Alice"
Cooked formula 'greeting' → root gc-30 (1 step)
  greeting.say-hello → gc-31: Say hello to Alice

~/my-city
$ gc formula cook greeting
Cooked formula 'greeting' → root gc-32 (1 step)
  greeting.say-hello → gc-33: Say hello to world
```

When you write `name = "world"` in `[vars]`, `"world"` is the default value. Without `--var name`, it falls back to that default. If a variable has no default and isn't marked `required`, the placeholder stays as the literal text `{{name}}` in the output — which is usually not what you want, so it's good practice to always provide either a default or mark it required.

Variables can also have richer definitions — descriptions, required flags, validation:

- `description` — human-readable explanation
- `required` — must be provided at instantiation time
- `default` — used when the caller doesn't supply a value
- `enum` — restrict to a set of allowed values
- `pattern` — regex validation

Here's a more complete example using those:

```toml
formula = "feature-work"

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
Cooked formula 'feature-work' → root gc-25 (1 step)
  feature-work.implement → gc-26: Implement Auth overhaul

~/my-city
$ gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"
Cooked formula 'feature-work' → root gc-27 (1 step)
  feature-work.implement → gc-28: Implement Auth overhaul
```

You can also preview the expansion without creating any beads using `show`:

```shell
~/my-city
$ gc formula show feature-work --var title="Auth system"
Formula: feature-work
Steps (1):
  └── feature-work.implement: Implement Auth system [needs: —]
```

The important thing to know: variables stay as placeholders through the entire compilation pipeline. They're only substituted when you actually create beads — via `cook` or `sling`. That's late binding, and it's what makes formulas reusable across different contexts.

## The dependency graph

You've already seen `needs` in the pancakes example. It gets more interesting as formulas grow. Steps can fan out — multiple steps depending on the same predecessor run in parallel:

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

Here `test` and `review` both wait for `implement` but can run in parallel with each other. The dependency graph is a DAG — cycles are rejected at compile time.

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

The parent acts as a container — `frontend` won't start until all of `backend`'s children are done. Children are namespaced under their parent in the compiled recipe (`backend.api`, `backend.db`), so IDs stay unique. The parent gives you a single thing to depend on (`needs = ["backend"]`) instead of listing every individual child.

You could achieve the same dependency structure with flat steps and explicit `needs` — make `api` and `db` top-level, then have `frontend` need both. Children are a convenience for large formulas where you'd otherwise be maintaining long `needs` lists. If `backend` has ten sub-steps, a single `needs = ["backend"]` is cleaner than `needs = ["api", "db", "schema", "seed", "migrate", ...]`. Children also give you namespacing — two different parent steps can each have a child called `test` without collision.

## Control flow

It's hopefully clear by now that the steps in a formula often execute in non-sequential, even non-deterministic order. The `needs` field is what sets up dependencies and allows us to make order out of the chaos. The `children` field allows us to wrangle that chaos across a lot of steps.

There are several other constructs that control whether a step executes at all, and if so, how many times.

### Conditions

A step can be conditionally included/excluded based on the value of a variable specified at sling or cook time.

```toml
[[steps]]
id = "deploy"
title = "Deploy to staging"
condition = "{{env}} == staging"
```

Conditions are evaluated when the formula is cooked (either explicitly with `gc formula cook` or implicitly with `gc sling`). If the condition is false, the step is removed from the recipe entirely.

Conditions use simple equality expressions: `{{var}} == value` or `{{var}} != value`. The variable is substituted first, then compared as a string. There's no complex expression language here — if you need more sophisticated branching, use multiple variables and conditions across different steps.

### Loops

A single step can execute multiple times:

```toml
[[steps]]
id = "retry"
title = "Attempt deployment"
loop = { count = 3 }
```

This expands into three copies of the step at cook time. There's no way to break out early — all iterations are baked into the recipe up front. If you need "try until it works" behavior, that's what Ralph is for.

### Ralph

Once a formula is cooked, conditions have been evaluated and loops have been expanded — all of that is decided up front. But sometimes you need a decision at runtime: did this step actually work?

That's what Ralph does. After the agent finishes a step, Gas City runs a check script. If the check passes, the step is done. If not, the agent tries again. The check runs after each attempt, while the formula is still executing — it's a runtime feedback loop, not a compile-time expansion.

```toml
[[steps]]
id = "implement"
title = "Implement the feature"

[steps.ralph]
max_attempts = 2

[steps.ralph.check]
mode = "exec"
path = "scripts/verify.sh"
timeout = "30s"
```

Here's what happens: the agent works on "implement." When it finishes, Gas City runs `scripts/verify.sh` to check the result. If the script exits 0, the step is done. If it exits non-zero, the agent gets another shot — up to `max_attempts` times total. If all attempts fail, the step fails.

---

That covers the core of formulas — defining steps, wiring dependencies, parameterizing with variables, and controlling execution with conditions, loops, and Ralph.

## What's next

- **[Beads](05-beads.md)** — the universal work primitive underneath formulas, sessions, and everything else
- **Orders** — formulas with scheduling gates for periodic dispatch (coming soon)

---

<!--
BONEYARD — draft material for future sections. Not part of the published tutorial.

### Gates

Gates are async wait conditions — a step that blocks until something external happens:

```toml
[[steps]]
id = "wait-for-ci"
title = "Wait for CI to pass"
gate = { type = "event", on = "ci.passed", timeout = "30m" }
```

## Inheritance

Formulas can extend other formulas:

```toml
formula = "feature-with-tests"
extends = ["feature-base"]

[[steps]]
id = "test"
title = "Run test suite"
needs = ["implement"]
```

The child inherits all steps from the parent and can add or override steps. Good for creating variations without duplicating the common parts.

## Late-bound attachment

One of the more interesting patterns: you can attach a formula to an existing bead at dispatch time with the `--on` flag:

```bash
gc sling mayor BL-42 --on feature-work --var title="Auth system"
```

This creates a wisp from `feature-work` and attaches it as a child of `BL-42`. The original bead gains a blocking dependency on the wisp — it can't close until the formula work completes.

This is runtime composition. An agent receives a bead, decides it needs a multi-step workflow, and attaches one on the fly. The formula doesn't have to be known ahead of time.

## Orders: scheduled formulas

Orders are formulas with gate conditions for periodic or event-driven dispatch. They live in `orders/` directories:

```toml
# orders/health-check/order.toml
[order]
description = "Periodic health check"
formula = "health-check"
pool = "mayor"
gate = "cooldown"
interval = "30m"
```

This tells the controller: every 30 minutes, instantiate the `health-check` formula and route it to the `mayor`.

Gate types:

- **cooldown** — run at most every `interval`
- **cron** — run on a cron schedule
- **condition** — run when a shell command exits 0
- **event** — run when a specific event fires
- **manual** — only run via `gc order run`

Orders are how Gas City drives ongoing operational work — sweeps, patrols, health checks, digest generation — without anyone having to dispatch each one by hand.

```
$ gc order list
NAME            GATE       INTERVAL  POOL     ENABLED
health-check    cooldown   30m       mayor    yes
gate-sweep      cron       —         dog      yes
orphan-sweep    cooldown   1h        dog      yes

$ gc order check
NAME            DUE    LAST RUN
health-check    yes    32m ago
gate-sweep      no     5m ago
orphan-sweep    yes    1h ago

$ gc order run health-check
Dispatched order 'health-check' → mayor
```

## Convoys: grouping work

When you dispatch a formula via `gc sling --formula`, a convoy is automatically created to group the resulting work. Convoys are coordination beads that track related beads and their dependencies.

```
$ gc convoy list
ID      NAME        BEADS  OPEN  CLOSED
gc-30   auth-work   5      3     2

$ gc convoy status gc-30
Convoy: auth-work (gc-30)
  gc-31  Implement auth    open
  gc-32  Write tests       open
  gc-33  Review PR         open
  gc-34  Design auth       closed
  gc-35  Load context      closed
```

You can also create convoys explicitly:

```bash
gc convoy create sprint-42 BL-1 BL-2 BL-3
```

The convoy doesn't close until all its member beads are done.

## The compilation pipeline

When you run `gc formula show` or `gc formula cook`, the formula passes through a 12-stage compilation pipeline:

1. Load the TOML
2. Resolve inheritance (`extends` chains)
3. Apply control flow (loops, gates)
4. Apply advice rules (before/after/around)
5. Apply inline expansions
6. Apply compose expansions
7. Apply aspects
8. Filter steps by condition
9. Materialize expansion formulas
10. Expand retry specifications
11. Expand Ralph patterns
12. Convert to recipe (flatten, namespace, order)

The output is a **recipe** — a flattened, ordered list of steps with fully resolved dependency edges and namespaced IDs. Variables are still placeholders at this point; they get substituted when the recipe is instantiated into beads.

You don't need to think about the pipeline to use formulas. But it helps to know that compilation is deterministic — the same formula with the same variables always produces the same recipe.

## Putting it together

A minimal formula-driven workflow:

```toml
# city.toml
[workspace]
name = "my-city"
provider = "claude"

[formulas]
dir = "formulas"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
```

```toml
# formulas/review.formula.toml
formula = "review"

[vars.pr]
description = "PR number or URL"
required = true

[[steps]]
id = "checkout"
title = "Check out PR {{pr}}"

[[steps]]
id = "review"
title = "Review changes in {{pr}}"
needs = ["checkout"]

[[steps]]
id = "comment"
title = "Post review comments"
needs = ["review"]
```

```
$ gc start
City 'my-city' started

$ gc sling worker review --formula --var pr="#42"
Dispatched wisp gc-10 (review) → worker
```

The worker gets a three-step workflow for reviewing PR #42. Each step has clear dependencies, the agent works through them in order, and the wisp closes when the last step is done.

That's formulas — declarative workflow templates, compiled into recipes, instantiated as beads, dispatched to agents. The same machinery scales from a three-step code review to a multi-agent orchestration pipeline with conditional steps, retry loops, and scheduled dispatch.
-->
