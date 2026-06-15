---
title: Tutorial 02 - Agents
sidebarTitle: 02 - Agents
description: Define agents and use them to execute work.
---

In [Tutorial 01](/tutorials/01-cities-and-rigs), you created a city, added a
rig, and slung work to an implicit agent. The implicit agents (`claude`, `codex`, etc.)
are convenient, but they have no custom prompt — they're just the raw provider.
In this tutorial, you'll define your own agents with specific roles and use them
to get work done.

We'll pick up where Tutorial 01 left off. You should have `my-city` running with
`my-project` rigged.

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

This creates the `agents/reviewer/` scaffold. The `agent.toml` file scopes the
reviewer to `my-project` and switches it from the city's default `claude`
provider to `codex`.

<Note>
This section sets `provider = "codex"`. If you don't have Codex installed and
configured, substitute another provider you do have (e.g., `provider =
"claude"`); the rest of the walkthrough is the same.
</Note>

One more piece of wiring: a city's provider catalog is explicit. `gc init`
registered your default provider (`claude`), but any other provider an agent
references must also be registered in `city.toml` — otherwise every `gc`
command fails with `provider catalog is missing referenced providers`.
Register the builtin alias:

```toml
# city.toml — register the second provider
[providers.codex]
base = "builtin:codex"
```

(If you forget, `gc doctor --fix` adds missing builtin provider aliases for
you. If you substituted a different provider above, register that one instead
— `base = "builtin:<name>"` works for any provider Gas City ships a builtin
preset for. And if you substituted `claude`, skip this step: `gc init`
already registered it.)

You'll want to create a prompt for the new agent. Let's first see what
`gc prime` returns when you don't name an agent — without an agent argument,
it falls back to a generic worker prompt useful for a single-shot CLI
invocation:

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

The `gc prime` command tells you the prompt an agent is running with. In
[tutorial 01](/tutorials/01-cities-and-rigs) we learned that slinging work to
an agent created a bead; the agent's prompt is what tells it how to pick up
and act on that work. Pass an agent name to inspect a specific agent:
`gc prime mayor` would print the mayor's prompt;
`gc prime my-project/reviewer` would print the reviewer's prompt once we've
written one.

To make the reviewer useful, we'll write a prompt that tells it how to
discover work (the standard Gas City "find and execute" loop) and then
layer on the specifics of being a review agent. Create the reviewer prompt
to look like the following:

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

Notice that use of `gc prime <agent-name>` to get the contents of your custom
prompt for that agent. That's a handy way to check on how the built-in agents or
your own custom agents are configured as you build out more of them over time.

If you wanted to get fancy, you could also set the model and permission mode:

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

Your new reviewer agent is scoped to the `my-project` rig, so from inside that
directory you can target it explicitly as `my-project/reviewer`. Gas City
started a Codex session, loaded the prompt from
`agents/reviewer/prompt.template.md`, and delivered the task to the rig-scoped
reviewer. The output looks a little different from Tutorial 01's sling, and
that's expected: implicit agents like `claude` come with `mol-do-work` as
their default sling formula, but a custom agent has none until you configure
one — so there's no `Attached workflow` line here. The bead is delivered
directly, and sling creates an auto-convoy to track it. You can watch progress with `bd show` as you already know. And when
the work is done, you can check the file system for the review you requested:

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

This is handy for fire-and-forget kind of work. However, if you'd like to see
the agent in action or even talk to one directly, you're going to need a
session. And for that, you'll want to check in on [the next
tutorial](/tutorials/03-sessions).

## What's next

You've defined an agent with a custom prompt, pointed it at a different
provider, and slung work to it directly. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/05-formulas)** — how multi-step work should be
  done: steps, dependencies, and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
