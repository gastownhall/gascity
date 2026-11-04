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

With the setting, this city's discovery reads, claims, routes, wake-ups and
releases in the rig's store keep to its own rows; reads by id, by parent and
of dependencies, and `gc bd` reads, still show every row. The store guard
covers every store handle the city opens, the paths that run `bd` directly
check the setting themselves, and the core pack's maintenance orders respect
it. The sections below describe each, and the last one lists what stays
outside.

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

  It claims, adopts, re-stamps, pins or retires a finished workflow only as
  an identity the rig admits, and a retirement leaves another city's rows
  open. An agent claiming under a short session id gets a hint to give it a
  rig alias or run it as a pool of the rig. For a city-scoped agent,
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
  `--no-auto`) are refused. A create is stamped as this city's, and a claim
  stamp (`gc.session_id`, `gc.session_name`, `gc.stranded_session`) is
  checked as the store guard checks it. Anything else is refused:

  ```text
  gc bd: refusing sql: rig "beta" (federation = "rig") lets gc bd run only the writes it can check first; use raw bd
  ```

  `relate` and `unrelate` pass: they record a link that blocks nothing.

  `gc bd` reads the command the way `bd` does, and `bd` takes the token after
  a flag it does not know yet as that flag's value. So in the rig's scope only
  `bd`'s global flags (`--actor`, `--json` and the like) may come before the
  command, or before a sub-command such as `comments add`; an argv with any
  other flag there is refused. So is a write that points `bd` at another
  store: `--db`, `--database`, `--server-url`, `--global`, `--profile`, or a
  `-C` that opens another store.

  A write that names the rig's bead from any other scope (`--rig` another
  rig, `--city`, `-C` another directory, or ids from two stores), or whose
  `-C` opens the rig's store from another scope, is refused too, before `bd`
  runs, because only the rig's own scope can check it:

  ```text
  gc bd: refusing update on be-12: it is rig "beta"'s bead (federation = "rig"), and this command runs in a scope that is no guarded rig's store, where that rig cannot check it; run it in rig "beta"'s scope (gc bd --rig beta ...)
  ```

  A claim of an unnamed row (`ready --claim`) is such a write, and `-C`
  counts in every spelling `bd` reads, attached (`-C<dir>`) or in a run of
  short flags (`-qC <dir>`).

The dolt pack's backup order (`mol-dog-backup`) skips a guarded rig and
records the skip in its run: the rig's store is shared with other cities, so
whoever operates it backs it up.

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

## Maintenance orders on a guarded rig

The core pack's orders treat a guarded rig's store as shared:

- The reaper, the JSONL export and `gate-sweep` skip guarded rigs, and so
  does the one-shot `is_blocked` repair `gc start`, `gc rig add` and a
  resumed rig run after a bd upgrade. This city neither reaps, archives nor
  repairs the shared store, and it does not close the timer and GitHub gates
  there, so their dependents stay blocked until someone does. Whoever
  operates the store must maintain and back it up, close those gates
  (`bd gate check`), and run `bd recompute-blocked` there after a bd upgrade.
- `cross-rig-deps` leaves dependents in guarded rigs alone, whether written
  as bd writes them (`external:<project>:<capability>`) or in gc's own
  `external:<rig>/<id>` spelling, and rewrites nothing in a run where it
  cannot read `gc rig list`.
- The orphan sweep asks `gc agent is-foreign --rig-namespace <rig>` about
  every rig claim whose holder it does not recognize (neither a configured
  agent nor a live session). A claim held outside the rig's namespace
  (another city's agent or a person) is never released; the sweep counts
  such claims per rig (`kept N claims outside guarded rig namespaces
  (beta: N)`) without naming them. It releases a rig claim in that rig's
  store. A claim that only `gc ready` lists (a wisp, or a rig whose own read
  was skipped) is the guarded rig's whose bead prefix its id carries. The
  verb decides the rig's namespace by store identity, as the store
  guard does. The controller's own pool sweep keeps and counts the same
  claims, and admits this city's pool sessions with the same session records
  as the store guard.
- The nudge and gate-notification orders check a guarded rig's assignee,
  route or addressee first. Both gate notifications leave a gate with no
  addressee alone when its creation stamp (`gc.owner_namespace`) names
  another namespace. The controller tells every exec order which rigs are
  guarded (`GC_GUARDED_RIGS`, the names from the loaded config), so in a city
  with no guarded rig these checks are skipped and `nudge-on-route` makes no
  `gc rig list` call. In a city with one, a run where `gc rig list` cannot be
  read cannot tell a guarded rig apart, so it nudges and notifies nothing new
  and fails, as `cross-rig-deps` does: the controller records `order.failed`
  with the reason, which the dashboard flags (and `gc doctor` after three in
  a row). A later run tries again: the event orders while the event is in
  their lookback window, the stale-gate re-nudge on its next sweep. An order
  that a `gc` without the variable runs reads the rig list and fails closed
  the same way.

<Warning>
Run the core pack from the same build as `gc`. Only the core pack's own
scripts ask with `--rig-namespace`: an older copy of the core pack (pinned,
vendored or left from an earlier install) running under a newer `gc` asks
without it, gets the answer for the city's roster, and does not apply the
namespace. That combination does not fail closed. (A newer core pack under an
older `gc` does: the older `gc` rejects the flag, and the claim is kept.)
</Warning>

## What it does not cover

- `bd` run directly by people or agents, and any writer other than Gas City.
- `gc bd` from an unguarded scope that names another store itself:
  `gc bd sql` against another database, or `gc bd --db <shared store>` (or
  `--database`, `--server-url`, `--global`, `--profile`) with an id whose
  prefix none of this city's rigs uses. Naming the store makes it the same as
  raw `bd`. From a guarded rig's scope, `gc bd` refuses all of them.
- Custom `work_query`, `scale_check` and `sling_query` commands.
- Session names built at runtime from `[workspace] session_template` or an
  agent's `tmux_alias`. The load checks only configured identities.
- Other cities. Each city attaching the store must set the key itself.

<Warning>
Builds without this setting ignore the `federation` key with an unknown-field
warning and act on every row of the shared store. Never move a city that has a
guarded rig to an older build. Detach the rig first.
</Warning>
