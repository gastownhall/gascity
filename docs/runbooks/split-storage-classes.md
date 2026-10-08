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

- **Take a backup.** The migration proves equality before recording anything,
  and it backs up every work-store row before clearing it, but it is still the
  moment your city's infrastructure state moves between stores. Back up the
  city directory.
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
| `edge payloads` | The work store cannot be asked what its dependency edges carry — `waits_for` gates store a payload there. | Nothing in your data: this city's work store runs on an engine that cannot report edge payloads, so this binary cannot split it. The block is permanent until the work store is on an engine that can (SQLite or native Dolt). |
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
8. Clears the work store's copies of the beads it moved. Each row, with every
   dependency edge touching it and that edge's payload, is first written to
   `<binding root>/infra.retained-source.jsonl`; the file is re-read from disk
   and proven equal to the rows it records; only then is a row removed.

**What the backup holds, and what it does not.** The backup records each bead
as gc models it: id, title, status, type, description, priority, assignee,
sender, parent, ref, labels, metadata, the ephemeral and no-history bits,
timestamps, defer-until, and every dependency edge touching the bead with its
payload. That is exactly the set of fields the copy carried into the binding
and the equality stage proved. It does **not** hold the fields only `bd` models:
notes, comments, close reason and closed-at, design, acceptance criteria, the
event history, and the indefinitely-deferred state. Those were never copied
into the binding either. On a native-Dolt work store each removal is a Dolt
commit, so until Dolt garbage-collects that history, `dolt` (`AS OF` a commit
before the clear) is the full-fidelity backstop for them.

Nothing in the work store changes before the marker: until step 7 the source is
exactly as you left it, and the cutover can still be abandoned by a config
edit. Past the marker the binding is the only authoritative copy, which is why
step 8 exists — a copy left behind in the work store is served to every reader
that reaches the work store first (`bd`, `gc hook`, the demand and claim
surfaces), and a workflow step closed in the binding stays open there and is
dispatched again and again.

Step 8 changes only what it has to. A work bead whose blocking dependency
points at a moved bead keeps that dependency, unless the binding has already
closed the bead it waits on, in which case the dependency is released. Every
other dependency from a work bead into a moved bead is kept by re-adding it
after its target row is removed.

**On a `bd` or native-Dolt work store those kept edges are lost.** Removing a
row there drops every edge touching it, and the store refuses to re-add an edge
to a bead it no longer holds. So a work bead blocked on a moved bead that is
still open stops being held back, and a `tracks` or `related` edge from a work
bead (a convoy tracking a workflow bead, say) stops linking the two. Cross-class
blocking is not expressible on a split city yet (ga-2orlf); the migration does
not refuse over it, because refusing would leave a city that boot also refuses
with no way forward. Instead:

- the migrate output lists every such edge as **AT RISK** before the first row
  is removed;
- every edge the store then refuses is recorded in
  `.gc/storage-infra-cleared.json`, listed under `lost cross-store edges` by
  `gc storage status`, and carried as `lost_cross_edges` on every
  `storage.binding.*` event that reads the note;
- every one stays in the backup (`dependents` on the moved bead's entry).

A file or SQLite work store keeps them.

The command is idempotent. Run it again on a converged city and it re-proves
convergence and finishes any clear that did not complete — including the
cross-store edges of rows an interrupted run already removed, which the note
names until the clear finishes; that is also how a
city migrated by gc 1.5.0, which kept the work store's copies, is repaired (see
below).

## Step 5: start, and verify

```
gc start
gc storage status
```

`status` reports the class map, the binding, the marker and manifest paths,
how many infrastructure beads the work store still holds, the retained-source
backup and how many beads it records, how many the binding itself holds right
now, and — once converged — the proven-copy size, the stranded count, and the
number of retained copies still in the work store. It exits non-zero while the
city is unconverged, while it holds a stranded write, and while the work store
still holds copies of beads the binding owns, so a deployment script can gate
on it.

On a converged city `source:` reads 0 and `binding:` carries the count. On an
unconverged city `binding:` reads zero without the database being created to
learn it.

## Upgrading a city migrated by gc 1.5.0

gc 1.5.0's migration kept the work store's copies of every bead it moved. A
city in that state refuses to start under this build:

```
gc start: this city converged on binding "infra", and its work store still
holds N retained pre-migration cop(ies) of beads the binding now owns (...).
Every reader that reaches the work store first is served those frozen copies,
so finished workflow steps are dispatched again and again (#5987). Stop the
city and clear them — each is backed up and proven before it is removed — with:
gc storage migrate --from-work --fleet-stopped
```

That is the whole repair:

```
gc stop
gc storage migrate --from-work --fleet-stopped
gc start
gc storage status
```

The command re-proves convergence, then runs step 8 above. Copies the binding's
own garbage collection has since removed from the binding are cleared too:
they are the frozen rows the demand surface would otherwise serve forever.

If the city also holds a stranded write, the command names it and clears
nothing; run `gc storage recover-stranded --from-work --fleet-stopped` first,
then the migration again.

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

`outcome` is finer than the event type. Five refusals — `unconverged`,
`stranded`, `born-split-blocked`, `genesis-blocked` and `retained-copies` — all
arrive as `storage.binding.unconverged`, because a subscriber branches on "is
this city serving" and all five answer no. The `outcome` field says which no it
was and `invariant` says why, so a consumer that switches on `outcome` must
handle all nine values, not the five type names.

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
> that did, the city starts cleanly against a work store that no longer holds
> its infrastructure beads, while everything the city has is in the binding,
> unread. Nothing says so.

Spell the rollback as a class map instead (on a city whose work store was
cleared, restore the work store first; see below):

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
revert abandons whatever the binding holds. Once the work store has been
cleared, a revert also starts the city with no infrastructure state at all: the
pre-cutover rows are in `infra.retained-source.jsonl`, not in the work store.

The clear also leaves `.gc/storage-infra-cleared.json` in the city directory.
It holds two edits that would otherwise start a city with no infrastructure
state:

- a binding whose volume is not mounted: with the note present, a missing
  marker refuses the boot instead of creating an empty binding;
- a revert — `[storage.classes]` pointed back at `work`, or `[storage]`
  deleted: boot refuses and names the restore below.

Removing the note is your attestation that the work store holds the state you
want served, or that the binding's contents are deliberately abandoned.

### Restoring the work store from the backup

This puts the pre-cutover infrastructure rows back into a `bd` work store, for
a rollback after the clear. It restores the state **as of the cutover**: beads
written to the binding since then are not in the backup and are lost by the
rollback, and the fields listed under "What the backup holds" are not restored.
That includes progress: a workflow step that was open at the cutover and
finished in the binding afterwards comes back open, and the restored city
dispatches it again. Check the binding (`gc bd show <id>` before the revert)
for any step whose work must not run twice.
Edge payloads (a `waits_for` gate's `{"gate":"any-children"}`) are not carried
by `bd import`; a restored gate reads as the default, `all-children`. Verified
with `bd` 1.3.2-rc.1.

The order matters, because `gc` refuses to serve a reverted city while the
notes stand — that refusal is what sent you here:

1. `gc stop`.
2. Point `[storage.classes]` back at `work` and drop the binding block.
3. Remove `.gc/storage-infra-cleared.json` and `.gc/storage-served-binding.json`.
   This is the attestation, and from here until step 5 the city must not be
   started: it would run with no infrastructure state.
4. Restore, from the city directory, with `B` set to the backup the boot
   refusal named:

```bash
B=<binding root>/infra.retained-source.jsonl

# The rows, with their outbound edges and parent links.
cat > /tmp/gc-restore.jq <<'JQ'
select(.bead)
| .bead as $b
| ([(.deps // [])[] | {issue_id, depends_on_id, type}]
   + (if $b.parent then [{issue_id: $b.id, depends_on_id: $b.parent, type: "parent-child"}] else [] end)
   | unique_by([.depends_on_id, .type])) as $deps
| $b
| del(.parent, .from)
| .dependencies = $deps
| if $b.from then .metadata = (($b.metadata // {}) + {from: $b.from}) else . end
JQ
jq -c -f /tmp/gc-restore.jq "$B" > /tmp/gc-restore.jsonl
gc bd import /tmp/gc-restore.jsonl

# The edges from work beads into the restored beads.
jq -r 'select(.bead) | .dependents[]? | "\(.issue_id) \(.depends_on_id) \(.type)"' "$B" |
  while read -r issue on kind; do gc bd dep add "$issue" "$on" --type "$kind"; done
```

5. `gc start`.

`gc bd import` needs the city's custom bead types, which a gc city's work store
already has (`gc bd config get types.custom`).

## If this city cut over before edge payloads were carried

A city that converged under an earlier build has a binding whose within-infra
dependency edges arrived **without their payloads**. The copy re-added every
edge with its endpoints and type intact and dropped the JSON sidecar. The fix
is in the copy; it does nothing for a city that already ran it.

The payload-carrying edges are the `waits_for` fanout gates between formula
step beads, and the payload records which gate the formula asked for —
`{"gate":"any-children"}`. A gate whose payload is gone reads as the default,
`all-children`. The consequence is one-directional: such a gate waits for
*every* child where the formula asked it to release on the first. It never
releases a gate early, and an `all-children` gate — the default, and the common
case — lands on the value it started with. Both endpoints are infrastructure
beads, so no cross-class edge is involved.

Nothing detects this state. The convergence re-check compares the beads the
proven-copy manifest names, and the manifest records ids, not edges.
`gc storage recover-stranded` writes only the edges the binding is *missing*;
a present-but-payloadless edge counts as one the binding already holds, so the
repair skips exactly the edges that are wrong. `gc storage preflight` returns
at the convergence step on a converged city and never reaches the edge-payload
check. Detection and a non-destructive repair are tracked as `ga-67pm3`.

**What is not lost.** The original payload of every pre-cutover edge is still
readable in the work store until it is cleared, and in
`infra.retained-source.jsonl` after that — the clear records every edge with
its payload. Every edge written to the binding after cutover went through the
normal writer and carries its payload. The damage is bounded to the edge set
the old copy carried.

### Re-converging, and what it costs

The only remedy this build offers is running the copy again, and the marker is
in the way: a marker whose database is present is a converged city, and the
migration re-proves convergence instead of re-copying. Removing the component
directory the database lives in — and leaving the marker where it is — is the
state the code calls stale. The marker's claim about the past still holds and
its claim about the present does not, so the migration reports `<marker>
claims convergence but <database> is gone; re-running the copy` and falls
through to the copy. The manifest is replaced by the same atomic rename that
writes it, so it needs no deletion.

```
gc storage status   # read the `database:` line; its parent directory is what
                    # the next step removes
gc stop
rm -rf <binding root>/graph
gc storage migrate --from-backup --fleet-stopped
gc start
gc storage status
```

`--from-backup` re-copies from `infra.retained-source.jsonl`. On a city whose
work store was never cleared (one migrated by gc 1.5.0 and not yet repaired),
use `--from-work` instead; the backup does not exist yet, and the re-copy ends
by clearing the work store. With the backup present, `--from-work` refuses:
the work store no longer holds the slice, and re-copying from it would rebuild
an empty binding.

> **Danger: this destroys every binding write made since cutover.**
> The re-copy reads the state as of the original cutover. Every infrastructure bead created since — and every edge
> among them — exists only in the directory being removed. Take a filesystem
> copy of the binding root first, and treat this as worth doing only where the
> gate kinds matter more than that history.

Leave the marker alone. The procedure does not need it gone — a marker with no
database is precisely the state that re-runs the copy — and it is the binding's
own record that this city has been in service, which the post-cutover refusals
key on.

## What a later boot keeps checking

Convergence is not asserted once. Every boot re-checks that the work store
holds no infrastructure bead, and classifies any it finds against the recorded
proven-copy manifest.

- A bead the copy **never carried** is a write that landed in the work store
  after the equality proof. It becomes a blocked boot that names the ids, and
  the beads are intact in the work store. It never becomes silence.
- A bead the copy delivered that the work store still holds — a city migrated
  by gc 1.5.0, or a clear that was interrupted — is a second row under an id
  the binding owns. It becomes a blocked boot that names the repair command.
- A check that could not run is reported as a failed check, not as a city that
  never converged, and the boot refuses rather than serving from a binding it
  could not verify.
