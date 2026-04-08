---
title: Tutorial 02 - Agents
sidebarTitle: 02 - Agents
description: Define agents with custom prompts and providers, interact through sessions, and configure scope and working directories.
---

In [Tutorial 01](/tutorials/01-cities), you created a city, slung work to an
implicit agent, and added a rig. The implicit agents (`claude`, `codex`, etc.)
are convenient, but they have no custom prompt — they're just the raw provider.
In this tutorial, you'll define your own agents with specific roles, interact
with them through sessions, and see how scope and working directories keep
things organized.

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
01](/tutorials/01-cities), we learned that slinging work to an agent created a
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
$ gc prime reviewer
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
session.

## Looking in on Polecats

Every provider — Claude, Codex, Gemini, etc. — has its own way of managing
conversations. Gas City normalizes all of that behind a single abstraction
called a **session**. A session is a live process with its own terminal, state,
and conversation history.

When you sling a bead, you're creating a session. You can peek at what's
happening in that session with the `gc session peek` command, passing in the
name of the agent you'd like to check in on:

```shell
~/my-project
$ gc session peek reviewer
› [my-city] reviewer • 2026-04-07T11:56:59

  Run `gc prime` to initialize your context.

  # Code Reviewer Agent
  You are an agent in a Gas City workspace. Check for available work and
  execute it.

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

... # content elided

• Ran bd ready --json
  └ warning: beads.role not configured (GH#2950).
      Fix: git config beads.role maintainer
    … +282 lines
      }
    ]

• The ready queue has one task that is explicitly routed to the reviewer agent:
  mc-p956, “Review hello.py and write review.md with feedback.” I’m pulling the
  bead details and the target file now, then I’ll write the review and close
  that item.

• Explored
  └ List rg --files -g hello.py -g review.md

• Ran bd show mc-p956 --json
  └ [
      {
    … +27 lines
      }
    ]

• Working (43s • esc to interrupt)
```

You'll notice that the result of `gc prime` for our reviewer agent as the first
input to the `codex` CLI. That's how GC lets Codex know how to act. Then you'll
notice Codex acting on those instructions by looking for the beads that are
ready for it to act on. It finds one, executes it and out comes our `review.md`
file.

When an agent has no work to do, it will go idle. And when it's been idle in a
session created for it to handle work that was slung to it, that session will be
cleanly shutdown by the GC supervisor process. These transient sessions are
often used by one-and-done agents know as "polecats". While you could talk to
one interactively, they're configured to execute beads, go idle and have their
sessions shutdown ASAP.

If you want an agent to to talk to, you'll want one configured for chatting
called a "crew" member.

## Chatting with Crew

Recall from our reviewer agent that it's prompt was authored to ask it to look
for and immediately start executing work assigned to it. While that work is
active, you can see it in the list of sessions:

```shell
~/my-project
$ gc session list
2026/04/07 21:50:21 tmux state cache: refreshed 2 sessions in 3.82725ms
ID       TEMPLATE  STATE     REASON          TITLE     AGE  LAST ACTIVE
mc-8sfd  reviewer  creating  create          reviewer  1s   -
mc-5o1   mayor     active    session,config  mayor     10h  14m ago
```

However, once the work is done, the reviewer will go idle and its session will
be shutdown by GC. On the other hand, you can see from this sample output that
the mayor has been running for the last ten hours -- since our city was started
-- but we haven't talked to it once? Has it been burning tokens all of this time?
Let's take a look:

```shell
~/my-project
$ gc session peek mayor --lines 3

City is up and idle. No pending work, no agents running besides me. What would
  you like to do?
```

So the mayor is clearly idle, but has not been shutdown. Why not? If you take a
look again at your `city.toml` file, you'll see why:

```toml
...
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[named_session]]
template = "mayor"
mode = "always"
...
```

The mayor has a specially named session called "mayor" that is always running.
It's kept up but the system so that you can have quick access to it for a chat
or some planning or whatever you'd like to do. A polecat is designed to be
transient, but an agent is a member of your "crew" (whether city-wide or
rig-specific) if it's always around and ready to chat interactively or receive
work.

To talk to the mayor (or any agent in a running session), you "attach" to it:

```shell
~/my-project
$ gc session attach mayor
2026/04/07 22:03:26 tmux state cache: refreshed 1 sessions in 3.828541ms
Attaching to session mc-5o1 (mayor)...
```

And as soon as you do, you'll be dropped into [a tmux
session](https://github.com/tmux/tmux/wiki/Getting-Started):

![mayor session screenshot](mayor-session.png)

You're in a live conversation. The agent responds just like any chat-based
coding assistant, but with the full context of its prompt template.

To detach without killing the session, press `Ctrl-b d` (the standard tmux
detach). The session keeps running in the background. You can reattach anytime.

You can also interact with running sessions without attaching. You've already
seen what peeking looks like. You can also "nudge" it, which types a new message
into the session's terminal:

```shell
~/my-city
$ gc session nudge mayor "What's the current city status?"
2026/04/07 22:07:28 tmux state cache: refreshed 2 sessions in 3.765375ms
Nudged mayor
```

![mayor nudge screenshot](mayor-nudge.png)

There are lots more things to learn about sessions in the next tutorial.

## What's next

You've defined agents with custom prompts, interacted with them through
sessions and configured different agents with different providers. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/04-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/05-beads)** — the work tracking system underneath it all
