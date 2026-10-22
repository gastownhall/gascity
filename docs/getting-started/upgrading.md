---
title: Upgrading an Existing City
description: Back up a city, upgrade Gas City and Beads, and run the one-time Beads schema migration when your city needs it.
---

A new `gc` binary is the easy part of an upgrade. The work ledger is the part
that matters: every bead your city and its rigs have recorded lives in Dolt
databases that the new `bd` may need to migrate. This page walks through an
upgrade of a running city, using a city at `~/my-city` with one rig,
`my-project`, at `~/my-project`.

Gas City 1.5 requires Beads (`bd`) 1.3.1. The Homebrew `gascity` formula
depends on `beads`, so `brew upgrade gascity` upgrades both.

## Check what you are upgrading from

```bash
gc version
bd version
```

What you need to do to the Beads databases depends on the `bd` your city has
been running:

| Coming from | `bd` it ran | Beads schema step |
|---|---|---|
| Gas City 1.4.2 | 1.3.0 | None. Beads 1.3.1 keeps the 1.3.0 schema. |
| Gas City 1.4.1 or earlier | 1.2.2 or older | Run `migrate schema` once for the city and once for each rig (see [Migrate the Beads schema](#migrate-the-beads-schema)). |

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
| `~/my-city/.beads/dolt` | The Dolt data for the city **and every rig**. A city created before Gas City 1.5 runs one Dolt server whose data directory holds one database per scope. |
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
gc version                  # 1.5.0 or newer
bd version                  # 1.3.1
```

With a direct download, install `bd` 1.3.1 from the
[Beads release assets](https://github.com/gastownhall/beads/releases/tag/v1.3.1)
as well as the new `gc`. Upgrade every `bd` that reaches these databases,
including copies elsewhere on your `PATH` (`which -a bd`). An older `bd` cannot
open a migrated database.

## Migrate the Beads schema

Skip this section if your city was already running `bd` 1.3.0.

Beads 1.3 moves the database schema from v53 to v66. `bd` migrates a store it
runs itself automatically, but a city's databases live on the Dolt server Gas
City manages. `bd` treats that server as shared, because other clients may
still be running an older `bd`, so it waits for you to ask by name. Until you
do, `bd` refuses every write, and most reads fail too: `bd list`, `bd show`
and `bd ready` stop with `table not found: leases`. Gas City 1.5 does not run
this migration for you.

The city and each rig have their own database, so migrate each one. Start the
city's Dolt server without its agents, migrate, then start the city:

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

## Optional: hand the city's Dolt process to Beads

New cities created by Gas City 1.5 run a Dolt store that `bd` owns. An
upgraded city keeps its Gas City–managed Dolt server and keeps working on it.
Moving it to the `bd`-owned topology is optional, is a separate step, and
**cannot be reversed**, so take a fresh backup first and finish the schema
migration above before you start. `gc beads city migrate-proxied` performs
the move; the
[Gas City 1.5.0 release notes](https://github.com/gastownhall/gascity/releases/tag/v1.5.0)
and the
[migration runbook](https://github.com/gastownhall/gascity/blob/main/engdocs/runbooks/beads-migrate-proxied.md)
cover the procedure, what it refuses, and how to verify it.
