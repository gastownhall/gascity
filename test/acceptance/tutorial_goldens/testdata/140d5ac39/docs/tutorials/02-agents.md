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

Open `city.toml`. You already have a `mayor` agent from the minimal template.
Let's add a second agent that uses `codex` instead of `claude`:

```toml
[workspace]
name = "my-city"
provider = "claude"

... # context elided

[[agent]]
name = "reviewer"
provider = "codex"
prompt_template = "prompts/reviewer.md"
```

You'll want to create a prompt for the new agent. Let's take a look at the
default GC prompt if you don't provide one:

```shell
~/my-city
$ gc prime
# Gas City Agent

You are an agent in a Gas City workspace. Find assigned work, claim it
atomically when needed, execute it, close it, and drain when idle.

## Your tools

- `bd list --assignee="$GC_SESSION_NAME" --status=in_progress --json` — resume work already claimed by this session
- `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1` — find assigned ready work
- `gc hook` — find routed pool work
- `bd update <id> --claim` — atomically claim an unassigned bead
- `bd show <id> --json` — verify claim and inspect metadata
- `gc runtime drain-ack` — tell the controller this session is idle and can stop

## Startup and Claim Protocol

1. Check assigned in-progress work, then assigned ready work.
2. If none exists, run `gc hook`.
3. If `gc hook` returns an unassigned bead, claim it first:
   `bd update <id> --claim`
4. Verify the bead is assigned to `$GC_SESSION_NAME` with `bd show <id> --json`.
5. If metadata has `gc.continuation_group` and `gc.root_bead_id`, pre-assign
   open sibling beads in that root/group to `$GC_SESSION_NAME`.
6. Execute the claimed bead's description and close it.
7. If no assigned continuation work is ready, run `gc runtime drain-ack && exit`.
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
You are an agent in a Gas City workspace. Claim routed work before executing it.

## Your tools
- `gc hook` — find routed work
- `bd update <id> --claim` — atomically claim unassigned work
- `bd show <id> --json` — verify assignee and metadata
- `bd close <id>` — mark work done
- `gc runtime drain-ack` — end the session when idle

## How to work
1. Check assigned work: `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1`
2. If none is assigned, run `gc hook`
3. Claim unassigned routed work with `bd update <id> --claim`
4. Verify `assignee` and `gc.continuation_group` metadata with `bd show <id> --json`
5. Review the code, write the requested feedback, and close the bead
6. If no assigned continuation work is ready, run `gc runtime drain-ack && exit`

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF
$ gc prime reviewer
# Code Reviewer Agent
You are an agent in a Gas City workspace. Claim routed work before executing it.
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
prompt_template = "prompts/reviewer.md"
option_defaults = { model = "sonnet", permission_mode = "plan" }
...
```

Now that your agent is available, it's time to sling some work to it:

```shell
~/my-city
$ cd ~/my-project
~/my-project
$ gc sling reviewer "Review hello.py and write review.md with feedback"
Created mc-p956 — "Review hello.py and write review.md with feedback"
Auto-convoy mc-4wdl
Slung mc-p956 → reviewer
```

Your new reviewer agent picks up the work automatically. Gas City started a
Codex session, loaded the prompt from `prompts/reviewer.md`, and delivered the
task. You can watch progress with `bd show` as you already know. And when the
work is done, you can check the file system for the review you requested:

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

You've defined agents with custom prompts, interacted with them through
sessions and configured different agents with different providers. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
