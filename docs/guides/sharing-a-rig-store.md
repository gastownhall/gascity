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

A city that sets neither key gets no namespace checks: nothing is filtered or
refused for a rig's namespace. `gc config show --json` lists both fields,
empty, on every rig, as it does for any rig field. A few fixes that came with
the guard reach every city; the CHANGELOG lists them.

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

Paths that write the rig's store without going through its store handles
check the setting themselves; see the next section.

## What the claim, routing and `gc bd` paths refuse

The hook claim cycle, `gc sling`, graph routing, `gc agent-script` and `gc bd`
write the rig's store without going through a guarded store, so they check
the setting themselves. Every refusal happens before the write's `bd`
process starts (`gc bd` and `gc agent-script` first read the row they would
write, with `bd show`), and `--force` never overrides one.

- **`gc hook --claim`** judges each candidate by the store it was read from,
  not only by its id prefix, and skips rows the rig does not let this city
  act on:

  ```text
  gc hook --claim: skipping be-12: rig "beta" serves only beta/ (federation = "rig"); its owner "crew.worker" is outside it
  ```

  It claims, adopts, re-stamps or pins only as an identity the rig admits.
  An agent claiming under a short session id gets a hint to give it a rig
  alias or run it as a pool of the rig. For a city-scoped agent,
  `gc hook --claim` does not search guarded rigs at all.
- **`gc sling` and formula graphs** refuse a route into the rig that is not
  one of its own agents, including custom `sling_query` targets, batch
  children and every graph step. A graph needs the rig's own control
  dispatcher; `gc config show --validate` and `gc doctor` warn when the rig
  has none.

  ```text
  gc sling: refusing route into rig "beta": target "mayor" is outside its namespace "beta/" (federation = "rig"); route to one of the rig's own agents. --force does not override this.
  ```
- **`gc agent-script`** checks `bd_claim` and `bd_update` the way `gc bd`
  does, and never falls back to another store.
- **`gc bd`** in the rig's scope runs only what it can check first. Reads
  pass. Writes that name beads (`update`, `close`, `reopen`, `delete`,
  `heartbeat`, `assign`, `unclaim`, `defer`, `dep add`, `link`, `label add`,
  `comment` and similar) are refused when another city owns a row they name.
  Claims need an identity the rig admits; claims of an unnamed row
  (`ready --claim`, `close --claim-next`, `close --continue` without
  `--no-auto`) are refused. A create is stamped as this city's. Anything
  else is refused:

  ```text
  gc bd: refusing sql: rig "beta" (federation = "rig") lets gc bd run only the writes it can check first; use raw bd
  ```

  `relate` and `unrelate` pass: they record a link that blocks nothing.

  A write that names the rig's bead from any other scope (`--rig` another
  rig, `--city`, `-C` another directory, or ids from two stores) is refused
  too, before `bd` runs, because only the rig's own scope can check it:

  ```text
  gc bd: refusing update on be-12: it is rig "beta"'s bead (federation = "rig"), and this command runs in a scope that is no guarded rig's store, where that rig cannot check it; run it in rig "beta"'s scope (gc bd --rig beta ...)
  ```

The core pack's maintenance orders (the orphan sweep and the scripts that ask
`gc agent is-foreign`) are not covered by this build yet; see the warning at
the top of this page.

### Routes on a new bead

The two layers apply one route rule at different strictness, by design:

- **The store guard** refuses a route that is not rig-qualified (a bare
  `crew.worker`), on a create and on any later write. On a create it accepts
  a rig-qualified route outside the namespace (`alpha/crew.worker`): the new
  row is stamped as this city's, so routing it elsewhere hands this city's
  own work off. On an existing row such a route is accepted only when the
  row is this city's own.
- **`gc sling`, graph routing and `gc bd create`** are stricter: a new bead
  may be routed only to one of the rig's own agents (`beta/...`), and a route
  outside the namespace is refused before the write's `bd` process starts.
  With `gc bd`, hand work off by updating the route of a row this city owns
  (`gc bd update <id> --set-metadata gc.routed_to=alpha/crew.worker`), which
  is checked against that row.

A hand-off is one way. Owners decide before the creation stamp, so once a
row's route names an identity outside the namespace the row is another
city's to this city: it drops out of this city's discovery reads, and this
city's writes to it, a route back included, are refused.

## What it does not cover

- `bd` run directly by people or agents, and any writer other than Gas City.
- `gc bd` from an unguarded scope that names another store itself:
  `gc bd sql` against another database, or `gc bd --db <shared store>` with
  an id whose prefix none of this city's rigs uses. Naming the store makes
  it the same as raw `bd`. From a guarded rig's scope, `gc bd` refuses both.
- Custom `work_query`, `scale_check` and `sling_query` commands.
- Session names built at runtime from `[workspace] session_template` or an
  agent's `tmux_alias`. The load checks only configured identities.
- Other cities. Each city attaching the store must set the key itself.

<Warning>
Builds without this setting ignore the `federation` key with an unknown-field
warning and act on every row of the shared store. Never move a city that has a
guarded rig to an older build. Detach the rig first.
</Warning>
