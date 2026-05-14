---
title: Gitignore Guidance
description: What to commit and what to ignore in Gas City city and rig repositories.
---

Gas City mixes portable configuration with machine-local runtime state. Keep
those layers separate in Git: commit the definitions that someone else can reuse,
and ignore generated state that belongs to one machine, one run, or one local
bead store.

## City Repositories

A city repository usually owns the Gas City configuration and pack content for
one environment.

Commit portable and intentional files such as:

- `pack.toml`
- `city.toml` when it represents shared deployment policy
- `agents/`, `commands/`, `doctor/`, `formulas/`, `orders/`, and
  `template-fragments/`
- `packs/` when the city vendors or authors reusable packs
- scripts, docs, hooks, and assets that are part of the city definition

Ignore generated or machine-local files such as:

- `.gc/`, including `.gc/site.toml`, session state, services state, and managed
  agent work directories
- `.beads/`, including the local bead database and generated `routes.jsonl`
- `worktrees/` when your city config uses a top-level worktree directory
- local logs, sockets, editor files, and OS metadata

For a private running city, start with:

```gitignore
# Gas City machine-local state
.gc/
.beads/

# Agent or workflow worktrees, when configured outside .gc/
worktrees/

# Local runtime noise
*.log
*.sock
.DS_Store
```

If this city keeps rigs under `rigs/<name>` and each rig is tracked as its own
repository, keep the parent city repository from accidentally absorbing those
project files:

```gitignore
# Optional: rigs are tracked in their own repositories.
rigs/*/
!rigs/.gitkeep
```

Do not use that optional `rigs/*/` rule when the city repository intentionally
owns files under `rigs/`.

## Rig Repositories

A rig repository is the project agents work on. It should normally commit the
project source code and any project-owned Gas City configuration you expect other
developers or agents to reuse.

Commit intentional project files such as:

- source code, tests, and project docs
- project-local scripts used by `work_query`, `sling_query`, `session_setup`, or
  `pre_start`
- rig-specific pack references or overrides when they are meant to travel with
  the project

Ignore generated or machine-local files such as:

- `.beads/`, including the rig bead database and generated `routes.jsonl`
- `.gc/` if a workflow writes rig-local Gas City state there
- `worktrees/` when the rig config or local scripts create disposable worktrees
- provider transcripts, logs, sockets, and editor files that should stay local

For a rig repository, start with:

```gitignore
# Gas City rig-local state
.beads/
.gc/

# Disposable agent worktrees, when configured at the rig root
worktrees/

# Local runtime noise
*.log
*.sock
.DS_Store
```

## Published Packs

A published pack should contain reusable definitions, not the state from a
running city. Commit pack files, templates, formulas, commands, examples, and
docs. Exclude `.gc/`, `.beads/`, generated `routes.jsonl`, local worktrees,
provider transcripts, and anything that encodes one developer's path or machine
identity.

As a quick review before committing, ask: would this file still be correct on a
different machine with a different city name, rig path, bead prefix, and active
sessions? If not, keep it out of the shared pack or city repo.
