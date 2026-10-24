---
title: "Assignee Doctor Checks"
---

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-10-09 |
| Issue | — |
| Supersedes | — |

An assignee is a routing instruction. `bd update <id> --assignee <name>` accepts
any string, so a typo, a renamed agent, or a target that was never configured
produces a bead that looks owned and is never picked up. This doc covers the two
`gc doctor` checks that make that state visible after the fact.

## Problem

The scheduler cannot catch an unroutable assignee. `buildDesiredState` walks the
configured specs and asks, for each, which beads are assigned to it. A bead whose
assignee matches no spec is never visited, so it contributes no demand and raises
no error. The question is only asked in the direction that cannot expose the gap.

`gc doctor` has the opposite risk: a run that could not read the config or a
store must not render that failure as health.

## Checks

### `config-load`

Registered with the core checks, so it runs even when `city.toml` fails to load.
It reports one of three states:

- error: the full config load failed, so every config-dependent check was not
  registered and the report is incomplete;
- warning: the load now succeeds but failed at registration time, so the
  config-dependent checks did not run in this invocation; rerun `gc doctor`;
- ok: the load succeeded and the config-dependent checks are registered.

### `assignee-resolves`

Registered with the city store checks, so it is skipped with them when the bead
store is unreachable. It lists open work beads (`open`, `in_progress`, `blocked`)
in the city store and in every active rig store, and reports each whose assignee
resolves to nothing. It never rewrites an assignee.

Findings are a warning. A scan that found nothing but could not cover a scope is
also a warning, with the skipped scope named: a store that failed to open or
list, a rig with no resolved path, an unreadable suspension state, a tmux alias
that failed to resolve, or an empty roster. A roster with no agents and no named
sessions cannot distinguish a bad assignee from a config that failed to load, so
the check reports that it could not answer instead of flagging every bead.

Classes served from a relocated store (`graph`, `messaging`, `nudges`, `orders`,
`sessions`) are not scanned. They are named in the details and in the message of
an otherwise clean result, and they do not turn it into a warning: a split-storage
city would otherwise carry a standing warning with nothing to act on.

## Resolution rules

The roster is built from the loaded config and the live session beads. An
assignee resolves when it is:

- reserved (`human`) or empty;
- the name, qualified name or runtime session name of a configured agent or named
  session, or a pool instance of one (`<stem>-<n>`, `<stem>-pool`,
  `<stem>-<bead-id>`);
- a configured `tmux_alias` or namepool name;
- an identity of a live session bead;
- shaped like an id minted by the city: a configured city or rig prefix, or a
  reserved class prefix, followed by a short id suffix;
- shaped like a generated session name: `<stem>-adhoc-<hex>` or `<stem>-auto-<n>`.

A `rig/` qualifier is honored only when it names a configured rig or agent
directory. An agent imported through a binding resolves by its full qualified
name (`pack.worker`). An undeclared qualifier does not fall back to the trailing
segment, so `wrongrig/<agent>` and `other.<agent>` do not resolve, and an agent
declared under one rig does not resolve under another.

The two shape rules are a deliberate trade-off. Session ids and generated session
names are minted at runtime, so a static roster cannot enumerate them. The check
accepts the shape and therefore passes a name such as `gc-ghost` or
`bogus-auto-3`; it never flags a legitimate runtime id. A city that declares no
prefixes accepts any bead-shaped name.
