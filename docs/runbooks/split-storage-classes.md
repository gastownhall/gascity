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
worst possible moment: with the fleet stopped and a window running. It also
reports, per rig, what `gc storage migrate --from-rigs` would do there (see
"Step 4b: import each rig").

The checks that most often stop a real cutover, and what each wants from you:

| Block | What it means | What to do |
| --- | --- | --- |
| `rig scopes` | A rig's graph rows have a shape the rig import cannot move without a cross-store edge (an open `tracks`/`parent-child` edge between a graph row and rig work, open work blocked by an open graph row), or a rig store cannot be read. | Do what the named fix says (close the workflow or convoy, finish the work, or remove the edge), or restore access to the rig. Rig history, closed or suspended, no longer blocks: each rig is a `[note]` with what its import would move. |
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
4. Does not census rig scopes. A rig's graph rows stay readable through its
   relic legs and move with `gc storage migrate --from-rigs` once the binding
   has converged; the success message names every rig still awaiting that.
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

## Step 4b: import each rig

Before S4 a rig's convergence loops, `cook --attach` grafts and their control
trees were written to the rig's own store. On a split city graph beads live in
the city binding at every scope, so those rows move too, rig by rig, with the
fleet stopped:

```
gc storage migrate --from-rigs --fleet-stopped            # every rig
gc storage migrate --from-rigs --rig <rig> --fleet-stopped # one rig
```

Each row keeps its id and is stamped `gc.root_store_ref = rig:<rig>` (wisps and
steps included). Edges inside the moved set are copied; an open `blocks` edge
onto rig work becomes a cross-store gate in the binding; every other boundary
edge is dropped and recorded on the row (`gc.migrated_cross_edges`) and in the
rig's backup. Shapes that would need a cross-store edge are refused by bead,
with the fix. Per rig it writes `infra.rig.<rig>.migrated.beads` (the manifest)
and `infra.rig.<rig>.migrated` beside `infra.migrated`, backs the rows up to
`infra.rig.<rig>.retained-source.jsonl`, raises the rig's `min_hash_length` so a
later short id cannot re-mint an imported one, then clears the rig's copies and
marks `.gc/storage-infra-cleared.rig.<rig>.json` complete. A rig with no graph
row is recorded `infra.rig.<rig>.clean`. Suspended rigs are imported without
being resumed; a rig that cannot be read blocks only itself. Every step resumes:
run the command again after any failure. A pending clear makes boot refuse
until the re-run finishes it.

`gc storage status` exits non-zero while any rig awaits import, holds a pending
clear, or holds rows written after its import.

The import refuses, before writing anything, a rig store another registered
city also uses or that it cannot rule out as one (shared or company stores wait
for the single-writer attestation; a Dolt rig whose endpoint does not resolve
and that pins no database name cannot be compared), rows scoped to another rig
or city, open rows another city's agent claims, rows its manifest says it
carried that the binding no longer holds, and a rig whose store cannot read the
payload of an edge the import copies or backs up. A bd CLI rig with an edge on
a row it moves is such a rig; a proxied-server rig reads payloads natively with
`GC_BEADS_PROXIED_NATIVE=1`. While an
import is in flight (`infra.rig.<rig>.importing` beside the binding) boot and
every gc command refuse; re-run the import to finish it. If a rig store is
gone for good after its import wrote its marker, `gc storage migrate
--from-rigs --rig <rig> --abandon-rig-store --fleet-stopped` proves the binding
holds every row the manifest records and then records the clear complete. It
refuses while the rig store is still reachable, while an import is in flight,
and while the binding holds rows of that rig its manifest does not record. Rig
records name the store they speak for: a new store under a reused rig name is
not taken for the old one.

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

## gc bd on split cities

On a split city, `gc bd` is an allowlist. bd writes whatever it is told into
the work store. A bead that lands there with an infrastructure class (a
`gc:session` label, a `gc.root_bead_id`, a `message` type, and so on) is
stranded, and every later routed `gc` command refuses until it is repaired.

How a command gets through:

- **The bd binary's command tree must match a pinned inventory.** gc does
  not trust the version string: every Enterprise build reports 1.3.0, and an
  OSS build from main reports 1.3.2 with a tree that differs from the 1.3.2
  tag's. Instead gc walks the binary's `--help` for every visible command and
  compares the commands, aliases and flags (shorthand, type, default,
  no-value default, usage) with the visible projection of each pinned
  inventory: bd 1.3.2, and the bd 1.3.0 Enterprise build at commit
  `77612097cec0`. A binary that matches one is judged against that
  inventory; any other is refused, with a `gc_bd.refused` event and a
  `gc doctor` error (check `bd-command-inventory`, which `gc start` also
  warns about). The inventory is bd's own command tree, hidden flags
  included, dumped from source by `internal/bdflags/inventorygen/generate.sh`
  with its provenance (commit, build tags, Go, cobra and pflag versions).
  Hidden commands and flags are not in `--help`, so for those gc relies on
  the pinned inventory of the build whose visible tree matched.
- **The walk is isolated and must be conclusive.** gc runs each
  `bd <path> --help` in a scratch directory under `.gc/cache/tmp` that is
  also bd's `HOME` and `TMPDIR`, with only `PATH` besides, so a caller's
  environment (`CLICOLOR_FORCE`, `BD_NAME`, `BEADS_DIR`, `TMPDIR`) cannot
  change what it reads. The walk stops at the first level of the tree that
  no inventory matches, and never runs a command no inventory knows. On a
  city whose `.gc` is read-only the cache is read but not written. Each run
  is killed with its process group after 20 seconds, and the whole walk
  after two minutes; stale scratch dirs are removed on the next walk. The
  binary must also be a Go build of bd's `cmd/bd` with the cobra and pflag
  versions the matched inventory records, which rejects a mise or asdf shim
  or a wrapper script: point `PATH` at the real binary.
- **Only conclusive verdicts are cached.** gc keys a verdict by the binary's
  resolved path, device, inode, size, modification time and change time, and
  by the set of inventories this gc embeds, in `.gc/cache/bd-inventory.json`
  (locked across processes, so a cold cache costs one walk). Any replacement
  (`cp`, `cp -p`, `install -p`, `rsync -t`, `tar`, a rename into place) and
  any gc upgrade that changes the inventories re-judges the binary. A walk
  that fails, times out or reads a root with no commands is reported as an
  error and remembered for a minute; it is never cached as a refusal.
- **The vouch is a convenience.** `gc storage vouch-bd` (with `--rig <rig>`
  for a rig's bd) walks the binary for real, never trusting a cached verdict
  or an existing vouch, and, only if its tree matches, records the binary's
  path, its bytes (sha256), the inventory and this gc's inventory set in
  `.gc/bd-inventory-vouch.json`. That binary is then admitted without a walk.
  The first admission by a vouch in each gc process (from the vouch or from
  the cached verdict it produced) records a `gc_bd.vouch_used` event. A
  binary that matches nothing is refused and nothing is written. A gc whose
  pinned inventories differ (an upgrade) ignores the vouch and walks the
  binary again. The vouch never goes in `city.toml`, so every gc version
  still loads the city, and you never edit a file to vouch.
- **Neither the vouch nor the cache is a security boundary.** Anyone who can
  write `.gc` can forge either. `gc doctor` therefore walks each distinct bd
  binary for real once per run and reports a binary whose cached or vouched
  verdict the walk contradicts (the walk also corrects the cache). An
  unreadable vouch file is ignored with a doctor warning. `gc start`'s
  warm-up reports from the cache only: a binary gc has not judged yet is
  "pending", not a failure.
- **gc resolves the command exactly as bd does,** with a mirror of that tree
  under the same cobra and pflag versions. It refuses any argv where cobra's
  command lookup and pflag's flag parse disagree about which arguments name
  the command. It also refuses any short-flag run that ends in a flag taking
  the next argument (`-qC x`); spell such a flag on its own.
- **Only the commands and flags below are forwarded.** Anything else is
  refused, and nothing is written. The refusal names the command and the `gc`
  alternative. `--help` on any command is allowed, because bd only prints
  help.

Single-store cities are unaffected: there `gc bd` forwards everything
unchanged.

**Reads:** `show`, `list`, `ready`, `search`, `query`, `count`, `children`,
`blocked`, `stale`, `status`, `statuses`, `types`, `where`, `info`, `context`,
`version`, `help`, `human`, `prime`, `history`, `graph`, `export`, `state`,
`state list`, `comments`, `comments list`, `label list`, `label list-all`,
`dep list`, `dep tree`, `dep cycles`, `mol current`, `mol show`,
`mol progress`, `formula list`, `formula show`, `gate list`, `config get`,
`config list`, `config show`, `backup status`, `dolt status`, `dolt show`.
gc serves its own `release-if-current` itself.

**Writes that change no field the class depends on:** `close`, `reopen`,
`assign`, `priority`, `note`, `comment`, `comments add`,
`heartbeat`, `unclaim`, `reclaim`, `defer`, `undefer`, `dep remove`,
`dep unrelate`, `promote`, `gate resolve`, `gate check`,
`backup sync`, `backup init`, `dolt start`,
`dolt stop`, `dolt push`, `dolt commit`, and `init` (only `--prefix`,
`--non-interactive`, `--skip-hooks`, `--skip-agents`, `--init-if-missing` and
`--recreate-missing`).

**Writes gc judges:**

- `create` and `q`: the new bead's type, labels and metadata are classified,
  including a `--metadata @file`, which gc reads, and the labels bd copies
  from a `--parent` (unless `--no-inherit-labels`). bd copies the labels of
  the parent its own store holds, so a parent that store's read does not find
  exactly is refused. An `--id` inside a class
  binding's reserved namespace is refused. `create --graph` is refused: a
  graph plan mints a bead per node and no shipped workflow uses it.
- `delete` (positionals and `--from-file`): every id must name exactly one
  bead of the work store (bd would delete whatever bead a short id
  prefixes). Refused when a class-binding bead depends on one of the beads,
  or an open cross-store drain gate waits on it (`gc.cross_store_blocker`),
  because that edge or gate would dangle; `--gc-allow-dangling-edges` (gc's
  own flag; bd never sees it) overrides that check, not the id check.
  `--cascade` is refused.
- `init`: a `--prefix` inside a class binding's reserved namespace is
  refused.
- `update`, `label add`, `label remove`, `tag` and `set-state`: the write is
  judged against the bead its id resolves to. If the id resolves to no bead,
  to a different bead (a short or partial id), or cannot be read, a write that
  touches type, labels or metadata is refused.
- `label propagate`: a class-marker label is refused.
- `import`: rows of a class the store does not hold are refused, or left out
  with `--skip-infra`. gc reads the file the way bd's import does: a line
  with a `_schema` key is the export header, a line whose `_type` key is
  exactly `"memory"` is a memory record, and a row whose status is
  `tombstone` is skipped. bd imports every other line as an issue, whatever
  else its `_type` says, so gc classifies every other line.
- `mol burn` (the gastown patrols): every id must name exactly one bead of
  the work store, as for `delete`. The burn is refused when a class-binding
  bead depends on, or an open cross-store drain gate waits on, the molecule
  or any bead bd lists below it (`bd list --parent`, recursively), because
  bd deletes all of them at once. For a persistent (non-wisp) molecule it is
  also refused when one waits on a bead that depends on any of those,
  through any edge (`bd dep list --direction=up`, transitively): on a
  proxied-server store bd's burn deletes those beads too. `--dry-run`
  deletes nothing.
- `mol wisp gc` (the deacon prompt): gc first asks bd which wisps the same
  command would delete (it runs it with `--dry-run --json`). It refuses when
  a class-binding bead depends on, or an open cross-store drain gate waits
  on, any of them. Otherwise gc has bd delete exactly the wisps it checked
  (`bd delete --force`, which prints delete's report) instead of running the
  gc, which would select again: a wisp that becomes a candidate after the
  check is left for the next gc, and a checked wisp that a second dry run no
  longer selects is left out. `--closed --force`, which deletes every closed
  wisp whatever its age, is refused. So is an `--age` under an hour (bd's
  default): a shorter age reaches wisps closed so recently that the gate
  sweep may not yet have released the class-binding gates waiting on them,
  and gc would refuse the whole gc while one of those gates is open.
- `purge` and `prune` (the core reaper's purge and session prune): bd
  deletes the closed beads it selects all at once, and its dry run counts
  them without naming them, so gc cannot check each one against the class
  binding as it does for `delete`. gc holds a run that deletes (`--force`
  without `--dry-run`) to age instead: it must give `--older-than`, which gc
  reads as bd does (`7`, `7d`, `1w`, `168h`), and an age under a week (168h)
  is refused, as is `prune --ignore-references`. The gate sweep, which runs
  every 30 seconds, releases a class-binding gate once its work blocker
  closes, so a bead closed a week ago is clear of the open cross-store drain
  gates unless the sweep has not run since then. The reaper's default ages
  (`GC_REAPER_PURGE_AGE` 168h, `GC_REAPER_SESSION_PURGE_AGE` 720h) meet that
  floor. Set either one under 168h and that reaper step's `gc bd` call is
  refused each time the step runs: the reaper records an anomaly, and gc
  records a `gc_bd.refused` event. Age does not cover a dependency edge that a
  class-binding bead itself holds on a closed work bead (a migration can carry
  such edges into the binding): `purge` and `prune` can delete that work bead
  and leave the edge dangling.
- `batch`: every `create` line is classified.
- `sql`: only a single plain read is allowed: no comments, no Dolt procedures,
  no backtick-quoted Dolt names.
- `config set`: only `types.custom`, `types.infra` and `types.retained`.
- `mol wisp` and `mol wisp create`: only with `--root-only` (the gastown
  patrols' shape). That creates a single bead, and gc judges every bead bd
  could clone for it before bd runs.
- `cook`: only without `--persist`.
- **No edge ever crosses stores.** `dep add` (positionals, `--blocked-by`,
  `--depends-on`), `dep relate`, `link`, `duplicate` (`--of`), `supersede`
  (`--with`), `gate create` (`--blocks`), `create`/`q`/`update --parent`, `create --deps` and
  `--waits-for`, the `dep add` lines of a `batch`, and the dependencies of an
  `import` must each name beads of the city's work stores that no class
  binding holds. Each endpoint is read in the work store its id prefix names,
  so a dependency between a rig's bead and the city's (or another rig's) is
  allowed. An `external:` reference is not a bead and is allowed. Through the
  by-id door, `update --parent` of a binding resident must name a parent the
  same binding holds. `dep add --file` is refused.

**Aliases forwarded:** `new` (create), `done` (close), `view` (show), `hb`
(heartbeat), `stats` (status), `dep rm` (dep remove). Every other alias is
refused, `mol fart` and `protomolecule` included.

**Notable refusals and what to use instead:**

| Refused | Use instead |
|---|---|
| `mol pour`, `mol bond`, `mol wisp` without `--root-only`, `cook --persist`, `swarm` | `gc sling <target> <formula>` or `gc formula cook <formula>`, which put every bead in the store that holds its class |
| `backup restore` | the split-city path in `examples/bd/dolt/docs/journal-corruption-recovery.md` |
| `dolt pull`, `vc`, `federation`, `sync`, `branch`, `conflicts`, `migrate`, `admin`, `init --db` | not available through `gc bd` on a split city: these maintain the store itself, and gc cannot judge what they write. Stop the city (`gc stop`) and have the store's operator carry out the maintenance under that operation's own runbook; then `gc start`, whose boot check refuses to serve if the work store holds an infrastructure row, and `gc doctor` |
| `serve` | the gc API, which routes writes by class |
| `create-form`, `edit` | `gc bd create` / `gc bd update` with flags |
| flags that point bd at another store: `-C`/`--directory`, `--db`, `--database`, `--profile`, `--server-url`, `--global`, `create`/`gate create --repo`, `dolt push --remote`, `cook --search-path`, and init's backend, server and remote flags | `gc bd --rig <rig>` to pick a scope; for anything else, none |

There is no override flag for these. gc cannot verify a backup's age or a
merged revision's contents against the split, so these commands stay refused.
No refusal sends you around gc: going around it would skip every check above.

Every refusal also writes a `gc_bd.refused` event to the city's event log,
with the command path, the verb, the scope, the caller (`GC_AGENT`) and the
session, so a script's `|| true` cannot hide one.

**Requesting an addition.** The allowlist lives in
`cmd/gc/bd_split_allowlist.go`. To add a command, open an issue or a PR that
names the workflow that needs it and says why the command cannot change a
bead's class or write a cross-store edge, or how a judge would decide it. Add
a row and its tests, and add it to this section;
`TestGcBdSplitAllowlistIsDocumented` checks that this section lists every
allowlisted command. Until then the command is not available through `gc bd`
on a split city.

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

## Upgrading a city whose storage check refuses

Before `gc hook` claims work for a session, it reads that session's drain
state from the binding. On a city whose storage check refuses (`gc storage
status` exits non-zero), that read fails and the hook fails closed: it claims
nothing, prints `GC_HOOK_STORE_UNAVAILABLE` on stderr, and exits 2. Builds
before this behavior kept claiming from the work store, so a city can have
been refused for a long time while its sessions kept working. Deploying onto
it stops every session's claims at its next hook.

So stop such a city before the new `gc` is installed, and repair it with the
new `gc` before you start it. The `gc` you are replacing cannot finish the
repair: its `migrate` refuses a converged city over infrastructure beads that
rig scopes still hold, its `recover-stranded` has no per-id choices, and its
clear matches the copy manifest by bare id, so it can delete a bead the copy
never carried. A stopped city runs no `gc hook`, so installing the new `gc`
while it is stopped stops no claims.

```
# before you install it, in each split city's directory, with the new build
# (read-only, so safe on a running city):
/path/to/new/gc storage status   # exits 0: nothing to do for this city
# in each city where it does not exit 0:
gc stop
# install the new gc; then, with it, in each stopped city:
gc storage recover-stranded --from-work --dry-run --dump /tmp/stranded.json
# close what must not be delivered (below)
gc storage recover-stranded --from-work --fleet-stopped
gc storage migrate --from-work --fleet-stopped
gc storage status        # must exit 0
gc start
```

Recovery copies every stranded bead into the binding as it is, so an open
message there is delivered and an open wait or session comes back to life.
Read the dump first, and close in the work store, with `bd close <id>` from
the city directory, any bead that must not: stale handoff mail, for example.
A store that only timed out does not stop claims; the hook goes on and names
the fault.

On a city that already converged, the `migrate` step copies nothing: it
clears the retained copies, and infrastructure beads that rig scopes still
hold from before the split do not stop it.

### A reused id the binding still holds

The work store can mint a short id again once its own bead under that id is
gone. If an infrastructure bead lands under such an id while the binding still
holds the bead the migration carried under it, no copy can carry the new one:
the id is taken. Boot and `gc storage status` name these ids, and
`recover-stranded` lists them under `NOT RECORDED` with both beads side by
side. Two beads are told apart by when they were created: a bead the binding
holds under the id with the same creation time is the same bead, changed since
it was carried, and `recover-stranded` records it instead of asking. A
work-store bead with no creation time is the same bead when the binding's row
under its id was copied from the work store, which the copy marks on the row.
Decide per id which bead keeps it:

| Flag | Keeps the id | Removes |
|---|---|---|
| `--replace-binding-row <id>` | the work store's bead, copied into the binding | the binding's bead |
| `--drop-work-row <id>` | the binding's bead | the work store's bead |

Each choice first writes the bead it removes, with every dependency edge
touching it, to `infra.recover-choices.jsonl` at the binding root, re-reads
the file, and only then removes anything. Those edges go with the bead: kept,
they would point at the other bead under the same id. `--dry-run` shows both
beads of each id without writing, and repeating a choice that already took
effect is a no-op.

### A bead the copy manifest lists by id alone

A manifest written by an earlier `gc` lists some beads by id alone. When the
binding no longer holds such a bead, the manifest may not say whether the
work-store row is the bead the copy carried: it records no time to judge it
by, the row has no creation time, or the row is younger than the manifest. An
earlier `recover-stranded` added the id of every bead it copied, so the
binding may since have delivered, closed and collected that very bead.
`recover-stranded` moves such a row when it is closed. An open one it refuses
and lists with its status, type, times and title, because moving a bead the
binding already delivered would deliver it again. Decide per id:

| Flag | What it does with the work-store row |
|---|---|
| `--move <id>` | copies it into the binding as it is |
| `--record-carried <id>` | records it as the bead the copy carried; `migrate` then clears it, and the clear's backup keeps it |

`--dry-run` and `--dump` list these rows without writing.

### A bead prefix in a reserved namespace

The binding mints ids under the reserved prefixes `gcg`, `gcm`, `gcs`, `gco`,
`gcn` and `gcnq`. A split city whose bead prefix, or a rig's, is one of them,
or starts with one before its first hyphen (`gcg-alpha`), shares the binding's
id namespace, so their ids collide. Ordinary names derive such a prefix: a
city named `gas-city-ops` derives `gco`. Earlier builds only warned. This
build refuses `gc start`, boot, a config reload and `gc storage migrate`, and
`gc storage status` exits 1 and names the prefix:

```
boot plan: REFUSED — a city boot would not serve this configuration: this city
splits its coordination classes into a binding that mints ids under the
reserved prefixes (...), and the city's bead prefix "gco" (derived from
"gas-city-ops" unless set) is in the reserved "gco" namespace; ...
```

The refusal is standing, so from deploy on every `gc hook` in the city fails
closed and claims nothing, and the repair steps above do not clear it. Fix it
before you install the new `gc`, with a prefix whose first segment is outside
the reserved set:

| Prefix | Set it with |
|---|---|
| The city's | `workspace_prefix = "<prefix>"` in `.gc/site.toml` |
| A rig's | `prefix = "<prefix>"` on its `[[rigs]]` entry |

The new prefix applies to beads minted from then on. Work beads already minted
under the reserved prefix keep their ids, which the split city looks up in the
binding: the new prefix does not move them.

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

Use `gc ready` instead. It federates the city store, the binding and the store
of every rig that is not suspended, and exits non-zero naming the leg it could
not read rather than returning a short array. A rig suspended with
`gc rig suspend`, or declared `suspended_on_start` and not resumed, is skipped
just as the controller skips it: what its own store holds comes back on
`gc rig resume`, and routed graph work in the binding stays visible throughout
(see [`gc ready`](/reference/cli#gc-ready)). Every worker's generated work query
is already swapped onto it, so this affects operators and ad-hoc scripts, not
the work loop.

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

The manifest also records which rules decided what counts as infrastructure
when the copy ran, and every later check classifies the work store by those
rules. A newer `gc` that counts more kinds of bead as infrastructure, such as
startup-health episodes, does not report the ones an earlier copy left in the
work store as stranded. They stay where that copy left them, and no check
names them.
