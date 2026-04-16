---
title: "Formula V2 (Beta)"
description: "Graph-first formulas with first-class retries, scope lifecycles, runtime fanout, and automatic workflow finalization."
---

> **Beta — opt-in while the feature stabilizes.**
> Formula v2 is shipping in production (Gastown uses it), but it is
> explicitly gated behind `[daemon] formula_v2 = true` and some pieces are
> still evolving. Before you turn it on, read this whole guide — v2 changes
> the worker contract, auto-injects a new implicit agent, and swaps the
> default prompt template for every agent in the city. If you already have
> custom workers, you will likely need to update how they close beads.

Formula v2 is an opt-in compilation and runtime contract for formulas that
need more than a simple step sequence. It turns a formula into a durable
directed acyclic graph with automatic finalization, scope lifecycles,
transient retries, runtime fanout, and session affinity. The legacy v1
contract still works and remains the default.

This guide assumes you already know v1 formulas — read
[Tutorial 05 — Formulas](/tutorials/05-formulas) first if you don't.

## When to reach for formula v2

Formula v2 is the right tool when any of these match your workflow:

- **Parallel fanout with synthesis.** Multiple review legs, design
  explorations, or analysis tasks that run concurrently and feed a
  synthesis step. V2's DAG model lets you express diamond and
  multi-parent dependencies cleanly.
- **Scoped worktree lifecycles.** Setup → implement → self-review →
  submit, with a cleanup step that must run even when the body fails.
  V2's scope model makes this a first-class pattern.
- **Transient retries on flaky work.** Provider rate limits, network
  glitches, or worker-specific flakes where rerunning on a fresh pool
  session usually succeeds. `[steps.retry]` classifies failures as
  transient or hard and retries accordingly.
- **Runtime fanout over step output.** A step produces N items and you
  want to spawn a molecule per item. `[steps.on_complete]` with
  `for_each` handles this without hand-rolled dispatch.
- **Aggregate outcome on the root.** You want the workflow root to
  close `pass` or `fail` automatically based on its steps, instead of
  manually closing it.
- **Session affinity across steps.** Multi-step work that benefits from
  running on the same live worker session (shared context, hot caches).
  Continuation groups provide this.

Formula v2 is **not** a good fit for:

- Simple linear pipelines with no retries, fanout, or scopes — v1 is
  simpler.
- Short-lived patrol or vapor-phase work — wisps and v1 still win on
  overhead.
- Cities where you run custom worker prompts that rely on the v1 close
  semantics (`bd close <id>`) — you would need to update those prompts
  first.

## Enabling formula v2

Set the flag in `city.toml`:

```toml
[daemon]
formula_v2 = true
```

Flipping that flag activates four behaviors at once:

1. **Formula compilation.** Formulas declaring `version = 2` compile
   using the graph.v2 contract. Formulas still declaring `version = 1`
   continue to compile as v1.
2. **Batch bead creation.** Molecule instantiation switches from
   sequential `store.Create` calls to atomic `ApplyGraphPlan` batches.
   The entire workflow graph becomes visible in one commit.
3. **Implicit control-dispatcher agents.** A city-scoped
   `control-dispatcher` agent plus one per rig are injected into your
   config, each with a singleton pool (`max_active_sessions = 1`) and
   an always-on named session.
4. **Default worker template swap.** Agents that do not set an explicit
   `prompt_template` switch from `prompts/pool-worker.md` to
   `prompts/graph-worker.md` city-wide.

### Backwards compatibility

The deprecated `[daemon] graph_workflows = true` auto-promotes to
`formula_v2 = true` during config parse. If you are upgrading from an
older city, you do not need to edit anything immediately.

### Checking it is active

```shell
gc config show | grep formula_v2
gc status | grep control-dispatcher
```

You should see `formula_v2 = true` in the effective daemon config and
the control-dispatcher session listed as running.

## How v2 differs at compile time

A v1 formula compiles to a hierarchical tree of beads. A v2 formula
compiles to a DAG of sibling beads. Consider this minimal formula:

```toml
formula = "pancakes"

[[steps]]
id = "dry"
title = "Mix dry ingredients"

[[steps]]
id = "wet"
title = "Mix wet ingredients"

[[steps]]
id = "cook"
title = "Cook pancakes"
needs = ["dry", "wet"]
```

### Under v1 (`version = 1` or unset)

```
pancakes (molecule)
├── pancakes.dry   (task)
├── pancakes.wet   (task)
└── pancakes.cook  (task, blocks on dry + wet)
```

Every non-root step is a child of the root via `parent-child`
dependency edges. Ordering inside the tree is expressed with `blocks`
edges.

### Under v2 (`version = 2`)

```
pancakes                    (task, gc.kind=workflow)
pancakes.dry                (task) ──tracks──▶ pancakes
pancakes.wet                (task) ──tracks──▶ pancakes
pancakes.cook               (task) ──tracks──▶ pancakes
    │                       blocks on dry + wet
pancakes.workflow-finalize  (task) ──tracks──▶ pancakes   (auto-injected)
pancakes (root)             ──blocks──▶ workflow-finalize
```

The structural changes:

| Aspect | v1 | v2 |
|---|---|---|
| Root bead type | `molecule` | `task` with `gc.kind=workflow` and `gc.formula_contract=graph.v2` |
| Step relationships | parent-child tree | flat DAG, no parent-child edges |
| Dependency types | `parent-child` + `blocks` | `blocks` + `tracks` |
| Auto-injected finalizer | none | `workflow-finalize` step; root blocks on it |
| Bead creation | sequential | atomic batch via `ApplyGraphPlan` |
| Step ordering in recipe | depth-first tree walk | topological sort |

### Dependency edge types

- **`blocks`** — causal ordering. Step A blocks step B means B is not
  ready until A is closed. Readiness flows along blocks edges.
- **`tracks`** — non-blocking ownership. Every non-root step tracks back
  to the root. This lets `bd delete --cascade` discover the whole
  workflow without coupling readiness to the root.

V2 never produces `parent-child` edges. If you see one in a compiled
v2 recipe, something is wrong.

### The workflow-finalize step

V2 appends a `workflow-finalize` step to every workflow, with `needs`
pointing at the DAG's sinks. The root bead then blocks on
`workflow-finalize`. This is what gives v2 its automatic aggregate
outcome: the control-dispatcher processes `workflow-finalize` when all
real work is done and closes the root with the aggregated
`gc.outcome=pass|fail`.

You do not write this step yourself. It is synthesized during compile.

## How v2 differs at runtime

### The control-dispatcher

The control-dispatcher is a built-in implicit agent that processes
"control beads" — synthesized infrastructure beads that drive workflow
finalization, scope semantics, retries, fanout, and ralph loops. It is
injected automatically when `formula_v2 = true` and you do not need to
add it to `city.toml`.

Control bead kinds it handles:

| `gc.kind` | Purpose |
|---|---|
| `workflow-finalize` | Aggregate the DAG's outcome and close the workflow root |
| `scope-check` | Pass or fail a scope member; abort remaining members on failure |
| `retry` | Classify a retry attempt and append the next attempt if budget remains |
| `retry-eval` | Evaluate a retry attempt result |
| `ralph` | Drive a ralph run/check loop |
| `check` | Execute a ralph check script |
| `fanout` | Expand `on_complete` `for_each` into a molecule per item |

The dispatcher runs `gc convoy control --serve --follow control-dispatcher`
in a singleton pool session. It is deterministic and does not make
judgment calls — it is pure infrastructure machinery.

### The graph-worker template

With `formula_v2 = true`, the default worker prompt switches from
`pool-worker.md` to `graph-worker.md`. The key differences:

- **Session lifetime.** Graph workers poll for work for up to 60
  seconds after closing a bead before draining. Pool workers exit
  immediately after a bead closes.
- **Step model.** Graph workers work one ready bead at a time. They do
  not use `bd mol current` — the workflow graph advances through
  explicit beads, not molecule-internal step lists.
- **Close semantics.** Graph workers set `gc.outcome` metadata as part
  of closing each bead (see the [worker contract](#the-worker-contract)
  section).
- **Continuation groups.** Graph workers pre-claim sibling beads that
  share a `gc.continuation_group` so the same live session keeps the
  work together.
- **Failure classification.** Graph workers distinguish transient from
  hard failures so `[steps.retry]` policies can act on them.

Agents that already set an explicit `prompt_template` in `city.toml` or
a pack keep their custom prompt. Only agents using the default template
auto-switch.

## Writing a v2 formula

### Minimal example

Add `version = 2` to the top of any formula:

```toml
formula = "review-pipeline"
version = 2

[[steps]]
id = "draft"
title = "Draft the PRD"

[[steps]]
id = "review-a"
title = "Review — perspective A"
needs = ["draft"]

[[steps]]
id = "review-b"
title = "Review — perspective B"
needs = ["draft"]

[[steps]]
id = "synthesize"
title = "Synthesize review feedback"
needs = ["review-a", "review-b"]
```

That's all you need for the basic DAG shape. The compiler will:

- Emit the four steps as a flat DAG (not a tree)
- Wire `blocks` edges from `needs`
- Add `tracks` edges from every step to the root
- Append a `workflow-finalize` step with `needs = ["synthesize"]`
- Block the root on `workflow-finalize`

Inspect the compiled recipe with `gc formula show review-pipeline` to
see it laid out.

### Scope lifecycles

Scopes model setup → body → teardown patterns where the teardown must
run even if the body fails. The ship-built-in
`mol-scoped-work` formula is the reference implementation for worktree
lifecycles. The essential pattern:

```toml
[[steps]]
id = "body"
title = "Worktree body scope"
needs = ["workspace-setup", "implement", "self-review", "submit"]
metadata = { "gc.kind" = "scope", "gc.scope_name" = "worktree", "gc.scope_role" = "body" }

[[steps]]
id = "workspace-setup"
title = "Set up the worktree"
needs = ["load-context"]
metadata = {
  "gc.scope_ref" = "body",
  "gc.scope_role" = "setup",
  "gc.on_fail" = "abort_scope",
}

[[steps]]
id = "implement"
title = "Implement the change"
needs = ["workspace-setup"]
metadata = {
  "gc.scope_ref" = "body",
  "gc.scope_role" = "member",
  "gc.on_fail" = "abort_scope",
}

[[steps]]
id = "cleanup-worktree"
title = "Remove the worktree"
needs = ["body"]
metadata = {
  "gc.kind" = "cleanup",
  "gc.scope_ref" = "body",
  "gc.scope_role" = "teardown",
}
```

What the compiler and runtime do with this:

- Every scope member gets a synthesized `scope-check` control bead.
- When a member closes with `gc.outcome=fail` and
  `gc.on_fail=abort_scope`, the scope-check skips all remaining scope
  members and closes the body bead with `gc.outcome=fail`.
- The teardown step depends on the body reaching a terminal state
  (pass or fail), so cleanup always runs.

Members with `gc.scope_role = "member"` or `gc.scope_role = "setup"`
participate in abort-on-fail. Teardowns do not get scope-check beads;
they run unconditionally when the body resolves.

### Transient retries

`[steps.retry]` turns a step into a retry-managed leg with first-class
transient vs hard failure classification:

```toml
[[steps]]
id = "fetch-remote-data"
title = "Fetch data from the external API"

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"   # or "soft_fail"
```

At compile time this emits two beads: a `gc.kind=retry` control bead
that is the stable logical step, and `fetch-remote-data.attempt.1` —
the actual work bead. Downstream `needs` attach to the logical step,
so they do not need to know how many attempts ran.

When the attempt bead closes, the control-dispatcher classifies the
outcome:

- `gc.outcome=pass` → close the logical step pass.
- `gc.outcome=fail` and `gc.failure_class=transient` and budget
  remains → append `attempt.(n+1)`, keep the logical step open, recycle
  the pooled session so a fresh worker picks up the retry.
- `gc.outcome=fail` and `gc.failure_class=hard` → close the logical
  step fail immediately.
- Transient failure, budget exhausted, `on_exhausted=hard_fail` → close
  the logical step fail.
- Transient failure, budget exhausted, `on_exhausted=soft_fail` → close
  the logical step pass with `gc.final_disposition=soft_fail`. Useful
  for optional review legs you want to continue past when all providers
  are misbehaving.

The worker decides `transient` vs `hard` when it closes the attempt.
See the [worker contract](#the-worker-contract) section below.

### Runtime fanout with on_complete

When a step produces output that lists N items and you want to spawn a
molecule per item, use `[steps.on_complete]`:

```toml
[[steps]]
id = "survey-rigs"
title = "List rigs that need work"

[steps.on_complete]
for_each = "output.rigs"
bond = "rig-work-molecule"
vars = { rig_name = "{item.name}" }
# parallel = true  (default; use sequential = true to serialize)
```

The step must emit JSON to its output with an array field matching
`for_each`. After it closes, the control-dispatcher reads the output,
instantiates one `rig-work-molecule` per item with the substituted
vars, and attaches each sub-molecule to the workflow. Downstream
steps that need the fanout to complete should wait on the fanout
control bead or the substitution-renamed step id (see
`ApplyFragmentRecipeGraphControls` for the rewrite rules).

`for_each` paths must start with `output.` and reach into the JSON
emitted by the producing step. `bond` names a formula that will be
instantiated per item. `{item}` and `{item.field}` placeholders are
resolved per iteration; `{index}` gives the zero-based position.

### Session affinity with continuation groups

When a multi-step workflow benefits from running on the same live
worker (shared context, hot caches, in-memory state), stamp
`gc.continuation_group` on the steps that should stick together:

```toml
[[steps]]
id = "load-context"
title = "Load and inspect the assignment"
metadata = { "gc.continuation_group" = "main", "gc.session_affinity" = "require" }

[[steps]]
id = "implement"
title = "Make the change"
metadata = { "gc.continuation_group" = "main", "gc.session_affinity" = "require" }

[[steps]]
id = "self-review"
title = "Review the diff"
metadata = { "gc.continuation_group" = "main", "gc.session_affinity" = "require" }
```

When a graph worker claims the first bead in a continuation group, it
pre-claims every other open bead in that group before any other pool
session can see them. That keeps the work pinned to the same live
session until the group is drained.

Continuation groups are a hint about affinity, not a correctness
invariant. If the session dies mid-group, another pool worker will pick
up the remaining beads.

## The worker contract

The single biggest operational difference between v1 and v2 is how
workers close beads.

### Closing a bead

**v1 worker (pool-worker.md):**

```bash
bd close <id>
```

**v2 worker (graph-worker.md) — success:**

```bash
bd update <id> --set-metadata gc.outcome=pass --status closed
```

**v2 worker — transient failure:**

```bash
bd update <id> \
  --set-metadata gc.outcome=fail \
  --set-metadata gc.failure_class=transient \
  --set-metadata gc.failure_reason=<short_reason> \
  --status closed
```

**v2 worker — hard (non-retriable) failure:**

```bash
bd update <id> \
  --set-metadata gc.outcome=fail \
  --set-metadata gc.failure_class=hard \
  --set-metadata gc.failure_reason=<short_reason> \
  --status closed
```

If a v2 retry-managed attempt bead closes without `gc.outcome`, the
runtime treats it as an invalid worker result and converts it to a
hard failure with `gc.failure_reason=invalid_worker_result_contract`.
This is the most common cause of v2 workflows that appear to "never
close" — workers are closing attempt beads without setting outcome.

### Failure reasons

Keep `gc.failure_reason` to short, machine-readable tokens.
Conventional values: `rate_limited`, `provider_unavailable`,
`worker_glitch`, `prompt_too_large`, `missing_input`,
`invalid_repo_state`. The runtime does not interpret these in v0 —
they are for observability and future policy.

### Other v2 worker behaviors

- **Poll before draining.** After closing a bead, check for more
  assigned work for up to 60 seconds before calling
  `gc runtime drain-ack`. The control-dispatcher may need a moment to
  process control beads and unlock the next step.
- **Claim continuation groups.** When claiming a bead, read its
  `gc.continuation_group` metadata and pre-assign sibling open beads in
  the same group to your session.
- **Do not use `bd mol current`.** Work the individual ready bead
  assigned to you. The molecule-internal step list is a v1 artifact
  that does not apply to graph-first workflows.
- **Do not execute control beads.** `workflow`, `scope`,
  `scope-check`, `workflow-finalize`, `fanout`, `check`, `retry`,
  `retry-eval`, and `ralph` beads are handled by the
  control-dispatcher. Normal workers should not receive them.

Read the full template at
[`cmd/gc/prompts/graph-worker.md`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/prompts/graph-worker.md).

## Migrating from v1

### What you change

1. **Enable the flag** in `city.toml`:
   ```toml
   [daemon]
   formula_v2 = true
   ```
2. **Bump formula version.** Add `version = 2` to formulas you want to
   use the graph contract. Formulas you leave at `version = 1`
   continue to compile the old way.
3. **Update custom worker prompts.** Any agent using a custom
   `prompt_template` keeps that template — it does not auto-switch.
   Update those prompts to match the graph-worker contract (outcome
   metadata, failure classification, polling before drain).

### What carries over automatically

- Agents using the default prompt template switch templates on their
  own on the next `gc prime`.
- The control-dispatcher agents are injected on config load.
- Existing in-flight v1 molecules continue to function; workers close
  their remaining beads normally.
- `graph_workflows = true` (the deprecated predecessor) auto-promotes
  to `formula_v2 = true`.

### What you might need to fix

- **Mixed-mode worker prompts.** If some agents have custom prompts
  and others use the default, they will run on different contracts at
  the same time. Audit custom templates before enabling the flag.
- **Hand-rolled bead closers.** Scripts or agents that run
  `bd close <id>` on graph-worker-assigned attempt beads will produce
  invalid worker result contracts. Update them to set `gc.outcome`.
- **Formula integrations.** Anything that dispatches formulas
  programmatically should pass `version = 2` through to the formula
  TOML if you want the graph contract.

### Reverting

Set `formula_v2 = false` or remove the flag. On the next `gc prime`:

- Formulas with `version = 2` fall back to v1 compilation (with a log
  warning).
- Default-prompt agents switch back to `pool-worker.md`.
- control-dispatcher agents stop being injected.
- In-flight graph.v2 workflows stop advancing because no dispatcher is
  running; close or delete them before reverting in production.

## Troubleshooting

### "Nothing closes my workflow. Steps pile up."

The top three causes in order:

1. **`formula_v2` is not enabled.** Check `gc config show` or
   `city.toml`. Without the flag the control-dispatcher is not
   injected and no one processes `workflow-finalize`.
2. **The control-dispatcher is not running.** Run `gc status` and look
   for a `control-dispatcher` session. If it is not there, run
   `gc start` or check for supervisor errors in
   `gc supervisor logs`.
3. **Workers are closing beads without `gc.outcome`.** Graph-worker
   attempts that close without outcome metadata are treated as
   `invalid_worker_result_contract` hard failures. Inspect a recent
   attempt bead with `bd show <id> --json | jq '.metadata'` — it
   should have `gc.outcome=pass` or `gc.outcome=fail` plus
   `gc.failure_class`.

### "My formula logs 'compiling as v1' even though I set version = 2"

That log line means `formula_v2` is disabled. Add `formula_v2 = true`
under `[daemon]` in `city.toml` and rerun. The compiler refuses to
emit graph.v2 output when the flag is off so that v2 declarations in a
pack do not silently destabilize a city that has not opted in.

### "Step beads are created but never dispatched"

Graph.v2 workflows create all beads in an atomic batch, including
control beads that the control-dispatcher owns. If those control beads
sit forever, the dispatcher is not picking them up. Check:

- `gc status` shows the control-dispatcher as running.
- `bd ready --assignee=<city>/control-dispatcher --json` returns the
  control beads (not empty).
- The dispatcher log at `$GC_CITY/control-dispatcher-trace.log` shows
  recent activity.

If the dispatcher is running but not claiming control beads, the most
likely cause is missing routing metadata on the beads — which points
at a compilation bug rather than a runtime issue.

### "Transient retries are not retrying"

Either the worker never emitted `gc.failure_class=transient`, or the
retry budget is exhausted. Inspect the logical bead:

```shell
bd show <logical-bead-id> --json | jq '.metadata'
```

Look for `gc.retry_count`, `gc.last_failure_class`, and
`gc.exhausted_attempts`. If `gc.last_failure_class` is missing or
`hard`, the worker did not classify the failure as transient. If
`gc.exhausted_attempts` equals `gc.max_attempts`, the budget is used
up — raise `max_attempts` in the formula or fix the underlying
failure.

### "I changed the formula but running molecules did not update"

Compiled recipes are materialized into beads at cook time. Changing
the formula does not retroactively rewrite in-flight molecules — you
need to close or delete the old one and cook a new one. This matches
v1 behavior.

## Limitations and current status

Formula v2 is a substantial new contract and some corners are still
being shaped:

- **The flag is opt-in and labeled "while the feature stabilizes."**
  Behavior may shift between versions. Pin your `gc` version if you
  deploy this.
- **Transient retry classification is worker-authored.** There is no
  runtime classifier that inspects agent output and decides
  transient vs hard. Your worker prompts must do this job.
- **The `gc workflow finish` helper is not yet shipped.** Workers
  currently write outcome metadata directly with `bd update`.
- **Requires bd with `--graph` support.** Older beads backends that
  predate graph-apply will fall through to sequential creation.
- **User-facing docs are limited to this guide and the generated
  config reference.** Some behavior is only specified in code and in
  `engdocs/design/formula-v2-transient-retries.md`.
- **Mixed-mode cities are fragile.** Running v1 and v2 workers against
  the same formulas simultaneously is not recommended.

If you hit something that does not match this guide, check the
draft design notes in `engdocs/design/` or file an issue.

## See also

- [Tutorial 05 — Formulas](/tutorials/05-formulas) — v1 formulas,
  variables, conditions, loops, and ralph.
- [Formula Files reference](/reference/formula) — formula file layout
  and common fields.
- [Config reference](/reference/config#daemonconfig) — the
  `[daemon]` section, including `formula_v2`.
- [`graph-worker.md`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/prompts/graph-worker.md) —
  the default worker prompt when v2 is enabled.
- [`mol-scoped-work.formula.toml`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formulas/mol-scoped-work.formula.toml) —
  the built-in scoped-worktree reference formula.
