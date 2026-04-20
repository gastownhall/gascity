---
title: Tutorial 06 - Beads
sidebarTitle: 06 - Beads
description: Understand the universal work primitive that underlies sessions, mail, formulas, and convoys — and learn to query and manipulate work items directly.
---

If you've been following along, you've been creating beads without knowing it.
When you started a session — that created a bead. When you sent mail — bead.
When you cooked a formula — beads. When sling dispatched a wisp — bead.

Beads are the universal work primitive in Gas City. Every trackable thing —
tasks, messages, sessions, molecules, convoys — is a bead in the store. This
tutorial peels back the layer and shows you what's underneath.

We'll pick up where [Tutorial 03](/tutorials/03-sessions) left off. You
should have `my-city` running with `my-project` rigged, and agents for `mayor`
and `reviewer` (along with the corresponding prompts):

```shell
~/my-city
$ cat pack.toml
[pack]
name = "my-city"
schema = 2

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"

[[named_session]]
template = "mayor"
mode = "always"

~/my-city
$ cat city.toml
[workspace]
provider = "claude"

[[rigs]]
name = "my-project"

~/my-city
$ cat agents/reviewer/agent.toml
dir = "my-project"
provider = "codex"
```

The corresponding prompt files live under `agents/<name>/prompt.template.md`.
The machine-local workspace identity and rig binding live in `.gc/site.toml`:

```toml
workspace_name = "my-city"

[[rig]]
name = "my-project"
path = "/Users/csells/my-project"
```

Beads are fundamental to the system. You're going to be working with crew to
turn plans into beads that can be executed in parallel by polecats.

## What is a bead

A bead is a unit of work with an ID, a title, a status, and a type. We use the
`bd` tool to work with beads directly.

```shell
~/my-city
$ bd list
○ mc-194 ● P2 pancakes
├── ○ mc-194.3 ● P2 Combine wet and dry
├── ○ mc-194.4 ● P2 Cook the pancakes
└── ○ mc-194.5 ● P2 Serve
○ mc-a4l ● P2 Refactor auth module
○ mc-d4g ● P2 Sprint 42
○ mc-io4 ● P2 mayor
○ mc-xp7 ● P2 Update API docs

Status: ○ open  ◐ in_progress  ● blocked  ✓ closed  ❄ deferred
```

By default `bd list` renders a tree, with parent beads grouping their children.
The leading glyph is the bead's status, followed by ID, priority (`P2`), and
title. Pass `--flat` for a single-level list and `--all` to include closed
beads.

Every bead has:

- **ID** — unique identifier prefixed with two letters derived from the city or
  rig name (e.g., `mc-194` for a city named "my-city", `ma-12` for a rig named
  "my-app")
- **Title** — human-readable name
- **Status** — `open`, `in_progress`, `blocked`, `deferred`, or `closed`
- **Type** — what kind of bead it is

## Bead types

The type determines what a bead represents:

| Type         | What it is                       | Created by                                |
| ------------ | -------------------------------- | ----------------------------------------- |
| **task**     | A unit of work                   | `bd create`, formula steps                |
| **message**  | Inter-agent mail                 | `gc mail send`                            |
| **session**  | A running agent session          | `gc session new`                          |
| **molecule** | Persistent formula instance      | `gc formula cook`                         |
| **wisp**     | Ephemeral formula instance       | `gc sling --formula`                      |
| **convoy**   | Container grouping related beads | `gc convoy create`, auto-created by sling |

The type system is simple by design. Gas City doesn't have separate storage for
tasks vs. messages vs. sessions — they're all beads with different type labels.
This is what makes the system composable: the same store, the same query
interface, the same dependency model works for everything.

## Creating beads

Most beads are created indirectly:

- `gc session new my-project/reviewer` creates a session bead
- `gc mail send mayor "Subject" "Body"` creates a message bead
- `gc formula cook review` creates molecule + step beads
- `gc sling mayor review --formula` creates a wisp bead + convoy

But you can use `bd` to create them manually:

```shell
~/my-city
$ bd create "Fix the login bug"
✓ Created issue: mc-ykp — Fix the login bug
  Priority: P2
  Status: open

$ bd create "Refactor auth module" --type feature
✓ Created issue: mc-a4l — Refactor auth module
  Priority: P2
  Status: open
```

## Bead lifecycle

Beads move through a small set of states:

```
open → in_progress → closed
```

- **open** — work hasn't started yet. Discoverable by agents via hooks.
- **in_progress** — claimed by an agent, being worked on.
- **closed** — done.
- **blocked** — has an open `blocks` dependency. Set automatically.
- **deferred** — explicitly snoozed until a date.

In day-to-day use, **open / in_progress / closed** are the ones you reach for.
`blocked` and `deferred` are derived states the system manages for you.

```shell
~/my-city
$ bd close mc-ykp
✓ Closed mc-ykp — Fix the login bug: Closed

$ bd list --status open --flat
○ mc-a4l [● P2] [feature] - Refactor auth module
○ mc-xp7 [● P2] [task]    - Update API docs
```

Note that the flag is `--status` (`--state` is a different command for state
dimensions).

## Beads as execution state

The bead store is effectively the execution state of the entire system. Every
session that's running, every message in flight, every formula step being worked
on — all of it is a bead with a status. If you want to know what the city is
doing right now, you query the store. The exact output depends on what is
currently active in your city. For example:

```shell
~/my-city
$ bd list --status in_progress --flat
◐ mc-io4 [● P2] [session] - mayor
```

This is what allows you to use agents sessions as disposable processes for
executing work; work isn't held in memory or tracked by a running process — it's
persisted in the store. If an agent dies, its beads stay open. When the agent
restarts, its hooks discover the same work and pick up where it left off. If the
whole city stops and restarts, the bead store is the ground truth for what was
happening and what still needs to happen.

The rest of this chapter covers the details — how beads get organized, routed,
grouped, and discovered by agents.

## Labels

Labels are how beads get organized and routed:

```shell
~/my-city
$ bd label add mc-a4l priority:high
✓ Added label 'priority:high' to mc-a4l

$ bd label add mc-a4l frontend
✓ Added label 'frontend' to mc-a4l

$ bd list --label priority:high --flat
○ mc-a4l [● P2] [feature] - Refactor auth module
```

`bd label add` takes a single label per call — apply multiples one at a time.

Some labels have special meaning in Gas City:

- **`gc:session`** — marks session beads
- **`gc:message`** — marks mail beads
- **`thread:<id>`** — groups mail messages into conversations
- **`read`** — marks a message as read

You can add any labels you want for your own organization.

## Metadata

Beads carry arbitrary key-value metadata for structured state:

```shell
~/my-city
$ bd update mc-a4l --set-metadata branch=feature/auth --set-metadata reviewer=sky
✓ Updated issue: mc-a4l — Refactor auth module
```

Metadata is used internally for things like session tracking (`session_name`,
`alias`), routing (`gc.routed_to`), merge strategies, and formula references.
You can use it for anything you want to attach to a bead without changing its
title or description. Use `--unset-metadata <key>` to remove one.

## Dependencies

Beads can depend on other beads. You've already seen this in formulas — when a
step declares `needs = ["design"]`, that's a blocking dependency. The step bead
can't start until the design bead closes. Dependencies are how Gas City enforces
ordering without a central scheduler: each bead knows what it's waiting for, and
agents only see work that's ready.

```shell
~/my-city
$ bd dep mc-a4l --blocks mc-xp7
✓ Added dependency: mc-a4l (Refactor auth module) blocks mc-xp7 (Update API docs)
```

Now `mc-xp7` won't appear in any agent's work query until `mc-a4l` is closed.
This is the same mechanism that makes formula step ordering work — `needs`
declarations become `blocks` edges between step beads.

The dependency types are **`blocks`** (must close before the other can start),
**`tracks`** (informational — "I care about this"), **`related`** (loose
association), **`parent-child`** (containment), and **`discovered-from`** (work
that surfaced while doing other work). Only `blocks` affects work visibility.

Beads also have a separate _parent-child_ relationship — a bead can set a
`parent_id` linking it to a container. This is how convoys and molecules group
their children. The difference: dependencies express ordering ("do A before B"),
while parent-child expresses containment ("these beads belong to this group"). A
convoy's children don't depend on each other — they're just members of the same
batch.

## Convoys

If you've slung a formula, you've already created a convoy without knowing it —
Gas City automatically wraps dispatched formula work in one. You'll see them in
`bd list` as beads with type `convoy`, and in `gc convoy list` with progress
summaries. They matter when you need to track a batch of related work as a unit:
"are all five of these tasks done yet?" is a convoy question.

You can also create them by hand to group arbitrary work — say, a set of beads
you want to track together as a sprint or a deploy:

```shell
~/my-city
$ gc convoy create "Sprint 42" mc-ykp mc-a4l mc-xp7
Created convoy mc-d4g "Sprint 42" tracking 3 issue(s)
```

The convoy is a bead with type `convoy`. The child beads are linked via their
`ParentID` — the same parent-child mechanism used by molecules, just for
grouping instead of step ordering.

```shell
~/my-city
$ gc convoy status mc-d4g
Convoy:   mc-d4g
Title:    Sprint 42
Status:   open
Progress: 1/3 closed

ID      TITLE                 STATUS  ASSIGNEE
mc-ykp  Fix the login bug     closed  -
mc-a4l  Refactor auth module  open    -
mc-xp7  Update API docs       open    -
```

### Auto-close

When a bead closes, Gas City checks whether its parent is a convoy with all
children now closed. If so, the convoy closes automatically. This happens in the
background via the `on_close` hook — no polling, no manual intervention.

Convoys with the **owned** label skip auto-close. These are for workflows where
you want explicit control over when the convoy completes:

```shell
~/my-city
$ gc convoy create "Auth rewrite" --owned --target integration/auth
Created convoy mc-0ud "Auth rewrite"
```

When you're done, land it explicitly:

```shell
~/my-city
$ gc convoy land mc-0ud
Landed convoy mc-0ud
```

### Adding beads and checking convoys

Sometimes work grows after a convoy is created — a new bug surfaces mid-sprint,
or a dependency gets discovered after the plan is set. You can add beads to an
existing convoy:

```shell
~/my-city
$ gc convoy add mc-d4g mc-xp7
Added mc-xp7 to convoy mc-d4g
```

If a convoy should have auto-closed but didn't (say a hook misfired), you can
reconcile manually:

```shell
~/my-city
$ gc convoy check
Auto-closed convoy mc-d4g "Sprint 42"
1 convoy(s) auto-closed
```

### Stranded work

To find open beads in convoys that have no assignee — work that's stuck waiting
for someone to pick it up:

```shell
~/my-city
$ gc convoy stranded
CONVOY  ISSUE   TITLE
mc-d4g  mc-a4l  Refactor auth module
mc-d4g  mc-xp7  Update API docs
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
Created convoy mc-zk1 "Deploy v2"
```

Or update the target later:

```shell
~/my-city
$ gc convoy target mc-zk1 develop
Set target of convoy mc-zk1 to develop
```

## How agents find work

This is where beads connect to the runtime. Agents discover work through _hooks_
— shell commands that run between turns and check for available beads.

The typical flow:

1. Work is created (via `bd create`, `gc sling`, formula cook, etc.)
2. Work is routed to an agent (via assignee or `gc.routed_to` metadata)
3. Agent's hook runs a _work query_ and looks for matching ready beads
4. If work is found, the hook injects it into the agent's context as a system
   reminder
5. The agent sees the work and acts on it (GUPP: "if you find work on your hook,
   you run it")

For routed pool work, the query checks metadata instead of assignee:

```shell
~/my-city
$ bd ready --metadata-field gc.routed_to=my-project/worker --unassigned --limit=1
```

Because `mc-xp7` is blocked by `mc-a4l` right now, this query won't return
anything yet. That's the point: blocked work is invisible to agent work
queries. Once `mc-a4l` closes, rerun the same query and `mc-xp7` becomes
eligible.

This is the "pull" model — agents check for work rather than having work pushed
to them. It's simple, crash-safe (queued work survives restarts), and scales
naturally.

## The bead store

Beads are persisted in a store. Gas City supports several backends:

- **bd** (default) — Dolt-backed database via the `bd` CLI. Full-featured, good
  for production.
- **file** — JSON file on disk. Simple, good for tutorials and small setups.
- **exec** — Delegates to a custom script. For integration with external
  systems.

Configure the backend in `city.toml`:

```toml
[beads]
provider = "file"    # or "bd" (default)
```

For most users, the default works fine and you don't need to think about it.

---

You don't usually work with beads directly. The higher-level commands — `gc
session`, `gc mail`, `gc sling`, `gc formula` — handle bead creation and
management for you. But when you want to query what work is outstanding across
the city, create ad-hoc tasks for agents, inspect the dependency graph of a
formula, or debug why an agent isn't picking up work — that's when you reach for
`bd` directly.

```shell
~/my-city
$ bd list --status open --type task --flat
○ mc-xp7 [● P2] [task] - Update API docs
○ mc-2wx.1 [● P2] [task] - Mix dry ingredients (parent: mc-2wx, blocks: mc-2wx.3)

$ bd show mc-a4l
○ mc-a4l · Refactor auth module   [● P2 · OPEN]
Owner: dbox · Type: feature
Created: 2026-04-08 · Updated: 2026-04-08

LABELS: frontend, priority:high

METADATA
  branch: feature/auth
  reviewer: sky

PARENT
  ↑ ○ mc-d4g: Sprint 42 ● P2

BLOCKS
  ← ○ mc-xp7: Update API docs ● P2

$ bd close mc-a4l
✓ Closed mc-a4l — Refactor auth module: Closed
```

Beads are the ground truth of the running state of the city. Everything else in
Gas City — sessions, mail, formulas, convoys — is built on top of them.

## What's next

- **[Orders](/tutorials/07-orders)** — formulas and scripts on autopilot, triggered
  by time, schedule, conditions, or events
