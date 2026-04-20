---
title: Tutorial 05 - Formulas
sidebarTitle: 05 - Formulas
description: Write declarative workflow templates with steps, dependencies, variables, and control flow, then dispatch them to agents.
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

A formula is a TOML file that describes a collection of steps with dependencies,
variables, and optional control flow. To run a formula, you `gc sling` it to an
agent just as you would any other work.

## A simple formula

Formula files use the `.formula.toml` extension and live in your city's
`formulas/` directory. `gc init` already dropped a few in there for you,
including a pancakes recipe:

```toml
# formulas/pancakes.formula.toml
formula = "pancakes"
description = "Make pancakes from scratch"

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
```

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
cooking
mol-do-work
mol-polecat-base
mol-polecat-commit
mol-scoped-work
pancakes
```

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
  └── pancakes.serve: Serve [needs: pancakes.cook]
```

`gc formula show` _compiles_ the formula by arranging the steps and the
dependencies, then displaying to you. In this case, the `(6)` count includes
the implicit root step that wraps the five recipe steps.

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
Slung formula "pancakes" (wisp root mc-194) → mayor
```

This compiles the formula, creates work items in the store, routes them to the
`mayor` agent, and creates a convoy to track the grouped work. Sling handles the
full lifecycle: compile, instantiate, route, convoy, and optionally nudge the
target agent.

When you sling a formula, the result is a **wisp** — a lightweight, ephemeral
bead tree. Only the root bead is materialized in the store, and the steps are
read inline from the compiled recipe. Wisps are garbage-collected after they
close. This is the right choice most of the time.

For long-lived workflows where multiple agents work on different steps
independently, you want a **molecule** instead. A molecule materializes every
step as its own bead, each independently trackable and routable. Use `gc formula
cook` to create a molecule, then sling individual steps wherever they need to
go:

```shell
~/my-project
$ gc formula cook pancakes
Root: mp-2wx
Created: 6
pancakes -> mp-2wx
pancakes.combine -> mp-2wx.3
pancakes.cook -> mp-2wx.4
pancakes.dry -> mp-2wx.1
pancakes.serve -> mp-2wx.5
pancakes.wet -> mp-2wx.2

~/my-project
$ gc sling worker mp-2wx
Auto-convoy mp-w0n
Slung mp-2wx → worker
```

Cook inside the rig whose agents will work on it. That keeps the molecule bead
prefix aligned with `my-project` so a rig-local worker can pick it up without
crossing scope boundaries. The distinction between wisps and molecules is just
about how much state gets materialized — wisps are light and fast, molecules
give you per-step visibility and routing.

## Variables

Like a function, a formula can be parameterized. You declare the parameters as
variables in a `[vars]` section and reference them as `{{name}}` inside your
formula in step titles, descriptions, and other text fields.

All variables are expanded at cook or sling time — the placeholders in your
formula become concrete values in the resulting beads.

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
Root: mc-8he
Created: 2
greeting -> mc-8he
greeting.say-hello -> mc-8he.1

~/my-city
$ gc formula cook greeting
Root: mc-kza
Created: 2
greeting -> mc-kza
greeting.say-hello -> mc-kza.1
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
  └── greeting.say-hello: Say hello to Alice
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
Root: mc-iqy
Created: 2
feature-work -> mc-iqy
feature-work.implement -> mc-iqy.1

~/my-city
$ gc formula cook feature-work --var title="Auth overhaul" --var priority="critical"
Root: mc-jrz
Created: 2
feature-work -> mc-jrz
feature-work.implement -> mc-jrz.1
```

You can preview the substituted recipe (and the declared variables) with `show`:

```shell
~/my-city
$ gc formula show feature-work --var title="Auth system"
Formula: feature-work

Variables:
  {{title}}: What this feature is about (required)
  {{branch}}: Target branch (default=main)
  {{priority}}: How urgent is this (default=normal)

Steps (2):
  └── feature-work.implement: Implement Auth system
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
each other. The dependency graph is a DAG — cycles are rejected at compile time.

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

The parent acts as a container — `frontend` won't start until all of `backend`'s
children are done. Children are namespaced under their parent in the compiled
recipe (`backend.api`, `backend.db`), so IDs stay unique. The parent gives you a
single thing to depend on (`needs = ["backend"]`) instead of listing every
individual child.

You could achieve the same dependency structure with flat steps and explicit
`needs` — make `api` and `db` top-level, then have `frontend` need both.
Children are a convenience for large formulas where you'd otherwise be
maintaining long `needs` lists. If `backend` has ten sub-steps, a single `needs
= ["backend"]` is cleaner than `needs = ["api", "db", "schema", "seed",
"migrate", ...]`. Children also give you namespacing — two different parent
steps can each have a child called `test` without collision.

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

Conditions use simple equality expressions: `{{var}} == value` or `{{var}} !=
value`. The variable is substituted first, then compared as a string. There's no
complex expression language here — if you need more sophisticated branching, use
multiple variables and conditions across different steps.

You can see conditions take effect with `gc formula show`:

```shell
~/my-city
$ gc formula show deploy-flow --var env=dev
Steps (2):
  └── deploy-flow.build: Build

~/my-city
$ gc formula show deploy-flow --var env=staging
Steps (3):
  ├── deploy-flow.build: Build
  └── deploy-flow.deploy: Deploy to staging
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

The body is expanded at cook time into three sequential iterations:

```shell
~/my-city
$ gc formula show retry-deploy
Steps (4):
  ├── retry-deploy.retries.iter1.attempt: Try to deploy
  ├── retry-deploy.retries.iter2.attempt: Try to deploy [needs: retry-deploy.retries.iter1.attempt]
  └── retry-deploy.retries.iter3.attempt: Try to deploy [needs: retry-deploy.retries.iter2.attempt]
```

Each iteration is materialized as its own step. There's no way to break out
early — all iterations are baked into the recipe up front.

### Check

Once a formula is cooked, conditions have been evaluated and loops have been
expanded — all of that is decided up front. But sometimes you need a decision at
runtime: did this step actually work?

Check runs a validation script after the agent finishes a step. If the script
passes, the step is done. If not, the agent tries again.
The check runs after each attempt, while the formula is still executing — it's a
runtime feedback loop, not a compile-time expansion.

```toml
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

Here's what happens: the agent works on "implement." When it finishes, Gas City
runs `scripts/verify.sh` to check the result. If the script exits 0, the step is
done. If it exits non-zero, the agent gets another shot — up to `max_attempts`
times total. If all attempts fail, the step fails.

---

That covers the core of formulas — defining steps, wiring dependencies,
parameterizing with variables, and controlling execution with conditions, loops,
and Check.

## What's next

- **[Beads](/tutorials/06-beads)** — the universal work primitive underneath
  formulas, sessions, and everything else
- **[Orders](/tutorials/07-orders)** — formulas with scheduling triggers for
  periodic dispatch
