---
title: Tutorial 03 - Sessions
sidebarTitle: 03 - Sessions
description: See agent output, interact directly with agents, and learn about polecats and crew.
---

In [Tutorial 02](/tutorials/02-agents), you worked with agents to produce work,
which created sessions with agents that we haven't seen yet. In this tutorial,
you'll see and talk with agents via sessions as well as see how agents talk to
each other. You'll also learn the difference between "polecats" (agents spun up
on demand to handle work) and "crew" (persistent agents with named sessions),

To continue with this tutorial, start from where the last two tutorials left
off: the city root has `pack.toml` and `city.toml`, and Tutorial 02 added the
reviewer under `agents/reviewer/`:

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

The city's machine-local identity and the rig's path binding now live in
`.gc/site.toml` instead:

```toml
workspace_name = "my-city"

[[rig]]
name = "my-project"
path = "/Users/csells/my-project"
```

The reviewer's prompt lives at `agents/reviewer/prompt.template.md`. This is the
standard city shape: root config files plus per-agent directories under
`agents/`.

## Looking in on Polecats

Every provider — Claude, Codex, Gemini, etc. — has its own way of managing
conversations. Gas City normalizes all of that behind a single abstraction
called a **session**. A session is a live process with its own terminal, state,
and conversation history.

When you sling a bead, you're creating a session. For a transient polecat
session, the easiest way to inspect it is to look up the live session ID and
then pass that to `gc session peek`:

```shell
~/my-project
$ gc session list --template my-project/reviewer
ID       TEMPLATE  STATE     REASON  TITLE     AGE  LAST ACTIVE
mc-8sfd  my-project/reviewer  creating  create  reviewer  1s   -

~/my-project
$ gc session peek mc-8sfd
› [my-project] reviewer • 2026-04-07T11:56:59

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
  mp-p956, “Review hello.py and write review.md with feedback.” I’m pulling the
  bead details and the target file now, then I’ll write the review and close
  that item.

• Explored
  └ List rg --files -g hello.py -g review.md

• Ran bd show mp-p956 --json
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

If you want an agent to talk to, you'll want one configured for chatting
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
mc-8sfd  my-project/reviewer  creating  create          reviewer  1s   -
mc-5o1   mayor     active    session,config  mayor     10h  14m ago
```

However, once the work is done, the reviewer will go idle and its session will
be shutdown by GC. On the other hand, you can see from this sample output that
the mayor has been running for the last ten hours -- since our city was started
-- but we haven't talked to it once? Has it been burning tokens all of this
time? Let's take a look:

```shell
~/my-project
$ gc session peek mayor --lines 3

City is up and idle. No pending work, no agents running besides me. What would
  you like to do?
```

So the mayor is clearly idle, but has not been shutdown. Why not? If you take a
look again at your `pack.toml` file, you'll see why:

```toml
...
[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"

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
```

Gas City confirms the nudge with either `Nudged mayor` or `Queued nudge for mayor`.

![mayor nudge screenshot](mayor-nudge.png)

To get a feel for whats's happening in your city, you can see all running
sessions:

```shell
~/my-city
$ gc session list
ID      ALIAS  TEMPLATE  STATE
my-4    —      mayor     active
```

## Session logs

Peek shows the last few lines of terminal output. Logs show the full
conversation history:

```shell
~/my-city
$ gc session logs mayor --tail 1
07:22:29 [USER] [my-city] mayor • 2026-04-08T00:22:24
Check the status of mc-wisp-8t8

07:22:31 [ASSISTANT] [my-city] mayor • 2026-04-08T00:22:31
mc-wisp-8t8 is a review request for the auth module. I've routed it to
my-project/reviewer.
```

Note that `--tail` here counts compaction _segments_, not lines — `--tail 1`
shows the most recent segment, `--tail 0` shows all of them. Follow live output
with `-f`:

```shell
~/my-city
$ gc session logs mayor -f
```

In another terminal, nudge the mayor and watch the follow stream show the
conversation as it happens:

```shell
~/my-city
$ gc session nudge mayor "What's the current city status?"
```

Again, Gas City confirms the nudge with either `Nudged mayor` or `Queued nudge for mayor`.

Useful for watching what a background agent is doing without attaching and
potentially interrupting it. Peek shows the terminal; logs show the
conversation as new user and assistant messages arrive.

## What's next

You've seen how sessions are created on demand for slung work, how named
sessions keep crew agents alive, and how to peek, attach, nudge, and read logs.
From here:

- **[Agent-to-Agent Communication](/tutorials/04-communication)** — how agents
  coordinate through mail, slung work, and hooks
- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
