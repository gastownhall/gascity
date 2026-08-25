---
title: Move a City's Infrastructure Classes onto Their Own Store
description: Config swap, one operator command, and the boot refusal that stands between them — plus what to check before, during, and after the cutover, and how to roll back while rolling back is still free.
---

Every city stores six kinds of state. One is the **work ledger** — the tasks,
epics, bugs, and convoys people and agents actually reason about. The other
five are **infrastructure**: the execution graph, session lifecycle, messaging,
orders, and the nudge queue. By default all six live in the work ledger.

They do not have to. `[storage]` in `city.toml` assigns each class to a named
binding, and this build serves one split: work stays on the work ledger, and
the five infrastructure classes move to a single SQLite bead engine. On a
production-scale city that measured **130–250× lower latency for
infrastructure-class reads** than serving them from the work ledger, because
those reads stop competing with the work ledger's own traffic.

This runbook is the whole cutover. It is short because the design is: swap the
config, run one command, start the city.

## Before you start

- **Take a backup.** The migration retains the source verbatim and proves
  equality before recording anything, but it is still the moment your city's
  infrastructure state exists in two places. Back up the city directory.
- **Know which side you are on.** `gc storage status` answers it, read-only,
  and never creates the database it reports on.
- **Rehearse the cutover.** `gc storage preflight` runs every check the
  migration runs, against a live city, without migrating. Run it before you
  schedule the window, not inside it — but after Step 1, since it resolves its
  destination from `[storage.classes]` and has nothing to check until that
  section names a binding.
- **Stop the city.** The migration refuses while a controller is live, and it
  asks you to attest that nothing else is writing either. `gc stop` handles
  the controller; the attestation is yours.
- **Cities on an out-of-tree provider do not take this path.** A city born on
  a provider a downstream fork compiled in creates its binding at first boot
  and moves nothing. This command's one source is the work store.

## Step 1: author the split

Add the class map and the binding it names. Either half may live in an include
fragment — `[storage]` composes across layers and is validated for
completeness only after every layer merges. `examples/storage/` ships both
halves as a reference.

```toml
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
```

All six assignments are required once `[storage]` exists. This build serves
the whole split or none of it: either every class names `work`, or work names
`work` and all five infrastructure classes name one shared binding. Anything
else is refused at startup rather than routed halfway.

**The config swap comes first, and it is deliberate.** The migration resolves
its destination from `[storage.classes]`, so it cannot run until the config
says where the destination is. Between the swap and a proven copy your city is
configured to read a binding that does not hold its state — which is exactly
why the next thing that happens is a refusal, not a boot.

## Step 2: watch the boot refuse

Start the city. It will not start:

```
gc start: [storage.classes] assign graph/sessions/messaging/orders/nudges to
binding "infra", but this city has not migrated onto it: that state still
lives in the work store and no convergence marker exists at
<path>/.gc/store/infra.migrated. Boot never migrates. Run:
gc storage migrate --from-work
```

This is the design working. **Boot never migrates.** A booting binary cannot
know whether some other process is still writing to the source, so it refuses
and names the command an operator runs deliberately, with the city stopped.

## Step 3: rehearse it before you take the window

```
gc storage preflight
```

Runs every check the migration runs against a **live** city — and copies
nothing, creates nothing, takes no migration guard, and publishes no event. It
exists because the migration's refusals are all correct and all arrive at the
worst possible moment: with the fleet stopped and a window running. The
rig-scope census is the one that most justifies it, because its remedy is moving
beads by hand and no command here does that for you.

The checks that most often stop a real cutover, and what each wants from you:

| Block | What it means | What to do |
| --- | --- | --- |
| `rig scopes` | An infrastructure bead lives in a rig's store, and the copy reads only the city work store. | Move the named beads into the city work store by hand. |
| `served binding` | This city already served its infrastructure classes from a different binding. | Verify or recover that binding first. Removing the note is your attestation that you have. |
| `edge payloads` | A dependency edge carries a payload this build's copy cannot carry — `waits_for` gates write these. | Nothing to fix yet; the carry is not built. The refusal keeps the source intact. |
| `destination` | The binding already holds beads this migration did not write. | Find out whose they are before letting a copy overwrite them. |

It exits non-zero when the migration would refuse for something you have to go
and fix. **That is a different question from `gc storage status`**, which exits
non-zero whenever the city is not yet serving from its binding — the ordinary
state of every city with a cutover still ahead of it. A city that preflight
clears will still fail a `status` gate until it has actually migrated.

Two things it reports rather than refuses. A **live controller** is named by PID
and does not affect the exit code — stopping it is the next thing you were going
to do anyway, and blocking would mean the command for planning a window could
only be run from inside one. And `--fleet-stopped` is **never checked**, here or
anywhere: it is an operator attestation precisely because no process can verify
it, and preflight says so rather than letting a clean report read as broader
than it is.

## Step 4: run the migration

```
gc stop
gc storage migrate --from-work --fleet-stopped
```

`--from-work` names the source explicitly. It is the only source this build
carries, and passing nothing refuses rather than defaulting, so a source added
later cannot inherit this one's behavior by silence.

`--fleet-stopped` is your attestation. The command proves on its own that no
controller is live on the city; it cannot prove anything about a stray `bd`,
a rig-side script, or a second gc process, so it asks you to state it.

What the command does, in order:

1. Resolves the same plan boot resolves, so a layout boot would refuse is
   refused here first rather than migrated toward.
2. Refuses if a controller answers the city's socket.
3. Takes a migration guard over the city, so a second migrator cannot run.
4. Censuses every rig's bead scope for infrastructure beads and **refuses by
   id and rig** if it finds any. The copy reads the city work store only, so a
   bead sitting in a rig scope would be left unreachable. Move those into the
   city work store first.
5. Copies every infrastructure bead with its id and within-class dependency
   topology preserved.
6. Closes the destination, reopens it, and proves field-level equality against
   the bytes on disk — not against the connection that wrote them.
7. Records a proven-copy manifest, and only then the convergence marker.

The source is never mutated. Nothing is deleted, moved, or pruned from the
work store.

## Step 5: start, and verify

```
gc start
gc storage status
```

`status` reports the class map, the binding, the marker and manifest paths,
how many infrastructure beads the retained source still holds, how many the
binding itself holds right now, and — once converged — the proven-copy size,
the stranded count, and how many the binding's own garbage collection has
removed since cutover. It exits non-zero while the city is unconverged, so a
deployment script can gate on it.

Read the two census lines as a pair. The `source:` count does not change when
a cutover succeeds — the migration copies and retains — so it is the `binding:`
count that tells you whether anything is being served from the new store. On an
unconverged city that line reads zero without the database being created to
learn it.

## Watching a cutover from outside

A boot that reaches a verdict about the binding publishes one
`storage.binding.*` event carrying that verdict and, on a serving outcome, the
size of the proven copy it rests on:

| Event                            | Meaning                                                 |
| -------------------------------- | ------------------------------------------------------- |
| `storage.binding.converged`      | Serving. Either a proven copy populated the binding, or the city was born split and the work store holds no infrastructure bead. |
| `storage.binding.genesis`        | Serving a binding created for a city with nothing to move. |
| `storage.binding.unconverged`    | Refused: config and data disagree. `invariant` says how. |
| `storage.binding.uncheckable`    | Refused: the check that would decide could not run.      |
| `storage.binding.not_configured` | This city has no infrastructure split.                   |

The last one is a verdict, not the absence of one, and it is why a subscriber
can gate on these events at all: silence would otherwise be indistinguishable
from a gate that crashed before deciding.

`outcome` is finer than the event type. Four refusals — `unconverged`,
`stranded`, `born-split-blocked` and `genesis-blocked` — all arrive as
`storage.binding.unconverged`, because a subscriber branches on "is this city
serving" and all four answer no. The `outcome` field says which no it was and
`invariant` says why, so a consumer that switches on `outcome` must handle all
eight values, not the five type names.

`proven_beads` is zero on every refusal, where it means the size was not
established rather than that the copy is empty. On `genesis` the zero is real.
It is also real on a born-split `converged`, where it means something else
again: no copy ever ran, and the discipline being reported is that the work
store holds nothing the binding would need. Those two are told apart by
`database`, which a born-split event leaves empty.

**Some refusals publish nothing, on purpose or otherwise.** A `[storage.classes]`
arrangement this build cannot serve at all, a plan that does not resolve, and a
binding whose provider opens no bead engine are all refused before any verdict
about the binding exists — the last deliberately, so a permanently unservable
binding does not publish `converged` on every boot. Gate on the events for the
cutover states above; read the exit code and stderr for the config states, which
no event describes.

### `gc bd ready` stops answering, on purpose

After the split, `gc bd ready` is refused with exit 1 on this city no matter
what arguments it is given, and the refusal names the relocated class and the
binding. `gc bd list --ready` is refused the same way, because bd documents that
flag as "same semantics as bd ready" and runs the same query. It is not a bug
and it is not scoped to the arguments: `bd ready` computes a frontier over the
one ledger `bd` resolves from the working directory, and takes no selector that
could reach the binding, so its answer is the work-class subset of the city's
ready set and nothing distinguishes that from the whole of it.

Use `gc ready` instead. It federates the city store, the rig stores and the
binding, and exits non-zero naming the leg it could not read rather than
returning a short array. Every worker's generated work query is already swapped
onto it, so this affects operators and ad-hoc scripts, not the work loop.

`gc ready` is flag-compatible with the `bd ready` invocation that generated work
query builds — **not** with all of `bd ready`. It takes `--assignee`,
`--unassigned`, `--metadata-field`, `--exclude-type`, `--exclude-label`,
`--sort`, `--limit`, `--include-ephemeral`, `--status` and `--json`, and rejects
the rest of bd's ready surface (`--label`, `--label-any`, `--parent`, `--type`,
`--priority`, `--offset`, `--has-metadata-key`, `--mol`, `--include-deferred`,
`--gated`, `--claim`, and every single-letter shorthand such as `-u` or `-n`).
Its `--sort` takes `oldest|newest`, not bd's `priority|hybrid|oldest`. If your
query needs a flag only bd has, narrow with `--metadata-field` or read the
relocated class directly from the binding —
`GC_BD_ALLOW_RELOCATED_CLASS_READ=1` runs the one-ledger read anyway when the
work-class subset is genuinely the answer you want.

Make sure the `gc` on every agent's `PATH` is the build that has `gc ready`:
the generated work query shells out to it by name, and an older `gc` on `PATH`
fails every hook with `running work query: exit status 1`.

## Rolling back

**Before cutover, rollback is free** — nothing moved, so nothing is lost. How
you spell it still matters.

> **Danger: deleting the `[storage]` section is not a rollback.**
> A city with no `[storage]` short-circuits at the top of the startup gate: no
> plan is resolved, no binding is named, no marker is read, and no convergence
> check runs at all. On a city that never cut over, that is harmless. On one
> that did, the city starts cleanly and serves every infrastructure read from
> the retained work store, while everything written since the marker stays in
> the binding, unread. The two diverge from that boot onward and nothing says
> so.

Spell the rollback as a class map instead:

```toml
[storage.classes]
work = "work"
graph = "work"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"
```

Drop the `[storage.bindings.infra]` block in the same edit — a binding no class
selects is refused at startup rather than quietly ignored.

Because `[storage]` stays in the file, the section is still resolved and
validated on every boot: a half-finished revert that leaves one class pointing
at the binding is refused as an arrangement this build cannot serve rather than
routed halfway, and `gc storage status` still prints the class map you are
reading. Pointing the map back at `infra` then puts the convergence checks
— marker, manifest, and the stranded-write re-check — back in play in one edit.

**After cutover it is not.** The binding is the live infrastructure store, and
everything written since the marker exists only there. The gc refusal messages
make that decision for you rather than leaving it to judgment: the sentence
telling you to revert appears **only** when the binding has been read and
proven to hold nothing. If gc cannot see the binding — an unmounted volume, a
permission fault, a root it could not list — it says so and withholds the
instruction, because an absence nobody could look at is not evidence of
emptiness.

The check to run by hand is the marker: if
`<binding path>/infra.migrated` exists, the city has cut over, and a config
revert abandons whatever the binding holds.

## What a later boot keeps checking

Convergence is not asserted once. Every boot re-checks that every
infrastructure bead the retained source still holds is readable from the
binding, classified against the recorded proven-copy manifest.

- A bead the copy delivered that the binding no longer holds is the binding's
  own garbage collection doing its job — expired closed workflows and read
  mail — and is counted, not alarmed.
- A bead the copy **never carried** is a write that landed in the source after
  the equality proof. It becomes a blocked boot that names the ids, and the
  beads are intact in the retained source. It never becomes silence.
- A check that could not run is reported as a failed check, not as a city that
  never converged, and the boot refuses rather than serving from a binding it
  could not verify.
