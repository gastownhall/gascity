---
title: Tutorial 03 - Sessions
sidebarTitle: 03 - Sessions
description: Manage session lifecycle, sleep and wake, named sessions, mail, and inter-agent communication.
---

In [Tutorial 02](/tutorials/02-agents), you created sessions with `gc session new`, attached and detached from them, peeked at their output, and nudged them with messages. All of that was about interacting with a single running session. This tutorial goes deeper — what happens when sessions go idle, how to keep important ones alive automatically, and how agents communicate with each other.

We'll pick up where Tutorial 02 left off. You should have `my-city` running with `my-project` and `my-api` rigged, and agents for `mayor`, `helper`, `worker`, and `reviewer`.

## Session states

Let's look at what's running right now:

```shell
~/my-city
$ gc session list
ID      TEMPLATE  STATE   REASON          TITLE   AGE  LAST ACTIVE
mc-io4  helper    active  session,config  helper  10m  2m ago
mc-2k1  helper    active  session,config  hal     8m   1m ago
mc-3p9  mayor     active  session,config  mayor   55m  25m ago
```

Every session has a state. So far you've only seen `active` — the session is running and accepting input. But sessions don't stay active forever. Let's see what happens when you explicitly pause one:

```shell
~/my-city
$ gc session suspend hal
Session mc-2k1 suspended. Resume with: gc session wake mc-2k1

~/my-city
$ gc session list
ID      TEMPLATE  STATE      REASON          TITLE   AGE  LAST ACTIVE
mc-io4  helper    active     session,config  helper  10m  2m ago
mc-2k1  helper    suspended  user-hold       hal     8m   -
mc-3p9  mayor     active     session,config  mayor   55m  25m ago
```

The `REASON` column shows *why* the session is in its current state — `user-hold` means you put it there. The session is still there, but it's not running — no process, no resources. It won't respond to nudges or pick up work until you bring it back:

```shell
~/my-city
$ gc session wake hal
Session mc-2k1: hold and quarantine cleared.
```

`wake` clears the user-hold and the reconciler brings the session back to active. You can also attach directly to a suspended session — Gas City wakes it automatically as part of the attach.

## Sleep and wake

Suspend is manual — you told the session to stop. Sleep is automatic. When an agent sits idle long enough, Gas City puts it to sleep on its own.

To indicate how long you want the system to wait, just add `idle_timeout` to the mayor agent in `city.toml`:

```toml
[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"
idle_timeout = "1h"
```

The controller picks up `idle_timeout` changes live — no restart needed.

After an hour of no activity, the mayor's session goes to sleep. Same as suspended — no process, no resources — but the difference is important: a sleeping session wakes automatically when something needs it. Nudge it, sling work to it, or attach to it, and Gas City brings it back. A suspended session stays suspended until you explicitly wake it.

```shell
~/my-city
$ gc session list --state all
ID      TEMPLATE  STATE      REASON       TITLE   AGE  LAST ACTIVE
mc-io4  helper    active     session      helper  2h   5m ago
mc-2k1  helper    active     session      hal     2h   3m ago
mc-3p9  mayor     suspended  idle-asleep  mayor   2h   1h ago
```

```shell
~/my-city
$ gc session nudge mayor "Any open tasks?"
Nudged mayor
```

If the target was asleep, the nudge wakes it as part of delivery — there's no separate "waking" message; the reconciler brings it back transparently.

When a session wakes, the provider restores the conversation. By default, Gas City reuses the provider's session key — Claude does this with `--resume`, so the agent picks up where it left off with full conversation history.

## Closing sessions

When you're done with a session and don't need it anymore, close it:

```shell
~/my-city
$ gc session close hal
Closed session hal
```

Close is graceful — the session finishes any in-flight work before shutting down. If a session is misbehaving and you need it gone immediately, use `gc session kill <id>` to force-kill the runtime process. The bead stays marked active, so the reconciler will detect the dead process and restart it according to the session's lifecycle rules — useful for unsticking a wedged session without losing its conversation history.

Over time, closed and suspended sessions accumulate. You can check with `gc session list --state all` — it shows sessions in every state, not just active ones. Clean them up with prune:

```shell
~/my-city
$ gc session prune --before 7d
```

This closes any suspended session that's been idle for more than the specified duration.

## Named sessions

Everything so far has been about sessions you create manually with `gc session new`. But some agents — like a coordinator — should always be running. You don't want to have to remember to start the mayor every time the city comes up.

Named sessions handle this. Add one to `city.toml`:

```toml
[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"
nudge = "Check mail and hook status, then act accordingly."
idle_timeout = "1h"

[[named_session]]
template = "mayor"
scope = "city"
mode = "always"
```

Restart the city to pick up the named session:

```shell
~/my-city
$ gc restart
```

Now the controller ensures the mayor is always running. If it crashes, the controller restarts it. If the city starts, the mayor starts with it:

```shell
~/my-city
$ gc session list
ID      TEMPLATE  STATE   REASON          TITLE  AGE  LAST ACTIVE
mc-io4  mayor     active  session,config  mayor  5s   -
```

There are two modes for named sessions:

- **`always`** — the controller keeps this session running. If it dies, it comes back.
- **`on_demand`** — reserves the identity but doesn't auto-start. The session gets created when something needs it — slung work, a nudge, an attach — and sleeps after its idle timeout.

Let's add on-demand workers:

```toml
[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
```

Restart the city:

```shell
~/my-city
$ gc restart
```

Right after restart, only the mayor is running — the workers are on-demand, so they're waiting:

```shell
~/my-city
$ gc session list
ID      TEMPLATE  STATE   REASON          TITLE  AGE  LAST ACTIVE
mc-io4  mayor     active  session,config  mayor  5s   -
```

Now sling some work to a worker:

```shell
~/my-city
$ gc sling my-project/worker "Add input validation to the API"
Created mp-6ou — "Add input validation to the API"
Auto-convoy mp-tje
Slung mp-6ou → my-project/worker
```

```shell
~/my-city
$ gc session list
ID      TEMPLATE  STATE   REASON          TITLE   AGE  LAST ACTIVE
mc-io4  mayor     active  session,config  mayor   2m   30s ago
mp-3w1  worker    active  session,config  worker  4s   -
```

The worker session was created on demand to handle the work. Once it finishes and sits idle, it'll go to sleep. The mayor stays running. You've gone from manually creating sessions to having Gas City manage them for you.

## Dependencies

The mayor is a coordinator — it plans work and dispatches it to workers. If a worker starts before the mayor is ready, it might try to check in and find nobody home. Dependencies prevent that:

```toml
[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
depends_on = ["mayor"]
```

The controller won't start any worker session until the mayor is running. This is about startup ordering, not a permanent link — if the mayor sleeps and wakes, workers don't need to restart.

The controller picks up `depends_on` changes live — no restart needed.

## Communicating processes

>***donna** Chris, I get the general model for tutorials. I think the communicating processes thing is pretty imortant to land for anoob. I'd love to chat before we do much more slicing adn dicing.*

Up to this point, you've been managing sessions one at a time — creating them, suspending them, keeping them alive with named sessions. But a city isn't a collection of independent agents working in isolation. It's a system of communicating processes.

The agents in your city don't call each other directly. There are no function calls between them, no shared memory, no direct references. Each session is its own process with its own terminal, its own conversation history, and its own provider. The mayor doesn't have a handle to the worker. The worker doesn't know the reviewer exists.

So how do they coordinate?

Through two mechanisms: **mail** and **slung work**. Both are indirect — the sender doesn't need to know which session receives the message or which instance picks up the task. Gas City handles the routing.

This indirection is deliberate. Because agents don't hold references to each other, they can crash, restart, sleep, and scale independently. The mayor can dispatch work to "the reviewer" without knowing whether there's one reviewer session or five, whether it's on Claude or Codex, or whether it's currently active or asleep. The work and the messages persist in the store. The sessions come and go.

Mail is the primary way agents talk to each other. Slung work — `gc sling` — is how they delegate tasks. Let's look at both.

## Mail

Mail creates a persistent, tracked message that the recipient picks up on its next turn. Unlike nudge (which is ephemeral terminal input), mail survives crashes, has a subject line, and stays unread until the agent processes it.

Send mail to the mayor:

```shell
~/my-city
$ gc mail send mayor -s "Review needed" -m "Please look at the auth module changes in my-project"
Sent message mc-wisp-8t8 to mayor
```

`gc mail send` takes the recipient as a positional argument and the subject/body via `-s`/`-m` flags. (You can also pass just `<to> <body>` with no subject.)

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

`gc mail inbox` defaults to unread messages, so there's no STATE column — everything listed is unread by definition.

The mayor doesn't have to manually check its inbox. Gas City installs provider hooks that surface unread mail automatically — on each turn, a hook runs `gc mail check --inject`, and if there's unread mail, it appears as a system reminder in the agent's context. The agent sees its mail without doing anything.

This is what the mayor's nudge — "Check mail and hook status, then act accordingly" — is about. When the mayor wakes up or starts a new turn, hooks deliver any pending mail, and the nudge tells it to act on what it finds.

## How agents coordinate

Here's what coordination looks like in practice. The mayor reads the mail message you sent. It decides the reviewer should handle it, so it slings the work:

```shell
~/my-city
$ gc session peek mayor --lines 6
[mayor] Got mail: "Review needed" — auth module changes in my-project
[mayor] Routing to reviewer...
[mayor] Running: gc sling my-project/reviewer "Review the auth module changes"
```

(The above is illustrative — `peek` returns the actual terminal contents of the session, so you'll see whatever the agent has rendered, not Gas City–formatted lines.)

The mayor didn't talk to the reviewer directly. It slung a bead to the reviewer agent template, and Gas City figured out which session picks it up. If the reviewer was asleep, Gas City woke it. If there were multiple reviewer sessions, Gas City routed the work to an available one. The mayor doesn't know or care about any of that — it describes the work and slings it.

This is the pattern that scales. A human sends mail to the mayor. The mayor reads it, plans the work, and slings tasks to agents. Those agents do the work and close their beads. Everyone communicates through the store, not through direct connections. Sessions come and go; the work persists.

## Hooks

Hooks are what make all of this work behind the scenes. Without hooks, a session is just a bare provider process — Claude running in a terminal, with no awareness of Gas City. Hooks wire the provider's event system into Gas City so agents can receive mail, pick up slung work, and drain queued nudges automatically.

The tutorial template sets hooks at the workspace level, so all your agents already have them:

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

When a session starts, Gas City installs hook configuration files that the provider reads. For Claude, this means a `hooks/claude.json` file that fires Gas City commands at key moments — session start, before each turn, on shutdown. Those commands deliver mail, drain nudges, and surface pending work.

Without hooks, you'd have to manually tell each agent to run `gc mail check` and `gc prime`. With hooks, it happens on every turn.

## Session logs

Peek shows the last few lines of terminal output. Logs show the full conversation history:

```shell
~/my-city
$ gc session logs mayor --tail 1
07:22:29 [USER] [my-city] mayor • 2026-04-08T00:22:24
Check the status of mc-wisp-8t8

07:22:31 [ASSISTANT] [my-city] mayor • 2026-04-08T00:22:31
mc-wisp-8t8 is a review request for the auth module. I've routed it to
my-project/reviewer.
```

Note that `--tail` here counts compaction *segments*, not lines — `--tail 1` shows the most recent segment, `--tail 0` shows all of them. Follow live output with `-f`:

```shell
~/my-city
$ gc session logs mayor -f
```

Useful for watching what a background agent is doing without attaching and potentially interrupting it. Peek shows the terminal; logs show the conversation.

## What's next

You've seen how sessions move through states, how named sessions keep agents alive, how mail and hooks enable agents to coordinate as a system, and how to manage session lifecycle. From here:

- **[Formulas](/tutorials/04-formulas)** — multi-step workflow templates with dependencies and variables
- **[Beads](/tutorials/05-beads)** — the work tracking system underneath it all

{/* BONEYARD — material for reference docs or future tutorials.

See fodder/sessions.md for the previous version.

### Nudge delivery modes

Three modes for gc session nudge --delivery:
- immediate (default) — sends now, wakes sleeping sessions
- wait-idle — waits for agent to finish current turn
- queue — queued, survives crashes, retried until delivered

### Startup lifecycle

1. pre_start — shell commands before session creation
2. Session creation — provider starts in tmux
3. session_setup — shell commands after creation
4. session_setup_script — script with GC_SESSION env var
5. session_live — idempotent commands re-applied on config change
6. overlay_dir — directory copied into workspace

### Pools

```toml
[[agent]]
name = "polecat"
min_active_sessions = 0
max_active_sessions = 5
namepool = "namepools/names.txt"
```

Pools let an agent scale up and down based on demand.

### Resume command

```toml
[[agent]]
name = "mayor"
resume_command = "claude --resume {{.SessionKey}} --dangerously-skip-permissions"
```

### Drain timeout

```toml
[[agent]]
name = "polecat"
drain_timeout = "30m"
```

Defaults to "5m". Controller waits this long before force-killing during scale-down.

### Boot and death hooks

```toml
[[agent]]
name = "helper"
on_boot = "scripts/init-workspace.sh"
on_death = "scripts/cleanup.sh"
```

### Quarantine

Sessions that crash repeatedly (hit the crash-loop threshold) are quarantined —
temporarily blocked from waking. Prevents runaway restart loops. */}
