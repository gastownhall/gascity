---
title: "Using Gas City as a Multi-Agent Engineering Environment"
description: How to take a multi-human, multi-agent workflow you are already running by hand and give it a better home in Gas City.
---

This guide is for teams who are already doing some version of
multi-agent engineering by hand.

Maybe you already have:

- humans coordinating branches and worktrees in chat
- several AI sessions working in parallel
- docs, migration notes, and issue threads moving at the same time
- one person watching product and release shape
- another person carrying operational/tutorial truth
- another person driving the bits into existence

If that sounds familiar, Gas City does not ask you to invent a new kind
of work. It gives the work you are already doing a better home.

## What you may already be doing by hand

A lot of multi-agent engineering teams are already improvising a system
that looks something like this:

- a shared repo with multiple worktrees
- one or more “coordinator” humans keeping the branch story straight
- specialist agents doing bounded tasks in parallel
- prompts, scripts, notes, and checklists scattered between files and chat
- ad hoc naming for roles like reviewer, migration lead, docs owner, or release sheriff

This can work surprisingly well.

It also creates friction:

- important context lives only in chat
- prompt and role behavior are hard to version cleanly
- branch and environment setup is repetitive
- teams drift between “what we do” and “what the tooling knows”
- operational truth gets split across tutorials, notes, and people’s heads

Gas City helps by making those moving parts first-class pack and city
content.

## What Gas City gives that workflow

At a high level:

- the **pack** holds portable team behavior
- the **city** holds deployment choices for this working environment
- **agents** become explicit directories with prompt and local assets
- **commands**, **doctor checks**, **orders**, **formulas**, and
  **template fragments** stop being random loose files
- `.gc/` becomes the machine-local site-binding and runtime layer

That means the working style becomes:

- more reproducible
- more legible
- easier to share
- easier to evolve under version control

## The mental model

Think in three layers:

### 1. Portable team definition

This is the stuff you want to keep, share, review, and evolve:

- pack identity
- agent defaults
- imported packs
- prompts
- overlays
- helper scripts
- commands and doctor checks
- formulas and orders

This belongs in:

- `pack.toml`
- pack-owned directories like `agents/`, `commands/`, `doctor/`,
  `formulas/`, `orders/`, `template-fragments/`, `overlays/`, and `assets/`

### 2. City deployment choices

This is the stuff that says how this particular engineering environment
is arranged:

- which rigs exist
- which packs compose into the city or specific rigs
- runtime and substrate choices
- deployment-specific policy

This belongs in:

- `city.toml`

### 3. Machine-local site binding and runtime state

This is the stuff that should not be mistaken for portable definition:

- local rig bindings
- runtime/controller state
- caches
- worktrees
- sockets, logs, local generated state

This belongs in:

- `.gc/`
- other runtime directories such as caches and work products

## A useful starting point

You do not need a perfect city on day one.

A good first version is:

1. one root city pack
2. a small set of named agents for the human and agent roles you already have
3. one or two commands that encode common team operations
4. one migration guide or working note you actively use
5. a habit of running the real work through the city instead of beside it

That is enough to start learning.

## A concrete team shape

Imagine a release-wave team with three humans and several agents.

The humans might naturally divide into:

- an operational/tutorial owner
- a product/engineering connector
- an implementation-heavy technical lead

The agents might naturally divide into:

- audit / review
- migration
- release-shape validation
- docs truth / schema alignment
- targeted implementation workers

Gas City gives those roles places to live:

- human-facing commands in `commands/`
- doctor checks in `doctor/`
- reusable prompt language in `template-fragments/`
- agent-specific prompt and overlay state in `agents/<name>/`

The point is not to freeze your team shape forever.

The point is to stop pretending that a real multi-agent workflow is just
“some prompts somewhere” plus a pile of shell history.

## What moves cleanly into Gas City

Good candidates:

- stable role prompts
- shared operating language
- repeated review or migration commands
- checks for known structural mistakes
- common overlays, helper scripts, and formulas
- release-wave coordination patterns you keep repeating

Less urgent candidates:

- every experimental prompt variation
- every temporary branch-specific hack
- major organization-wide policy before the local team model is working

Start with the parts you are already repeating.

## Commands are underrated

One of the easiest wins is to turn repeated human/team operations into
pack commands.

Examples:

- “show me the active release branches”
- “run the focused migration checks”
- “summarize open release issues”
- “prepare the branch for review”

This is valuable even if the implementation is just shell scripts at
first.

The win is not just automation.

The win is that your working method becomes visible and versioned.

## Doctor checks are underrated too

If your team keeps rediscovering the same mistakes, encode them.

Examples:

- stale file naming after a migration
- a required prompt file missing from an agent directory
- contradictory config shape across pack and city layers
- known release-shape mismatches in example packs

A doctor check is often a better long-term home than a Slack message or
buried release note.

## Use the product to improve the product

If your team is building Gas City, using Gas City to do that work is one
of the highest-signal feedback loops you can create.

That does not mean every hour of work must happen through the city.

It means:

- if the workflow is awkward, you will feel it
- if the docs lie, you will discover it
- if the migration path is shaky, the team will notice quickly
- if the working model is good, it will make the release better

The key is to use a branch you are actually trying to trust, not a
purely experimental sandbox, when you want meaningful product signal.

## A practical adoption path

You do not need to start from scratch.

Instead:

1. write down the working style you already have
2. identify the parts you keep doing by hand
3. move the repeated parts into pack-owned content
4. give the team a city that reflects the way you actually work
5. let the friction teach you what to improve next

This is usually a better path than trying to design the perfect
multi-agent city in one shot.

## What this guide is not

This guide is not:

- the Pack/City schema reference
- the migration guide
- the tutorials

Those documents answer different questions:

- the **reference** says what the fields mean
- the **migration guide** says how to move existing content forward
- the **tutorials** teach the product

This guide is about turning an existing multi-agent engineering style
into something more coherent and more teachable.

## See also

- [Migrating to Pack/City v.next](/guides/migrating-to-pack-vnext)
- [Shareable Packs](/guides/shareable-packs)
