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

You'll want to create a prompt for the new agent. Let's first see what
`gc prime` returns when you don't name an agent — without an agent argument,
it falls back to the managed-session worker prompt used when the runtime cannot
resolve a more specific agent prompt:

```shell
~/my-city
$ gc prime
# Gas City Agent

You are an agent in a Gas City workspace. Find assigned work, claim it
atomically when needed, execute it, close it, and drain when idle.

This fallback prompt is for a managed runtime session. If $GC_SESSION_NAME is empty,
do not run this protocol; use a named agent prompt or direct bd commands for
manual work instead.

## Your tools

- `bd list --assignee="$GC_SESSION_NAME" --status=in_progress --json` — resume work already claimed by this session
- `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1` — find assigned ready work
- `gc hook` — find routed pool work
- `bd update <id> --claim` — atomically claim an unassigned bead
- `bd show <id> --json` — verify claim and inspect metadata
- `bd close <id>` — mark work done when no outcome metadata is required
- `gc runtime drain-ack` — tell the controller this session is idle and can stop

## Startup and Claim Protocol

1. First check for work already assigned to this session:
   `bd list --assignee="$GC_SESSION_NAME" --status=in_progress --json`
2. If none, check for assigned ready work:
   `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1`
3. If none, run `gc hook` for routed pool work.
4. If `gc hook` returns an unassigned bead, claim it before doing anything else:
   `bd update <id> --claim`
   If the claim command fails, another session won the race. Do not work that
   bead; run `gc hook` again or drain if no valid work remains.
5. Verify the claimed bead before doing work:
   `bd show <id> --json`
   The assignee must be `$GC_SESSION_NAME`. If `$GC_TEMPLATE` is set,
   `gc.routed_to` or `gc.run_target` must match it.
6. If the bead metadata has `gc.continuation_group` and `gc.root_bead_id`,
   pre-assign only unassigned sibling beads in the same root, continuation
   group, and route so the workflow continues in this live session:
   If `$GC_TEMPLATE` is empty, skip sibling pre-assignment.
   `bd list --metadata-field gc.routed_to="$GC_TEMPLATE" --metadata-field gc.root_bead_id=<root> --metadata-field gc.continuation_group=<group> --status=open --no-assignee --json`
   If the claimed bead used `gc.run_target` without `gc.routed_to`,
   use `--metadata-field gc.run_target="$GC_TEMPLATE"` instead.
   Then `bd update <sibling-id> --assignee="$GC_SESSION_NAME"` for each sibling.
   Never assign a sibling already assigned to another session or another route.
7. Execute exactly the claimed bead's description.
8. Close the bead when done. If the workflow expects explicit outcome
   metadata, set it before closing; otherwise `bd close <id>` is enough.
9. After closing, check `bd ready --assignee="$GC_SESSION_NAME" --json --limit=1`
   once for continuation work. If none is ready, run:
   `gc runtime drain-ack && exit`

Do not keep scanning the global queue after your assigned work is complete.
The controller will start another session when more work is available.
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
$ gc prime my-project/reviewer
# Code Reviewer Agent
You are an agent in a Gas City workspace. Claim routed work before executing it.
... # contents elided as identical to the above
```

Notice that use of `gc prime <agent-name>` to get the contents of your custom
prompt for that agent. That's a handy way to check on how the built-in agents or
your own custom agents are configured as you build out more of them over time.

If you wanted to get fancy, you could also set the model and permission mode:

```toml
dir = "my-project"
provider = "codex"
option_defaults = { model = "sonnet", permission_mode = "plan" }
```

That file would live at `agents/reviewer/agent.toml`.

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
reviewer. You can watch progress with `bd show` as you already know. And when
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

You've defined agents with custom prompts, interacted with them through
sessions and configured different agents with different providers. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
