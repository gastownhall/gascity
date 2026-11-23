---
title: Upgrading an Existing City
description: Back up a city, upgrade Gas City and Beads, and run the one-time Beads schema migration when your city needs it.
---

A new `gc` binary is the easy part of an upgrade. The work ledger is the part
that matters: every bead your city and its rigs have recorded lives in Dolt
databases that the new `bd` may need to migrate. This page walks through an
upgrade of a running city, using a city at `~/my-city` with one rig,
`my-project`, at `~/my-project`.

The Homebrew `gascity` formula depends on `beads`, so `brew upgrade gascity`
upgrades both to the latest release.

## Check what you are upgrading from

```bash
gc version
bd version
```

What you need to do to the Beads databases depends on the `bd` your city has
been running:

| `bd` it ran | Beads schema step |
|---|---|
| 1.3.0 or newer | None. The current `bd` keeps the 1.3.0 schema. |
| 1.2.2 or older | Run `migrate schema` once for the city and once for each rig (see [Migrate the Beads schema](#migrate-the-beads-schema)). |

If you are not sure, run the migration step anyway. On a database that is
already current it changes nothing and reports `Schema already at v66`.

## Stop the city and back it up

Note each rig's path, then stop the city:

```bash
gc rig list                 # rigs and their paths; their .beads go in the backup too
gc stop ~/my-city
pgrep -fl "my-city/.gc/runtime/packs/dolt"   # prints nothing once Dolt is down
```

`gc stop` stops the agents and then the city's Dolt server. A copy of the Dolt
data is only consistent once that server has exited, so wait for the `pgrep`
check to come back empty. `gc stop` also unregisters the city from the
supervisor, so name the city by its directory, not its name, until
`gc start ~/my-city` registers it again.

Back up the city directory and each rig's `.beads` directory:

```bash
tar -czf ~/my-city-backup.tgz -C ~ my-city
tar -czf ~/my-project-beads-backup.tgz -C ~/my-project .beads   # repeat for each rig
```

| What | Why it matters |
|---|---|
| `~/my-city/.beads/dolt` | The Dolt data for the city **and every rig**. A city created by an older release runs one Dolt server whose data directory holds one database per scope. |
| `~/my-city/.beads` | The city's Beads identity and connection settings. |
| `~/my-city/.gc/site.toml` | Where each rig lives on this machine. |
| `~/my-city/city.toml`, `pack.toml`, packs and lock files | Your city's configuration. |
| `~/my-project/.beads` | Each rig's Beads identity and connection settings. |

A rig that uses its own Dolt server or an external one keeps its data
elsewhere; back that up too. A large `.beads/backup/` directory holds bd's own
automatic backups and can be left out with `tar --exclude`.

To roll back, stop the city, put these directories back, and reinstall the
previous `gc` and `bd`.

## Upgrade Gas City and Beads

```bash
brew update
brew upgrade gascity        # also upgrades beads
gc version                  # the latest release
bd version
```

With a direct download, install the latest `bd` from the
[Beads releases](https://github.com/gastownhall/beads/releases)
as well as the new `gc`. Upgrade every `bd` that reaches these databases,
including copies elsewhere on your `PATH` (`which -a bd`). An older `bd` cannot
open a migrated database.

## Migrate the Beads schema

Skip this section if your city was already running `bd` 1.3.0 or newer.

Beads 1.3 moves the database schema from v53 to v66. Migrating is one-way: a
`bd` built for the older schema refuses a migrated database. A city's
databases live on the Dolt server Gas City manages, where any agent, order or
`bd` on the machine may still run an older `bd`, so `bd` migrates one only when
you ask by name. Until you do, `bd` refuses every write, and most reads fail
too: `bd list`, `bd show` and `bd ready` stop with `table not found: leases`.

Gas City does not run this migration for you; see
[Databases Gas City leaves to you](#databases-gas-city-leaves-to-you).

The city and each rig have their own database, so migrate each one. Start the
city's Dolt server without its agents, migrate, then start the city. On a city
that splits its storage, `gc bd` refuses `migrate`: run the command the
`bd schema:` line gives for each database instead.

```bash
cd ~/my-city
gc dolt restart                                # Dolt only; no agents yet
gc dolt status                                 # Dolt server: running (managed, …)
gc bd --city ~/my-city migrate schema          # the city's own database
gc bd --rig my-project migrate schema          # repeat for each rig
```

Use `gc dolt restart` here: `gc stop` removes the Dolt runtime state that
`gc dolt start` needs, so `gc dolt start` fails on a stopped city.

Each run prints a warning that it is applying the pending schema migrations
to a shared server database and that older `bd` clients will refuse the
database, then `✓ Schema already at v66`. The last line says "already" even on
the run that migrates, because `bd` applies the migrations as it opens the
database. Once every `bd` is upgraded, the warning needs no action.

<Note>
If `bd` refuses the migration because the database has a Dolt remote, the
remote's other clones must not migrate on their own. Run
`gc bd --rig my-project migrate schema --force` from one machine only, then
push the result with `gc bd --rig my-project dolt push` so the other clones
can pull it.

If you start the city before migrating, `gc start` fails with
`city failed to start: … table not found: leases`, but it leaves Dolt running
and the city registered. Run the two `migrate schema` commands; the supervisor
retries and the city comes up on its own, with no second `gc start`.
</Note>

## Databases Gas City leaves to you

Gas City runs `bd migrate schema` only on a database it is creating. Every
other database keeps its schema until you migrate it, whichever `bd` the city
pins: migrating changes the schema for every client of the database at once,
and a `bd` built for the older schema refuses it afterwards, so it is your
call when every client is ready. The one exception is a city that sets
`beads.allow_schema_behind_migrate = true`, which lets Gas City's own store
migrate a database that is behind when it opens it.

You do not have to track which databases are left. `gc start`, `gc reload`
(without `--async`) and the `bd-schema` check of `gc doctor` name each database
whose schema differs from the one built into `gc`, with the command that
migrates it:

```text
gc start: warning: bd schema: rig "my-project" database "mp" on 127.0.0.1:3307 is at schema v53/11 (main/ignored migration lanes), behind v66/26, the schema of the beads library linked into gc (the pinned bd reports "bd version 1.3.4"). … an operator migrates it with a bd at that schema: gc bd --city /home/me/my-city --rig my-project migrate schema
```

On a city that splits its storage the line gives the pinned `bd` itself, run
in the rig's directory with `BEADS_DIR` set to it. Run that from a shell with
no `BEADS_DOLT_*` variables set, or `bd` may reach another server.

### Migrate both lanes

Beads records a database's schema in two migration lanes, shown as
`main/ignored` (`v53/11` above). A migration is finished only when both lanes
reach the schema the line names. A `bd` whose migration moves only the main
lane can leave the example database at `v66/11`, which `gc` still reports as
behind and which a `bd` may refuse to open. Migrate with a `bd` you know moves
both lanes, then confirm it. The check reads both lanes and reports the
database as current only when both match:

```bash
gc doctor --check bd-schema
```

### Name the migrator you trust

The command a `bd schema:` line gives runs the city's pinned `bd`. If that is
not the binary you migrate with, set `migrate_command` under `[beads]`. Every
line and the `bd-schema` check then give your command, filled in for each
database. Gas City never runs it.

Pick a migrator whose schema is the one the city's pinned `bd` runs at, never
a newer one. A database migrated past the pinned `bd`'s schema is one that
`bd` refuses, so change the migrator only together with the pinned `bd`.

```toml
[beads]
# One shell line, rendered per database as a Go template.
migrate_command = "cd {{shq .ScopeDir}} && BEADS_DIR={{shq .BeadsDir}} BEADS_DOLT_SERVER_HOST={{shq .Host}} BEADS_DOLT_SERVER_PORT={{shq .Port}} BEADS_DOLT_SERVER_DATABASE={{shq .Database}} /opt/beads/migrator/bd migrate schema"
```

| Field | Value |
|---|---|
| `{{.City}}` | the city directory |
| `{{.Rig}}` | the rig's name; empty for the city's own database |
| `{{.ScopeDir}}` | the directory of the city or rig the database belongs to |
| `{{.BeadsDir}}` | that directory's `.beads` |
| `{{.Database}}` | the database's name on its Dolt server |
| `{{.Host}}`, `{{.Port}}` | the Dolt server's TCP address; empty on a Unix socket |
| `{{.Socket}}` | the Dolt server's Unix socket; empty over TCP |
| `{{.Bd}}` | the `bd` the city pins for that scope, or `bd` if `gc` cannot resolve it |

Values are raw; `shq` quotes one for a POSIX shell. On a server reached over
TCP, setting the address and database in the command keeps `bd` on the
database the line names; for a Unix socket, set `BEADS_DOLT_SERVER_SOCKET` from
`{{.Socket}}` instead. Run it from a shell with no other `BEADS_DOLT_*`
variables set, as the line says; `bd` then reads a server password from its
credentials file. `gc`
refuses to load a config whose template does not parse or does not render a
single line for a sample city and rig database, and names
`beads.migrate_command`. If the template fails for a real database, the line
gives the default command and the reason. Remove the setting to go back to the
default command.

### When gc and bd disagree

`gc` compares each database with the Beads library it is built with, and the
`bd` you run can differ from that library. A `bd` newer than the library can
still refuse a database `gc` reports as current, and its own message names the
schema it needs. A `bd` older than the schema a line names has nothing to
apply: upgrade `bd` before you migrate.

A rig whose database the city's `bd` cannot open because of such a schema
difference does not stop the city: `gc start` and `gc reload` leave that rig
unserved, the city's log says so, and the other rigs are served until you
migrate it. The city's own database has no such fallback, so the city does
not start until it is migrated.

## Start the city and check it

```bash
gc start ~/my-city
gc doctor
```

`gc start` registers the city with the supervisor again. If the supervisor is
still running the old binary, `gc start` restarts it on the new one. `gc doctor`
reports anything left to repair; `gc doctor --fix` applies the fixes it can
make safely, such as renaming an `[imports.gascity]` pack import to
`[imports.gc]`. A `✗ beads-store` or `✗ rig:<name>:beads` failure that
mentions `leases` means that scope still needs `migrate schema`.

Then repair blocked flags. A Beads 1.3 schema migration can mark beads as
blocked when they are not, which hides them from `bd ready` and stops Gas City
dispatching them ([beads#7037](https://github.com/gastownhall/beads/issues/7037)).
Any database that has migrated to Beads 1.3, whether during this upgrade or
when it first ran `bd` 1.3.0, can carry the bad flags. The repair recomputes
them from the dependency graph and is safe to run on any database:

```bash
gc bd --city ~/my-city recompute-blocked
gc bd --rig my-project recompute-blocked       # repeat for each rig
```

Each run reports how many rows it corrected, or
`is_blocked already consistent — nothing to recompute.`

## Your city's storage stays as it was

New cities are created split: `gc init` writes a `[storage]` section that keeps
work in the work ledger and puts sessions, mail, workflow steps, order runs and
queued nudges in a local store under `.gc/store`. An upgrade never adds that
section to an existing city, so a city created before it keeps every bead in
its work ledger and runs as it did.

To move an existing city's infrastructure beads onto their own store, follow
[Move a City's Infrastructure Classes onto Their Own Store](/runbooks/split-storage-classes):
author the section, rehearse with `gc storage preflight`, stop the city, run
`gc storage migrate --from-work --fleet-stopped`, then start it again.

## Optional: hand the city's Dolt process to Beads

New cities run a Dolt store that `bd` owns. An
upgraded city keeps its Gas City–managed Dolt server and keeps working on it.
Moving it to the `bd`-owned topology is optional, is a separate step, and
**cannot be reversed**, so take a fresh backup first and finish the schema
migration above before you start. `gc beads city migrate-proxied` performs
the move; the
[migration runbook](https://github.com/gastownhall/gascity/blob/main/engdocs/runbooks/beads-migrate-proxied.md)
covers the procedure, what it refuses, and how to verify it.
