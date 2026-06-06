---
title: Internals
description: How Gas City stitches its pieces together under the hood.
---

The Internals section explains how Gas City's surfaces fit together below the
CLI layer. These pages aren't required reading to use `gc` — the
[Architecture Overview](/concepts/architecture-overview),
[Tutorials](/tutorials/index), and [Guides](/guides/index) come first — but they
answer the "why does it look like that on disk?" questions that show up the
first time you peek inside a city directory.

For the big-picture mental model — cities, rigs, agents, sessions, the beads
store, the event bus, and the controller — read the
[Architecture Overview](/concepts/architecture-overview) first.

Each page focuses on one substrate and traces it from user-visible behavior
down to the files and processes that produce it.

## Pages

- [Beads Storage Topology](/internals/beads-topology) — what backs `gc rig`:
  the shared Dolt server, per-rig `.beads/` directories, and why `bd list`
  from a rig sees only that rig's beads.

For forward-looking design notes and contributor-only material, see
`engdocs/` in the repository.
