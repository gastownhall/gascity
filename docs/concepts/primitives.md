---
title: Primitives Reference
description: A deeper, per-concept reference for the nine building blocks of Gas City — Session, Beads Store, Event Bus, Config, Prompt Templates, Messaging, Formulas, Dispatch, and Health Patrol — each with a copy-pasteable example.
---

The [Architecture Overview](/concepts/architecture-overview) gives you the top-down mental model. This page is the bottom-up companion: a reference you can dip into for any single building block once you know where it sits in the whole.

Gas City is built from **nine concepts** — *five* irreducible *primitives* and *four derived mechanisms* composed from them.

Everything `gc` does, from slinging a single task to running a fleet of pooled agents, is some combination of these nine. Read this page after the overview, or jump straight to the concept you need.

<Note>
This is reference material, not a tutorial. Each section explains what a concept is, what it does for you, and shows one snippet you can copy-paste. For the guided, end-to-end path, start with the [Tutorials](/tutorials/index).
</Note>

## The five primitives

A **primitive** is irreducible: you cannot rebuild it out of the other concepts, and removing it would make whole classes of orchestration impossible. There are exactly five.

### Session

A **session** is a single running instance of an agent — a live process (by default a `tmux` pane) that Gas City can start, stop, prompt, and observe, regardless of which provider backs it.

Sessions are deliberately **disposable**. They come and go; the work they were doing survives them, because work lives in the [Beads Store](#beads-store), not in the process. This is what lets the [controller](/concepts/architecture-overview#controller) restart a stalled agent, replace a crashed one, or **adopt** a still-running one after the controller itself restarts — without losing anything.

A session's [*identity*](/guides/capabilities-for-coding-agent-users#identity) is stable even though the process is not. The same agent always resolves to the same session name, so the controller can find, re-attach to, or replace it across restarts.

**What it does for you:** you never manage processes by hand:

- You declare agents in [config](#config)
- The controller spawns, supervises, and reaps sessions to match
- When you need to look inside one, you address it by name (peek it, attach to its session, etc.).

```shell
# List the live sessions in your city
gc session list

# Peek at what an agent is currently doing
gc session peek mayor --lines 20

# Attach to a session interactively (detach with the provider's keybinding)
gc session attach mayor
```

> Under the hood, the runtime boundary is a `Provider` interface with implementations for tmux (production), subprocess (remote), exec (script), Kubernetes, and a fake provider for tests. They all expose the same start/stop/prompt/observe surface, so nothing above the session layer cares which one is in use.

### Beads Store

The **Beads Store** is the universal persistence substrate.

The rule is absolute: *everything is a **bead***.

A bead is one row in one store, and tasks, mail messages, molecules, convoys, and epics are all beads that differ only by their `type` field.

A bead is a single unit of work with:

- an ID
- a title
- a status (`open` → `in_progress` → `closed`)
- a type
- an optional assignee
- parent/child links
- dependencies (`needs`)
- a description
- labels.

The store offers one small interface over all of them: create, read, update, close, list, query by label, and walk parent/child relationships.

**What it does for you:** it is the single source of truth for *what work exists and what state it's in*. Because every piece of durable state flows through this one interface, the system converges to correct outcomes even as sessions churn — kill every agent and the work is still there, waiting to be picked up again.

```shell
# Create a task bead
bd create --title "Add a health endpoint" --type task --priority 2

# Find work that is ready (open, unblocked) and inspect one bead
bd ready
bd show <bead-id>

# Claim it, then close it when done
bd update <bead-id> --claim
bd close <bead-id> --reason "Shipped in v1.1.0"
```

> By default the store is backed by Dolt through the `bd` CLI, with one Dolt server per city. The city and its rigs share that server but stay logically separate by `issue_prefix`. See [Beads Storage Topology](/reference/internal/beads-topology) for where the files live.

<Tip>
A **label** is just a string tag on a bead (e.g. `pool:dog`, `rig:tower-of-hanoi`). Labels drive pool dispatch and rig scoping, and you can query by them with `bd list --label <name>`. They are how higher-level mechanisms route and group work without any new storage.
</Tip>

### Event Bus

The **event bus** is the universal observation substrate: an append-only pub/sub log of everything that happens in the system.

Events are immutable and carry a monotonically increasing sequence number, so observers can replay from any point and never miss or reorder an event.

It has two tiers:

- **critical** events on a bounded queue, for infrastructure that must not drop anything;
- **optional**, fire-and-forget events for audit and visibility.

Other parts of the system *watch* the bus reactively instead of polling it, which is what keeps the [controller](/concepts/architecture-overview#controller) responsive without busy-looping.

**What it does for you:** it is how you (and the rest of the system) see what is happening as it happens — a bead being created, a convoy closing, a session restarting. It is the backbone behind every `--watch`/`--follow` view.

```shell
# Show recent events
gc events

# Filter by type and time window
gc events --type bead.created --since 1h

# Follow new events live (Ctrl-C to stop)
gc events --follow --type convoy.closed
```

### Config

**Config** is TOML with *progressive activation*: capabilities switch on simply because a section is present, not because you flipped a feature flag. For instance, an empty `city.toml` gives you the bare minimum; adding sections unlocks more.

Config is assembled from several sources:

- **`city.toml`** — the root config file at the city directory root; the entry point the controller reads first and the file you always edit.
- **Fragment files** — additional TOML files pulled in via an `include` field in `city.toml`.
- **`pack.toml`** — reusable configuration directories (packs) that define agents and prompts.
- **`agents/<name>/agent.toml`** — one file per agent; optional per-agent overrides (provider, rig scope, model, pool size). Lives under the `agents/` directory of *any* pack — the city root (yes, a city is also a pack, called the root pack), an imported pack, or a rig-level import.
- **`formulas/*.toml`** — one file per formula; can live at the city root (`<city-root>/formulas/`) or inside a pack (`<pack-dir>/formulas/`).
- **`orders/*.toml`** — one file per order (a trigger — `cooldown`, `cron`, `condition`, `event`, or `manual` — plus either a formula to instantiate or an exec command to run); same placement rules as formula files.

These files serve distinct purposes:

- **`city.toml`** is the *operational* config: which agents run, how many, on which provider, health thresholds, mail settings, etc. It is the desired state the [controller](/concepts/architecture-overview#controller) reconciles toward — change it and the controller notices (it watches the file) and drives reality to match, no restart required for most changes.
- **`pack.toml`** is the *behavioral* config: what agents do — their prompts. Packs are reusable and can be shared across cities.
- **`agents/<name>/agent.toml`** is the *per-agent* config: how one agent diverges from the defaults — its provider, rig scope, model, or pool size. Optional; omit it and the agent inherits everything.
- **Formula and order files** are the *workflow* config: the step-by-step definitions that instantiating a molecule or firing an order consumes at runtime. They can live at the city root or inside a pack.

**What it does for you:** `city.toml` is where you say *how* your city runs; packs and their `agents/<name>/agent.toml` files are where you define *which* agents exist and *what* each one does; formula and order files are where you define the workflows they run. Together they form the full picture of your city's desired state, with no separate state file to maintain.

A minimal two-agent city declares each agent as its own directory under
`agents/`. Scaffold them with `gc agent add`:

```shell
gc agent add --name mayor
gc agent add --name worker
```

Each command creates an `agents/<name>/` directory with a starter prompt. The
result is a tree where `city.toml` holds the operational defaults the agents
inherit:

```text
bright-lights/
├── city.toml          # operational config (below)
└── agents/            # behavioral config — one directory per agent
    ├── mayor/
    │   └── prompt.template.md
    └── worker/
        └── prompt.template.md
```

```toml
# city.toml — operational config
[workspace]
name = "bright-lights"
provider = "claude"   # default provider; an agent's agent.toml can override it
```

Both `mayor` and `worker` run on `claude` because they inherit the workspace
default; neither needs an `agent.toml` until it diverges from it.


### Prompt Templates

A **prompt template** is a Go `text/template` written in Markdown that defines *what an agent does*.

It is the entire behavioral specification for a session — the SDK contains **zero** hardcoded roles, so a "mayor" or a "reviewer" is nothing more than the prompt you wrote for it.

Templates are rendered at spawn time with context about:

- the city
- the agent
- the rig
- git metadata.

That rendered text is handed to the session as its priming prompt.

**What it does for you:** this is where you express *intent*. Instead of encoding role logic in code (which Gas City forbids — see [Zero Framework Cognition](#a-note-on-design-principles)), you write a sentence and let the model act on it. Want a different role? Write a different prompt.

```markdown
<!-- agents/reviewer/prompt.template.md -->
# Reviewer

You are the reviewer for **{{ .RigName }}** (working in `{{ .WorkDir }}`).

Check your hook for assigned work, review the change, and leave findings.
Find your pool work with: `{{ .WorkQuery }}`

When you are done, close the bead with a one-line summary.
```

Common template variables include `{{ .AgentName }}`, `{{ .RigName }}`, `{{ .RigRoot }}`, `{{ .WorkDir }}`, `{{ .WorkQuery }}`, `{{ .IssuePrefix }}`, `{{ .CityRoot }}`, and `{{ .DefaultBranch }}`.

Prompt file discovery prefers `prompt.template.md` (with `prompt.md` and `prompt.md.tmpl` accepted for compatibility).

## The four derived mechanisms

A **derived mechanism** is one that is *composed* from the primitives above — it needs no new storage, no new runtime, no new infrastructure. Each one below is just a particular combination of Session, Beads Store, Event Bus, and Config.

### Messaging

**Messaging** is how agents talk to each other. It is two things, neither of which is a new primitive:

- **Mail** is a bead with `type: message`. An agent's inbox is a query for open message beads addressed to it; archiving a message is closing that bead. Mail is therefore *just the Beads Store*. Mail is **not instantaneous**: the recipient reads it the next time the `UserPromptSubmit` (or equivalent) agent provider's hook fires (which runs `gc mail check --inject` to inject pending messages into the agent's next prompt). If you need the agent to act before that, pair a mail with a nudge.
- **Nudge** is text typed directly into a running agent's session to prod it. It is fire-and-forget and uses *just the Session* layer.

**What it does for you:** durable, queryable inter-agent communication (mail) plus a lightweight "wake up and re-check" poke (nudge) — without learning any new concept. Mail persists and survives restarts; a nudge does not.

```shell
# Mail: durable, shows up in the recipient's inbox
gc mail send mayor -s "Review needed" -m "Please look at the auth changes"
gc mail inbox mayor

# Nudge: ephemeral, prods a live session to act now
gc session nudge mayor "Check mail and hook status, then act accordingly"
```

### Formulas & Molecules

A **formula** is a reusable, parameterized *method* — how a piece of multi-step work should be done — written as TOML. It is not the work itself: a bead is the unit of work, and a convoy is a graph of related work. Applying a formula is what produces that work.

A **molecule** is a formula *instantiated at runtime* — the work a formula produces: a root bead plus one bead per step in the Beads Store, with progress tracked by closing those beads.

A **wisp** is an ephemeral molecule that auto-closes and is garbage-collected after a configurable time-to-live (TTL).

Formulas compile under one of two contracts, and the contract determines the shape of those beads. Under the **v1 compiler** — the default when a formula declares nothing — the molecule is a tree: a root bead with the step beads attached as parent-child children.

Under **formulas v2** — which a formula opts into by declaring `[requires] formula_compiler = ">=2.0.0"`, and which the host enables by default via the `[daemon] formula_v2` switch — the compiler emits a flat graph instead: a workflow root, step beads linked only by blocking dependencies (no parent-child edges), and controller-owned control beads, ending in a `workflow-finalize` step the root blocks on until the whole graph completes. The base constructs — steps, `needs`, conditions, loops, variables — are common to both contracts; the v2-only constructs and the full opt-in details live in [Choosing a Compiler Contract](/guides/understanding-formulas#choosing-a-compiler-contract).

Instantiating a molecule from a formula is pure composition from the primitives: [Config](#config) supplies the formula definition (a `formulas/*.toml` file), and the [Beads Store](#beads-store) holds the resulting root bead and step beads. Steps declare dependencies on each other with `needs`, so the store's readiness queries naturally schedule them in the right order.

**What it does for you:** instead of slinging work one piece at a time, you describe a whole workflow once and dispatch it as a unit. The steps fan out and join automatically based on their dependencies.

```toml
# formulas/pancakes.toml
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
description = "Fold wet into dry. Do not overmix."
needs = ["dry", "wet"]
```

```shell
# See available formulas, then dispatch one to an agent
gc formula list
gc sling worker pancakes --formula
```

Here `dry` and `wet` have no dependencies and can run in parallel; `combine` waits for both. See [Tutorial 05](/tutorials/05-formulas) for the full walkthrough.

### Dispatch (Sling)

**Dispatch** — invoked with `gc sling` — is the routing mechanism that turns "do this work" into a running agent.

It composes the primitives end to end:

- find or spawn an agent (Session)
- select a formula if one applies (Config)
- create the work bead or molecule (Beads Store)
- hook it to the agent (Beads Store)
- nudge the session (Session)
- optionally create a convoy to group related work (Beads Store)
- log an event (Event Bus).

**What it does for you:** it is the single command that gets work *moving*. Sling a plain description for a one-off task, or sling a formula to kick off a whole molecule. Either way the work lands in the store, gets routed, and a session picks it up on the [controller](/concepts/architecture-overview#controller)'s next tick.

```shell
# Sling a single task to an agent
gc sling claude "Create a script that prints hello world"

# Sling a formula — starts a multi-step workflow (pancakes declares v2)
gc sling worker pancakes --formula
```

<Tip>
A **convoy** is a container bead that groups related work as one tracked batch. Membership is recorded as `tracks` dependency edges from the convoy to each member — tracking never changes a member's parent and blocks nothing. Dispatch can create a convoy for you so a fan-out of related tasks reports progress as a unit.
</Tip>

### Health Patrol

**Health patrol** keeps the fleet alive.

Like [Dispatch](#dispatch-sling) it composes the primitives end to end:

- probe sessions for liveness (Session)
- compare what it finds against thresholds (Config)
- publish stalls to the [Event Bus](#event-bus)
- restart unhealthy sessions with backoff (Session).

The supervision model follows the Erlang/OTP "let it crash, then restart" pattern.

Crucially, the [**controller**](/concepts/architecture-overview#controller) drives all of this on its own — no user-configured agent role is required for the infrastructure to stay healthy. If removing an agent's `agents/<name>/` directory would break supervision, that would be a bug.

**What it does for you:** stalled and crashed sessions recover automatically. You declare the health thresholds in config; the controller does the probing, restarting, and backoff. When you want to check the system's health yourself:

```shell
# Check workspace health (add --fix to attempt automatic recovery)
gc doctor

# Check the beads provider specifically
gc beads health
```

## A note on design principles

These nine concepts are not an arbitrary list — they are the *minimal* set that makes multi-agent orchestration possible. Three rules keep the boundary honest:

- **Atomicity.** If a capability can be decomposed into the five primitives, it is a derived mechanism, not a new primitive. That is why Messaging, Formulas, Dispatch, and Health Patrol are *composed*, not built.
- **Bitter Lesson.** Every primitive must become *more* useful as models improve, never less. Gas City adds no heuristics or decision trees that a better model would outgrow.
- **ZFC (Zero Framework Cognition).** Go handles transport, not reasoning. If a line of Go contains a judgment call, it is a violation — the decision belongs in a [prompt template](#prompt-templates), not in code.

This is why all role behavior is configuration and the SDK has *zero* hardcoded roles: the model is the intelligence, and these nine concepts are only the plumbing it acts through.

## Where to go next

- [Architecture Overview](/concepts/architecture-overview) — the top-down view these primitives compose into.
- [Tutorials](/tutorials/index) — the guided, end-to-end path through every concept above.
- [Tutorial 06: Beads](/tutorials/06-beads) — go deeper on the Beads Store that underpins everything here.
- [Beads Storage Topology](/reference/internal/beads-topology) — how a city and its rigs share one store under the hood.
- [Reference](/reference/index) — command, config, formula, and provider lookup.
