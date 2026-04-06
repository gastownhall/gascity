---
title: Tutorial 05 - Beads
description: Understand the universal work primitive that underlies sessions, mail, formulas, and convoys — and learn to query and manipulate work items directly.
---

# Tutorial 05: Beads

If you've been following along, you've been creating beads without knowing it. When you started a session — that created a bead. When you sent mail — bead. When you cooked a formula — beads. When sling dispatched a wisp — bead.

Beads are the universal work primitive in Gas City. Every trackable thing — tasks, messages, sessions, molecules, convoys — is a bead in the store. This tutorial peels back the layer and shows you what's underneath.

We'll pick up where Tutorial 04 left off. You should have `my-city` running with `my-project` and `my-api` rigged, and agents for `mayor`, `helper`, `worker`, and `reviewer`.

You don't need to understand beads to use Gas City productively. But if you want to understand *why* the system works the way it does, or if you want to query and manipulate work items directly, this is where it lives.

## What is a bead

A bead is a unit of work with an ID, a title, a status, and a type. We use the `bd` tool to work with beads directly.

```shell
~/my-city
$ bd list
ID      TYPE      STATUS       TITLE
gc-1    session   in_progress  mayor
gc-2    session   in_progress  helper
gc-5    task      open         Review auth module
gc-6    message   closed       Review needed
gc-10   wisp      closed       review (PR #42)
gc-11   convoy    open         sprint-42
```

Every bead has:

- **ID** — unique identifier with a short prefix derived from the city or rig name (e.g., `gc-5` for a city named "gascity", `ma-12` for a rig named "my-app")
- **Title** — human-readable name
- **Status** — `open`, `in_progress`, or `closed`
- **Type** — what type of bead it is

## Bead types

The type determines what a bead represents:

| Type | What it is | Created by |
|---|---|---|
| **task** | A unit of work | `bd create`, formula steps |
| **message** | Inter-agent mail | `gc mail send` |
| **session** | A running agent session | `gc session new` |
| **molecule** | Persistent formula instance | `gc formula cook` |
| **wisp** | Ephemeral formula instance | `gc sling --formula` |
| **convoy** | Container grouping related beads | `gc convoy create`, auto-created by sling |

The type system is simple by design. Gas City doesn't have separate storage for tasks vs. messages vs. sessions — they're all beads with different type labels. This is what makes the system composable: the same store, the same query interface, the same dependency model works for everything.

## Creating beads

Most beads are created indirectly:

- `gc session new helper` creates a session bead
- `gc mail send mayor "Subject" "Body"` creates a message bead
- `gc formula cook review` creates molecule + step beads
- `gc sling worker review --formula` creates a wisp bead + convoy

But you can use `bd` to create them manually:

```shell
~/my-city
$ bd create "Fix the login bug"
Created gc-15: Fix the login bug

$ bd create "Refactor auth module" --type feature
Created gc-16: Refactor auth module
```

## Bead lifecycle

Beads have three states:

```
open → in_progress → closed
```

- **open** — work hasn't started yet. Discoverable by agents via hooks.
- **in_progress** — claimed by an agent, being worked on.
- **closed** — done.

That's it. There's no "blocked" or "review" state in Gas City's model — those are useful in project management tools, but the system only needs to know whether work is available, claimed, or finished.

```shell
~/my-city
$ bd close gc-15
Closed gc-15: Fix the login bug

$ bd list --state open
ID      TYPE    STATUS  TITLE
gc-16   feature open    Refactor auth module
```

## Beads as execution state

The bead store is effectively the execution state of the entire system. Every session that's running, every message in flight, every formula step being worked on — all of it is a bead with a status. If you want to know what the city is doing right now, you query the store:

```shell
~/my-city
$ bd list --state in_progress
ID      TYPE      STATUS       TITLE
gc-1    session   in_progress  mayor
gc-2    session   in_progress  helper
gc-15   task      in_progress  Fix the login bug
```

This is what makes Gas City crash-safe. Work isn't held in memory or tracked by a running process — it's persisted in the store. If an agent dies, its beads stay open. When the agent restarts, its hooks discover the same work and pick up where it left off. If the whole city stops and restarts, the bead store is the ground truth for what was happening and what still needs to happen.

The rest of this chapter covers the details — how beads get organized, routed, grouped, and discovered by agents. You can skim these sections and come back when you need them.

## Labels

Labels are how beads get organized and routed:

```shell
~/my-city
$ bd label gc-16 priority:high frontend
Added labels to gc-16

$ bd list --label priority:high
ID      TYPE     STATUS  TITLE
gc-16   feature  open    Refactor auth module
```

Some labels have special meaning in Gas City:

- **`pool:<agent-name>`** — used for pool agent routing. When work is slung to a pool, it gets this label so pool members can discover it.
- **`gc:session`** — marks session beads
- **`gc:message`** — marks mail beads
- **`thread:<id>`** — groups mail messages into conversations
- **`read`** — marks a message as read

You can add any labels you want for your own organization.

## Metadata

Beads carry arbitrary key-value metadata for structured state:

```shell
~/my-city
$ bd meta gc-16 branch=feature/auth reviewer=sky
Set metadata on gc-16
```

Metadata is used internally for things like session tracking (`session_name`, `alias`), merge strategies, and formula references. You can use it for anything you want to attach to a bead without changing its title or description.

## Dependencies

Beads can depend on other beads. You've already seen this in formulas — when a step declares `needs = ["design"]`, that's a blocking dependency. The step bead can't start until the design bead closes. Dependencies are how Gas City enforces ordering without a central scheduler: each bead knows what it's waiting for, and agents only see work that's ready.

```shell
~/my-city
$ bd dep gc-16 blocks gc-17
Added dependency: gc-16 blocks gc-17
```

Now `gc-17` won't appear in any agent's work query until `gc-16` is closed. This is the same mechanism that makes formula step ordering work — `needs` declarations become `blocks` edges between step beads.

There are three dependency types: **`blocks`** (must close before the other can start), **`tracks`** (informational — "I care about this"), and **`relates-to`** (loose association). Only `blocks` affects work visibility.

Beads also have a separate *parent-child* relationship — a bead can set a `parent_id` linking it to a container. This is how convoys and molecules group their children. The difference: dependencies express ordering ("do A before B"), while parent-child expresses containment ("these beads belong to this group"). A convoy's children don't depend on each other — they're just members of the same batch.

## Convoys

If you've slung a formula, you've already created a convoy without knowing it — Gas City automatically wraps dispatched formula work in one. You'll see them in `bd list` as beads with type `convoy`, and in `gc convoy list` with progress summaries. They matter when you need to track a batch of related work as a unit: "are all five of these tasks done yet?" is a convoy question.

You can also create them by hand to group arbitrary work — say, a set of beads you want to track together as a sprint or a deploy:

```shell
~/my-city
$ gc convoy create "Sprint 42" gc-15 gc-16 gc-17
Created convoy gc-20 "Sprint 42" tracking 3 issue(s)
```

The convoy is a bead with type `convoy`. The child beads are linked via their `ParentID` — the same parent-child mechanism used by molecules, just for grouping instead of step ordering.

```shell
~/my-city
$ gc convoy status gc-20
Convoy:      gc-20
Title:       Sprint 42
Status:      open
Progress:    1/3 closed

ID      TITLE                    STATUS       ASSIGNEE
gc-15   Fix the login bug        closed       my-app/polecat
gc-16   Refactor auth module     open         -
gc-17   Update API docs          open         claude
```

### Auto-close

When a bead closes, Gas City checks whether its parent is a convoy with all children now closed. If so, the convoy closes automatically. This happens in the background via the `on_close` hook — no polling, no manual intervention.

Convoys with the **owned** label skip auto-close. These are for workflows where you want explicit control over when the convoy completes:

```shell
~/my-city
$ gc convoy create "Auth rewrite" --owned --target integration/auth
Created convoy gc-25 "Auth rewrite"
```

When you're done, land it explicitly:

```shell
~/my-city
$ gc convoy land gc-25
Landed convoy gc-25
```

### Adding beads and checking convoys

Sometimes work grows after a convoy is created — a new bug surfaces mid-sprint, or a dependency gets discovered after the plan is set. You can add beads to an existing convoy:

```shell
~/my-city
$ gc convoy add gc-20 gc-18
Added gc-18 to convoy gc-20
```

If a convoy should have auto-closed but didn't (say a hook misfired), you can reconcile manually:

```shell
~/my-city
$ gc convoy check
Auto-closed convoy gc-20 "Sprint 42"
1 convoy(s) auto-closed
```

### Stranded work

To find open beads in convoys that have no assignee — work that's stuck waiting for someone to pick it up:

```shell
~/my-city
$ gc convoy stranded
CONVOY  ISSUE  TITLE
gc-20   gc-16  Refactor auth module
```

### Convoy metadata

Convoys carry metadata that controls how grouped work behaves:

- **`convoy.owner`** — which agent manages this convoy
- **`convoy.notify`** — who to notify when the convoy completes
- **`convoy.merge`** — merge strategy for PRs (`direct`, `mr`, `local`)
- **`target`** — target branch inherited by child beads

These are set at creation time with flags:

```shell
~/my-city
$ gc convoy create "Deploy v2" --owner mayor --merge mr --target main
Created convoy gc-30 "Deploy v2"
```

Or update the target later:

```shell
~/my-city
$ gc convoy target gc-30 develop
Set target of convoy gc-30 to develop
```

## How agents find work

This is where beads connect to the runtime. Agents discover work through *hooks* — shell commands that run between turns and check for available beads.

The typical flow:

1. Work is created (via `bd create`, `gc sling`, formula cook, etc.)
2. Work is routed to an agent (via `gc sling`, pool labels, assignee)
3. Agent's hook runs a *work query*: `bd ready --assignee=<agent-name>`
4. If work is found, the hook injects it into the agent's context as a system reminder
5. The agent sees the work and acts on it (GUPP: "if you find work on your hook, you run it")

For pool agents, the query checks labels instead of assignee:

```shell
~/my-city
$ bd ready --label=pool:my-project/worker --unassigned --limit=1
```

This is the "pull" model — agents check for work rather than having work pushed to them. It's simple, crash-safe (queued work survives restarts), and scales naturally.

## The bead store

Beads are persisted in a store. Gas City supports several backends:

- **bd** (default) — Dolt-backed database via the `bd` CLI. Full-featured, good for production.
- **file** — JSON file on disk. Simple, good for tutorials and small setups.
- **exec** — Delegates to a custom script. For integration with external systems.

Configure the backend in `city.toml`:

```toml
[beads]
provider = "file"    # or "bd" (default)
```

For most users, the default works fine and you don't need to think about it.

---

You don't usually work with beads directly. The higher-level commands — `gc session`, `gc mail`, `gc sling`, `gc formula` — handle bead creation and management for you. But when you want to query what work is outstanding across the city, create ad-hoc tasks for agents, inspect the dependency graph of a formula, or debug why an agent isn't picking up work — that's when you reach for `bd` directly.

```shell
~/my-city
$ bd list --state open --type task
ID      TYPE  STATUS  TITLE
gc-15   task  open    Fix the login bug
ma-3    task  open    Update API docs

$ bd show gc-15
ID:          gc-15
Title:       Fix the login bug
Type:        task
Status:      open
Assignee:    my-project/worker
Labels:      pool:my-project/worker, priority:high
Created:     2026-03-30 14:22:01
Description: The login endpoint returns 500 when...

Dependencies:
  gc-15 blocks gc-16

$ bd close gc-15
Closed gc-15: Fix the login bug
```

Beads are the ground truth of the running state of the city. Everything else in Gas City — sessions, mail, formulas, convoys — is built on top of them.

## What's next

- **[Orders](06-orders.md)** — formulas and scripts on autopilot, gated by time, schedule, conditions, or events

<!--
BONEYARD — draft material for future sections. Not part of the published tutorial.

### The bead store internals

The bd (beads) store is built on Dolt — a database with Git-like versioning.
Each city gets its own Dolt database in .beads/dolt/. The bd CLI talks to a
local Dolt SQL server. This gives you SQL access to bead state if you need it,
plus branch/merge semantics for the work store itself.

The file store alternative writes JSON to .beads/store.json — useful for
tutorials and testing, but doesn't support concurrent access well.

The exec store delegates everything to a user script, which receives
subcommands (create, get, list, update, close, etc.) and returns JSON.
This is the escape hatch for integrating with external issue trackers
like Linear or GitHub Issues.

### Custom bead types

The type field is a free-form string. Gas City reserves a few (task, message,
session, molecule, wisp, convoy) but you can create beads with any type:

bd create "Security audit" --type audit

Custom types show up in bd list and can be filtered with --type. They don't
get special treatment from the system — no auto-close, no hook integration —
but they're useful for organizing domain-specific work.

### Everything is a bead (reference-style recap)

The unifying principle: beads are the persistence substrate for all domain state.

When you gc session new helper, the system creates a bead with type session,
labels it gc:session, and stores the session metadata (tmux name, alias,
provider, working directory) as bead metadata. When the session closes, the
bead closes.

When you gc mail send mayor "Subject" "Body", the system creates a bead with
type message, title set to the subject, description set to the body, assignee
set to the recipient. Reading the message adds a read label. Replying creates
a new bead in the same thread.

When you gc sling worker review --formula, the system compiles the formula,
creates a wisp bead as the root, optionally creates step beads as children,
creates a convoy bead to group them, routes the root to the worker, and nudges
the worker to check its hook.

Same store. Same interface. Same query model. Could be promoted to a reference
doc rather than tutorial content — the bead types table and "beads as execution
state" section already cover the core idea.

### Bead queries for scripting

bd supports JSON output for scripting:

bd list --state open --json | jq '.[].title'
bd ready --assignee=worker --json | jq length

The work_query config on agents uses this — it's just a shell command that
returns JSON, and the hook infrastructure parses it.
-->
