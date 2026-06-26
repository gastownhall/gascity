---
title: Tutorial 03 - Sessions
sidebarTitle: 03 - Sessions
description: See agent output, interact directly with agents, and learn the difference between on-demand and always-on sessions.
---

Slinging work to agents creates **sessions** — live agent processes you
haven't seen yet. This tutorial shows you how to watch and talk to them. A
session comes in two flavors:

| Flavor | Lifecycle | Declared by |
| --- | --- | --- |
| **On-demand** | Spun up to handle slung work, shut down when idle | nothing — the default |
| **Always-on** | Kept alive so you can chat anytime | `[[named_session]] mode="always"` in the local pack |

A pack might give these flavors its own role names, but those are pack
configuration, not Gas City concepts. The underlying primitive is the session.

This tutorial continues from the city the last two left off with: `pack.toml`
and `city.toml` at the root, plus the reviewer Tutorial 02 added under
`agents/reviewer/`.

```shell
~/my-city
$ cat pack.toml
[pack]
name = "my-city"
schema = 2

[[named_session]]
template = "mayor"
mode = "always"

~/my-city
$ cat city.toml
[workspace]
provider = "claude"

... # content elided

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

## Looking in on an on-demand session

Every provider — Claude, Codex, Gemini — manages conversations its own way. Gas
City normalizes all of that behind the **session**: a live process with its own
terminal, state, and conversation history.

Slinging a bead to an agent that isn't running creates a session. To inspect a
live one, look up its ID with `gc session list`, then pass that to `gc session
peek`. (If the reviewer already finished Tutorial 02's review and its session
was cleaned up, sling the same review work again for a fresh live session to
watch.)

```shell
~/my-project
$ gc session list --template my-project/reviewer
ID       TEMPLATE              STATE     REASON  TARGET        TITLE     AGE  LAST ACTIVE
mc-8sfd  my-project/reviewer   creating  create  reviewer-a1b  reviewer  1s   -

~/my-project
$ gc session peek mc-8sfd
› [my-city] my-project/reviewer • 2026-04-07T11:56:59

  Run `gc prime` to initialize your context.

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

... # content elided

• Ran gc hook --claim --json
  └ warning: beads.role not configured (GH#2950).
      Fix: git config beads.role maintainer
    … +282 lines
      }
    ]

• The hook claimed one task that is explicitly routed to the reviewer agent:
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

The peek shows the reviewer's prompt — the output of `gc prime` — landing as
the first input to the `codex` CLI. That's how GC tells Codex how to act. Codex
then claims a ready bead, executes it, and produces `review.md`.

When the work runs out, the agent goes idle and the GC supervisor shuts its
session down. That's the on-demand default: an agent with no always-on
declaration executes beads, goes idle, and is cleaned up. You could chat with
one while it's alive, but it won't stick around for you.

For an agent that's always there to talk to, you want an always-on session,
declared by the local pack.

## Chatting over an always-on session

`gc session list` shows both flavors side by side — the reviewer mid-work, and
the mayor up for ten hours:

```shell
~/my-project
$ gc session list
ID       TEMPLATE              STATE     REASON          TARGET        TITLE     AGE  LAST ACTIVE
mc-8sfd  my-project/reviewer   creating  create          reviewer-a1b  reviewer  1s   -
mc-5o1   mayor                 active    session,config  mayor         mayor     10h  14m ago
```

The reviewer will go idle and be shut down once its work is done. The mayor has
been up since the city started, yet nobody has chatted with it — is it burning
tokens? Peek says no:

```shell
~/my-project
$ gc session peek mayor --lines 3

City is up and idle. No pending work, no agents running besides me. What would
  you like to do?
```

Idle but not shut down. The mayor survives because the local pack — your
`pack.toml`, the root pack that imports the others — declares an always-on
named session for it:

```toml
[[named_session]]
template = "mayor"
mode = "always"
```

That `mode="always"` keeps the session running so the mayor is always around to
chat, plan, or receive work. Any agent the local pack (or a pack it imports)
declares this way gets the same treatment; everything else runs on demand.

To talk to the mayor — or any agent in a running session — attach to it:

```shell
~/my-project
$ gc session attach mayor
Attaching to session mc-5o1 (mayor)...
```

And as soon as you do, you'll be dropped into [a tmux
session](https://github.com/tmux/tmux/wiki/Getting-Started):

![mayor session screenshot](mayor-session.png)

You're in a live conversation. The agent responds just like any chat-based
coding assistant, but with the full context of its prompt template.

To detach without killing the session, press `Ctrl-b d` (the standard tmux
detach). The session keeps running in the background. You can reattach anytime.

You can also reach a running session without attaching. Besides peeking, you
can **nudge** it — type a new message into its terminal:

```shell
~/my-city
$ gc session nudge mayor "What's the current city status?"
Nudged mayor   # or "Queued nudge for mayor" if the session isn't ready yet
```

![mayor nudge screenshot](mayor-nudge.png)

## Session logs

Peek shows the last few lines of terminal output. Logs show the full
conversation history:

```shell
~/my-city
$ gc session logs mayor --tail 2
22:07:29 [USER] What's the current city status?
22:07:38 [ASSISTANT] City is up and idle. No pending work, no agents running besides me.
```

`--tail N` prints the last N transcript entries (same convention as `tail -n`),
so `--tail 2` shows the most recent prompt and reply. Use `--tail 0` for the
whole conversation. Follow live output with `-f`:

```shell
~/my-city
$ gc session logs mayor -f
```

Now nudge the mayor from another terminal and the follow stream prints the
exchange as it arrives — handy for watching a background agent without
attaching and risking an interruption.

<Accordion title="Edge: how --tail counts entries">
A compact-boundary divider counts as an entry if one lands inside the final
window. As of 1.0, `--tail` counts displayed transcript entries; before 1.0 it
counted compaction segments. The HTTP API's `tail` query parameter still counts
compaction segments.
</Accordion>

## What's next

You've created sessions on demand, kept the mayor alive with an always-on
`[[named_session]]`, and used peek, attach, nudge, and logs to watch and talk to
agents. From here:

- **[The six primitives](/getting-started/how-gas-city-works)** — the canonical model the
  session and these mechanisms build on
- **[Agent-to-Agent Communication](/tutorials/04-communication)** — how agents
  coordinate through mail, slung work, and hooks
- **[Formulas](/tutorials/05-formulas)** — how multi-step work should be
  done: steps, dependencies, and variables
- **[Beads](/tutorials/06-beads)** — the work tracking system underneath it all
