---
title: Quickstart
description: Create a city, add a rig, and send work through a formula in a few minutes.
---

<Note>
This guide assumes you have already installed Gas City and its
prerequisites. If you haven't, start with the
[Installation](/getting-started/installation) page.
</Note>

You need `gc` (`brew install gascity` brings `bd`, `dolt`, `tmux`, `jq`, and
`flock` with it) and a coding agent CLI that is already logged in. The examples
use Claude Code.

<Tip>
Oh My Zsh's `git` plugin defines a `gc` alias for `git commit --verbose`. If
`gc version` or `gc init` opens git commit instead of Gas City, use
`command gc ...` temporarily and remove the alias after Oh My Zsh loads.
See [Troubleshooting](/getting-started/troubleshooting#oh-my-zsh-git-plugin-hides-gc).
</Tip>

## 1. Give Dolt your identity

Gas City stores work as beads in Dolt, and Dolt needs an author. `gc init`
stops with "startup is blocked by Dolt author identity" without one.

Git needs an identity first: the commands below copy it, and step 3 commits
with it. If `git config --global user.name` or `git config --global user.email`
prints nothing, set it with `git config --global user.name "Your Name"` or
`git config --global user.email "you@example.com"`, using your own name and
email.

Then copy your git identity into Dolt wherever Dolt's name or email is missing
or empty:

```bash
[ -n "$(dolt config --global --get user.name)" ]  || dolt config --global --add user.name  "$(git config --global user.name)"
[ -n "$(dolt config --global --get user.email)" ] || dolt config --global --add user.email "$(git config --global user.email)"
```

## 2. Create a City

```bash
gc init ~/my-city --template gascity --default-provider claude
```

`gc init` bootstraps the city directory, registers it with the supervisor, and
starts the orchestrator in the background. The defaults are the recommended
setup; use `--default-provider codex` (or `gemini`) for a different agent CLI.
The dashboard is at `http://127.0.0.1:8372/`.

A new city takes a few minutes to finish setting up. Until it does, these
signals are normal and need no action:

- `gc init`'s output can end at `Adopting sessions...`.
- `gc status` can list agents as `unknown (partial status)` and print
  `bead cache unavailable` or `session snapshot timed out`.
- `gc doctor` can report the store as not running.
- A dashboard run page can say "run projection is warming"; refresh it after a
  minute.

## 3. Add a Rig

```bash
mkdir -p ~/hello-world && cd ~/hello-world
git init && git commit --allow-empty -m "initial commit"
cd ~/my-city
gc rig add ~/hello-world
```

A rig is a project directory registered with the city. It gets its own beads
database, hook installation, and routing context.

## 4. Send Work Through a Formula

```bash
gc sling hello-world/claude "Create hello.sh that prints hello world and commit it" --on mol-do-work
```

The target is the rig, then the provider: if you chose `codex` or `gemini` in
step 2, sling to `hello-world/codex` or `hello-world/gemini`.

`gc sling` creates a bead for the task and runs the `mol-do-work` formula on
it: one agent reads the task, does it, and closes it. It prints a `Dashboard:`
link to the run:

```text
Dashboard: http://127.0.0.1:8372/city/my-city/runs/hw-4up
```

That line is the result of getting Gas City running. An agent doing this setup
for you should end its final message with it exactly as printed. `gc sling`
leaves the line out when it cannot reach the dashboard, but the run still
started: its `Attached workflow <ID>` line names the run, and the link is
`http://127.0.0.1:8372/city/my-city/runs/<ID>`.

## 5. Watch It Finish

Open the link, or pass the run ID (the last part of the link) to `gc bd show`.
It reports `CLOSED` when the run is done, usually within a minute or two, and
`hello.sh` is committed in `~/hello-world`:

```bash
gc bd show hw-4up
```

This is the smallest formula: one bead, one agent. Gas City's real power is
orchestration -- a **formula** (the method for getting a job done) that the
**orchestrator** runs as a graph: decomposing the work into beads, fanning the
ready ones out to many agents at once, gating each step on its dependencies,
retrying failures, and driving the whole thing to completion outside your
session. See [Formulas](/tutorials/05-formulas) and
[Orders](/tutorials/07-orders), which trigger formulas on a schedule or event.
For a fuller walkthrough of cities and rigs, continue to
[Tutorial 01](/tutorials/01-cities-and-rigs).
