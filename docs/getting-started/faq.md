---
title: FAQ
description: Quick answers to the questions newcomers ask most — what Gas City adds over a single coding agent, what it runs on, and where to start.
---

## Why do I need Gas City when I already have a coding agent?

A coding agent gives you one session: a faster pair of hands, steered live,
and gone when it crashes. Gas City turns a fleet of them into a **software
factory**. You write down how a job gets done once — a
[formula](/guides/understanding-formulas) — and an orchestrator runs it across
many agents *outside your session*: it decomposes the job, runs the
independent pieces in parallel, reviews and gap-checks the result, and retries
what fails until the work is done. You describe a feature once and come back
to a finished branch. [How Gas City Works](/getting-started/how-gas-city-works)
is the full mental model.

## Couldn't I get the same thing from a bash loop or CI?

A loop can respawn an agent, but it has no model of the work: every iteration
starts blind, and a crash loses whatever the last iteration knew. The
orchestrator runs a formula as a *graph* — it holds each step until its
dependencies close, fans the ready steps out to many agents at once, retries
failures, and keeps every unit of work in a durable store, so progress
survives any crash on either side. CI is complementary rather than
competitive: CI verifies a change after you make it; a formula is what
produces the change. See
[Understanding Formulas](/guides/understanding-formulas) for what the
orchestrator does that a script cannot.

## How does Gas City relate to Gas Town?

Gas City is the platform Gas Town's machinery was extracted into. The
platform hardcodes zero roles — every role Gas Town wired into code (mayor,
crew, and the rest) is now configuration expressed as a
[pack](/guides/understanding-packs), so the same engine runs Gas Town, Ralph,
or whatever you configure. If you know Gas Town, [Coming from Gas
Town](/getting-started/coming-from-gastown) maps its roles, commands, and
layout onto Gas City one table at a time.

## Which coding agents does it work with?

Sixteen built-in harnesses, including Claude Code, Codex CLI, Gemini CLI,
Cursor Agent, GitHub Copilot, Sourcegraph AMP, OpenCode, Grok, Kimi Code, and
Pi — [Harness Recipes](/guides/harness-recipes) has the copy-paste setup for
each. Agents run under the logins or API keys you already have, and each
agent picks its own harness, so a mixed fleet is just configuration.

## Do I have to write Go, or any code at all?

No. Everything user-facing is configuration: TOML files
(`city.toml`, `pack.toml`) declare your agents, formulas, and orders, and
markdown prompt templates define what each role does. A "reviewer" or
"planner" is a prompt you wrote, not a plugin you compiled. Start with
[Configuring an Agent](/guides/configuring-an-agent).

## Do I need tmux? What else does it depend on?

Yes — agent sessions run in tmux. The full runtime set is tmux, jq, git,
dolt, bd (the beads CLI), and flock; `brew install gascity` installs all of
them for you. For the lightest possible start, `GC_BEADS=file` skips the
dolt + bd pair. [Installation](/getting-started/installation) has the exact
versions and the non-Homebrew paths.

## What happens when an agent crashes mid-job?

Nothing is lost. Every unit of work is a **bead** in a durable store that
outlives any session: if an agent dies, its beads stay open and a fresh agent
picks up the same work; if the orchestrator restarts, it adopts the live
sessions it finds and resumes from the store. Sessions are disposable — the
work they did is not. The [Bead
section of How Gas City Works](/getting-started/how-gas-city-works#bead)
explains why the system converges.

## Can I use it with my existing repos?

Yes. Register any project as a **rig** with `gc rig add <path>` — its
directory can live anywhere on disk, and each rig gets its own bead namespace
and agent scope, so work in one project stays isolated from the others.
[Tutorial 01](/tutorials/01-cities-and-rigs) walks through it.

## Is it open source? What does it cost?

Gas City is MIT-licensed and free —
[github.com/gastownhall/gascity](https://github.com/gastownhall/gascity). The
only spend is the model usage of the agents you run, billed through the
harness credentials you already use.

## Where do I start?

[Installation](/getting-started/installation), then the
[Quickstart](/getting-started/quickstart) — it boots your first city in a few
minutes. When you want the guided path, the [Tutorials](/tutorials/index)
build a complete city up, command by command.
