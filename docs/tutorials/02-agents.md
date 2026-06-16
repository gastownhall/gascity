---
title: Tutorial 02 - Agents
sidebarTitle: 02 - Agents
description: Define agents and use them to execute work.
---

An implicit agent like `claude` or `codex` is unconfigured: no custom prompt,
adopting its provider's name, running the raw provider. This tutorial defines a
custom agent with its own role and prompt, then slings work to it.

You should have `my-city` running with `my-project` rigged (from
[Tutorial 01](/tutorials/01-cities-and-rigs)).

## Defining an agent

Each custom agent gets its own directory under `agents/<name>/`. Start by
creating a rig-scoped reviewer:

```shell
~/my-city
$ gc agent add --name reviewer
Scaffolded agent 'reviewer'

~/my-city
$ cat > agents/reviewer/agent.toml << 'EOF'
dir = "my-project"
provider = "codex"
EOF
```

The `agent.toml` scopes the reviewer to `my-project` and switches it from the
city's default `claude` provider to `codex`.

<Note>
This section sets `provider = "codex"`. If you don't have Codex installed and
configured, substitute another provider you do have (e.g., `provider =
"claude"`); the rest of the walkthrough is the same.
</Note>

The provider catalog is explicit. `gc init` registered `claude`, but any other
provider an agent references must also be registered in `city.toml` — otherwise
every `gc` command fails with `provider catalog is missing referenced
providers`. Register the builtin alias:

```toml
# city.toml — register the second provider
[providers.codex]
base = "builtin:codex"
```

(`gc doctor --fix` adds missing builtin provider aliases for you.
`base = "builtin:<name>"` works for any provider Gas City ships a preset for.
If you substituted `claude` above, skip this step — `gc init` already
registered it.)

The agent needs a prompt. With no agent named, `gc prime` falls back to a
generic worker prompt useful for a single-shot CLI invocation:

```shell
~/my-city
$ gc prime
# Gas City Agent

You are an agent in a Gas City workspace. Claim available work and execute it.

## Your tools

- `gc hook --claim --json` — find and atomically claim one work item
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work

1. Claim work: `gc hook --claim --json`
2. Read the claimed bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.
```

`gc prime` shows the prompt an agent runs with — the instructions that tell it
how to pick up and act on a slung bead. Pass an agent name to inspect a specific
one: `gc prime mayor` prints the mayor's prompt; `gc prime my-project/reviewer`
prints the reviewer's once we've written it.

The reviewer's prompt pairs the standard "find and execute" loop with
review-specific instructions:

```shell
~/my-city
$ cat > agents/reviewer/prompt.template.md << 'EOF'
# Code Reviewer Agent
You are an agent in a Gas City workspace. Claim available work and execute it.

## Your tools
- `gc hook --claim --json` — find and atomically claim one work item
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work
1. Claim work: `gc hook --claim --json`
2. Read the claimed bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF
$ gc prime my-project/reviewer
# Code Reviewer Agent
You are an agent in a Gas City workspace. Claim available work and execute it.
... # contents elided as identical to the above
```

You can also set the model and permission mode:

```toml
dir = "my-project"
provider = "codex"
option_defaults = { model = "o4-mini", permission_mode = "suggest" }
```

That file would live at `agents/reviewer/agent.toml`. Valid values come from
each provider's options schema, so they differ per provider — `o4-mini` is a
Codex model, while the same key on a `claude` agent would take values like
`sonnet` or `haiku`.

Now that your agent is available, it's time to sling some work to it:

```shell
~/my-city
$ cd ~/my-project
~/my-project
$ gc sling my-project/reviewer "Review hello.py and write review.md with feedback"
Created mp-p956 — "Review hello.py and write review.md with feedback"
Auto-convoy mp-4wdl
Slung mp-p956 → my-project/reviewer
```

Because the reviewer is scoped to `my-project`, you target it as
`my-project/reviewer` from inside that directory. Gas City started a Codex
session, loaded `agents/reviewer/prompt.template.md`, and delivered the task.

There's no `Attached workflow` line this time: implicit agents like `claude`
ship with `mol-do-work` as their default sling formula, but a custom agent has
none until you configure one, so the bead is delivered directly. Sling still
creates an auto-convoy to track it. Watch progress with `bd show`; when the
work finishes, the review is on disk:

```shell
~/my-project
$ ls
hello.py  review.md

~/my-project
$ cat review.md
# Review
No findings.

`hello.py` is a single `print("Hello, World!")` statement and does not present a meaningful bug, security, or style issue in its current form.
```

Direct delivery suits fire-and-forget work. To watch an agent run or talk to one
directly, you need a session — see [the next tutorial](/tutorials/03-sessions).

## What's next

You've defined an agent with a custom prompt, pointed it at a different
provider, and slung work to it directly. From here:

- **[The six primitives](/getting-started/how-gas-city-works)** — the canonical model agents,
  sessions, and work all build on
- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/05-formulas)** — how multi-step work should be
  done: steps, dependencies, and variables
- **[Beads](/tutorials/06-beads)** — the unit of work; every task, message, and
  convoy member is a bead
