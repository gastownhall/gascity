---
title: "Share a Rig's Bead Store Between Cities"
description: Mark a rig whose bead store another city also attaches, so each city acts only on its own rig's work.
---

Two cities can attach the same bead store as a rig: each declares its own rig,
and both rigs point at one database. Every city decides which rows are its own
by comparing identity strings, and those strings are unique only inside one
city. When both cities import the same pack, a city-scoped pool such as
`crew.worker` exists in each of them, so each city serves, claims, wakes on and
releases the other city's rows.

A rig's **federation** setting is the opt-in that keeps a city to its own rig's
work in a shared store.

<Warning>
This build validates `federation` and `namespace` but does **not enforce them
yet**. A rig with `federation = "rig"` is not protected: the city still acts
on every row of the rig's store. `gc start`, every `gc reload`,
`gc config show --validate`, `gc doctor` and the config validation API warn
about each guarded rig, and `gc rig list` marks it "not enforced by this
build". Do not attach a bead store that another city also uses until you run
a build that enforces the setting.
</Warning>

## Set it

```toml
[[rigs]]
name = "beta"
path = "/srv/work/beta"
federation = "rig"        # "city" (default) | "rig"
namespace  = "exclusive"  # "shared" (default) | "exclusive"; needs federation = "rig"
```

| Key | Values | Meaning |
|---|---|---|
| `federation` | `city` (default), `rig` | `city` keeps the behavior every rig has without the key. `rig` limits this city to identities inside the rig's namespace when it acts on the rig's store. |
| `namespace` | `shared` (default), `exclusive` | `exclusive` declares that no other city serves this rig's namespace on the same store. It is a declaration for cross-city checks and changes nothing in this city on its own. |

Set `federation = "rig"` on the rig in **every** city that attaches the store.
The setting only constrains the city that sets it. Neither key can be set from
a `[[patches.rigs]]` entry: the load refuses it there.

A city that sets neither key behaves as before. The one visible difference is
that `gc config show --json` lists both fields, empty, on every rig, as it
does for any rig field.

## Which identities a guarded rig admits

The rig's namespace is its name followed by `/`, compared the way the bead
store compares actors: an exact `--` reads as `/`, any other run of `.`, `_` or
`-` reads as `_`, and letter case is kept (see the
[identity separator contract](/reference/specs/identity-separator-contract-v1)).

| Identity | Rig `beta` with `federation = "rig"` |
|---|---|
| `beta/crew.worker`, `beta/crew.worker-2` | inside |
| `beta--crew__worker` (session-name spelling) | inside |
| `crew.worker` (city-scoped) | outside |
| `alpha/crew.worker` (another rig) | outside |
| `Beta/crew.worker` | outside (case differs) |

The city's own pool sessions of the rig's agents are also admitted, by their
session records.

## What the config load checks

These are load errors, so a bad value never starts or reloads a city:

- a `federation` or `namespace` value outside its list;
- `namespace = "exclusive"` without `federation = "rig"`;
- a guarded rig name that is not one segment: letters, digits, `-` and `_`,
  starting and ending with a letter or digit, with no `--`, because `--` reads
  as `/`;
- two rigs whose names are one namespace once case is also ignored, for
  example `my-rig` and `My_Rig` (the store keeps case, so this rule is
  stricter than the store's);
- `federation` or `namespace` in a `[[patches.rigs]]` entry.

Where the rig directories are known, a guarded rig also needs the following.
`gc start`, the supervisor, `gc reload`, `gc rig add` and config edits refuse a
config that breaks them, and a running city keeps its old config;
`gc config show --validate`, `gc doctor` and the config validation API report
it as an error.

- its own directory, shared with no other rig and not the city root;
- its agents attached by rig name, not by the rig's path;
- no agent, pool slot, name-pool name or named session outside the rig whose
  identity reads as one inside the namespace (a city agent named
  `beta--worker` reads as `beta/worker`);
- every qualified `default_sling_target` inside the namespace.

## See it

`gc rig list --json` shows `federation` and `namespace` on a rig when they are
not the default:

```json
{"name":"beta","path":"/srv/work/beta","prefix":"be","hq":false,"suspended":false,"running":true,"federation":"rig","namespace":"exclusive","beads":"initialized"}
```

## What the store guard does

Wherever this city opens a guarded rig's bead store, the store is wrapped in a
guard:

- Discovery reads (lists, ready queries, the dashboard and API listings) show
  only rows whose owners are all inside the namespace, rows a session of this
  city's own pools of the rig claimed, rows the guard created (they carry
  `gc.owner_namespace`) and rows no one can be attributed to. A query for an
  identity outside the namespace returns nothing.
- A row's owners (its assignee and routes) decide before the creation stamp.
  A row the guard created whose assignee or route later names an identity
  outside the namespace is another city's row to this city: discovery reads
  hide it and writes to it are refused.
- Reads by id, by parent and of dependencies show every row.
- Writes to another city's row are refused, as are bare routes and holders
  outside the namespace. Each guarded write reads the row it writes once
  more, live, before writing it.
- Another city's changes to the store wake nothing in this city.
- A guarded rig whose store is the city's or another rig's is not opened, and
  neither is an unguarded rig whose store is a guarded rig's.

`gc doctor` reports a guarded rig's store as the bd front door even when its
scope runs the proxied native lane: the guard does not hand out the proxied
store's internals, so the check cannot see the lane.

Paths that do not go through the rig's store handles are not covered by this
build yet; see the warning at the top of this page.

## What it does not cover

- `bd` run directly by people or agents, and any writer other than Gas City.
- Custom `work_query`, `scale_check` and `sling_query` commands.
- Session names built at runtime from `[workspace] session_template` or an
  agent's `tmux_alias`. The load checks only configured identities.
- Other cities. Each city attaching the store must set the key itself.

<Warning>
Builds without this setting ignore the `federation` key with an unknown-field
warning and act on every row of the shared store. Never move a city that has a
guarded rig to an older build. Detach the rig first.
</Warning>
