---
title: Tutorial 02 - Agents
sidebarTitle: 02 - Agents
description: Define agents and use them to execute work.
---

In [Tutorial 01](/tutorials/01-cities-and-rigs), you created a city, slung work to an
implicit agent, and added a rig. The implicit agents (`claude`, `codex`, etc.)
are convenient, but they have no custom prompt — they're just the raw provider.
In this tutorial, you'll define your own agents with specific roles and use them
to get work done.

We'll pick up where Tutorial 01 left off. You should have `my-city` running with
`my-project` rigged.

## Defining an agent

Open `city.toml`. You already have a `mayor` agent from the tutorial template.
Let's add a second agent that uses `codex` instead of `claude`:

```toml
[workspace]
name = "my-city"
provider = "claude"

... # context elided

[[agent]]
name = "reviewer"
dir = "my-project"
provider = "codex"
prompt_template = "prompts/reviewer.md"
```

You'll want to create a prompt for the new agent. Let's take a look at the
default GC prompt if you don't provide one:

```shell
~/my-city
$ gc prime
# Gas City Agent

You are an agent in a Gas City workspace. Check for available work
and execute it.

## Your tools

- `bd ready` — see available work items
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work

1. Check for available work: `bd ready`
2. Pick a bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.
```

The `gc prime` command let's an agent running in GC how to behave, specially how
to look for work that's been assigned to it. In [tutorial
01](/tutorials/01-cities-and-rigs), we learned that slinging work to an agent created a
bead. Looking here at the default prompt, it should be clear how the agent can
actually pick up work that was slung its way.

What we want to do is to preserve the instructions on how to be an agent in GC,
but also add the specifics for being a review agent. To do that, create the
reviewer prompt to look like the following:

```shell
~/my-city
$ cat > prompts/reviewer.md << 'EOF'
# Code Reviewer Agent
You are an agent in a Gas City workspace. Check for available work and execute it.

## Your tools
- `bd ready` — see available work items
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work
1. Check for available work: `bd ready`
2. Pick a bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF
$ gc prime my-project/reviewer
# Code Reviewer Agent
You are an agent in a Gas City workspace. Check for available work and execute it.
... # contents elided as identical to the above
```

Notice that use of `gc prime <agent-name>` to get the contents of your custom
prompt for that agent. That's a handy way to check on how the built-in agents or
your own custom agents are configured as you build out more of them over time.

If you wanted to get fancy, you could also set the model and permission mode:

```toml
...
[[agent]]
name = "reviewer"
dir = "my-project"
prompt_template = "prompts/reviewer.md"
option_defaults = { model = "sonnet", permission_mode = "plan" }
...
```

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
started a Codex session, loaded the prompt from `prompts/reviewer.md`, and
delivered the task to the rig-scoped reviewer. You can watch progress with `bd
show` as you already know. And when the work is done, you can check the file
system for the review you requested:

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

The exact review text will vary by provider and model. The important part is
that the reviewer creates `review.md` in the rig and fills it with review
feedback.

This is handy for fire-and-forget kind of work. However, if you'd like to see
the agent in action or even talk to one directly, you're going to need a
session. And for that, you'll want to check in on [the next
tutorial](/tutorials/03-sessions).

## What's next

You've defined agents with custom prompts, interacted with them through
sessions and configured different agents with different providers. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
