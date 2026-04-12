---
title: Tutorial 04 - Agent-to-Agent Communication
sidebarTitle: 04 - Communication
description: How agents coordinate through mail, slung work, and hooks — without direct connections.
---

In [Tutorial 03](/tutorials/03-sessions), you saw how to peek at agent output in
polecat sessions, attach to crew sessions, and nudge them with messages. All of
that was you talking to agents. This tutorial covers how agents talk to _each
other_.

We'll pick up where Tutorial 03 left off. You should have `my-city` running with
`my-project` rigged, and agents for `mayor` and `reviewer`.

## Agents talking to each other

Up to this point, you've been managing sessions one at a time — creating them on
demand for polecats, keeping with alive as crew with named sessions. But a city
isn't a collection of independent agents working in isolation. It's a system of
agents that can talk to each other.

The agents in your city don't call each other directly. There are no function
calls between them, no shared memory, no direct references. Each session is its
own process with its own terminal, its own conversation history, and its own
provider. The mayor doesn't have a handle to a polecat or vice versa.

However, they can still coordinate with each other via **mail** and **slung
work**. Both are indirect — the sender doesn't need to know which session
receives the message or which instance picks up the task. Gas City handles the
routing.

This indirection is deliberate. Because agents don't hold references to each
other, they can run, go idle, restart, and scale independently. The mayor can
dispatch work to `my-project/reviewer` without knowing whether there's one
reviewer session or five for that rig, whether it's on Claude or Codex, or
whether it's currently active or idle. The work and the messages persist in the
store. The sessions come and go.

Mail is the primary way agents talk to each other. Slung work — `gc sling` — is
how they delegate tasks. Let's look at both.

## Mail

Mail creates a persistent, tracked message that the recipient picks up on its
next turn. Unlike nudge (which is ephemeral terminal input), mail survives
crashes, has a subject line, and stays unread until the agent processes it.
Mail itself does not wake the recipient.

Send mail to the mayor:

```shell
~/my-city
$ gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"
Sent message mc-wisp-8t8 to mayor
```

`gc mail send` takes the recipient as a positional argument and the subject/body
via `-s`/`-m` flags. (You can also pass just `<to> <body>` with no subject.)

Check for unread mail:

```shell
~/my-city
$ gc mail check mayor
1 unread message(s) for mayor
```

See the inbox:

```shell
~/my-city
$ gc mail inbox mayor
ID           FROM   SUBJECT        BODY
mc-wisp-8t8  human  Review needed  Please look at the auth module changes in my-project
```

`gc mail inbox` defaults to unread messages, so there's no STATE column —
everything listed is unread by definition.

If you want to see the mayor react right away in `peek` or `logs`, give it a
turn:

```shell
~/my-city
$ gc session nudge mayor "Check mail and hook status, then act accordingly"
Nudged mayor
```

The mayor doesn't have to manually check its inbox. Gas City installs provider
hooks that surface unread mail automatically — on each turn, a hook runs `gc
mail check --inject`, and if there's unread mail, it appears as a system
reminder in the agent's context. The agent sees its mail without doing anything.

That nudge does not deliver the mail by itself — it just wakes the mayor so a
new turn starts. When the mayor wakes up or starts a new turn, hooks deliver
any pending mail, and the nudge tells it to act on what it finds.

## Slinging beads to coordinate agents

Here's what coordination looks like in practice. Once the mayor takes a turn, it
reads the mail message you sent. It decides the reviewer should handle it, so
it slings the work:

```shell
~/my-city
$ gc session peek mayor --lines 6
[mayor] Got mail: "Review needed" — auth module changes in my-project
[mayor] Routing to my-project/reviewer...
[mayor] Running: gc sling my-project/reviewer "Review the auth module changes"
```

(The above is illustrative — `peek` returns the actual terminal contents of the
session, so you'll see whatever the agent has rendered, not Gas City–formatted
lines.)

The mayor didn't talk to the reviewer directly. It slung a bead to the
`my-project/reviewer` agent template, and Gas City figured out which session
picks it up. If the reviewer was asleep, Gas City woke it. If there were
multiple reviewer sessions for that rig, Gas City routed the work to an
available one. The mayor doesn't know or care about any of that — it describes
the work and slings it.

This is the pattern that scales. A human sends mail to the mayor. The mayor
reads it, plans the work, and slings tasks to agents. Those agents do the work
and close their beads. Everyone communicates through the store, not through
direct connections. Sessions come and go; the work persists.

## Hooks

Hooks are what make all of this work behind the scenes. Without hooks, a session
is just a bare provider process — Claude running in a terminal, with no
awareness of Gas City. Hooks wire the provider's event system into Gas City so
agents can receive mail, pick up slung work, and drain queued nudges
automatically.

The tutorial template sets hooks at the workspace level, so all your agents
already have them:

```toml
[workspace]
install_agent_hooks = ["claude"]
```

You can also set them per agent:

```toml
[[agent]]
name = "mayor"
install_agent_hooks = ["claude"]
```

When a session starts, Gas City installs hook configuration files that the
provider reads. For Claude, this means a `hooks/claude.json` file that fires Gas
City commands at key moments — session start, before each turn, on shutdown.
Those commands deliver mail, drain nudges, and surface pending work.

Without hooks, you'd have to manually tell each agent to run `gc mail check` and
`gc prime`. With hooks, it happens on every turn.

## What's next

You've seen the two coordination mechanisms — mail for messages and slung beads
for work — and the hook infrastructure that wires it all together. From here:

- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
