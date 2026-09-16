---
title: Migrating a legacy GC-managed city to bd's proxied-server topology
description: The supported rc.2 procedure for moving an existing gc-owned Dolt city onto a bd-owned proxy, what gc beads city migrate-proxied does, and how to recover.
---

# Migrating a legacy GC-managed city to proxied-server

> **Status:** Supported, and on beads `v1.3.0-rc.2` the only one. The journaled
> ownership handoff is **not available**: it needs phased `bd migrate
> ownership-handoff` verbs no beads release has yet, and when it lands gc will
> orchestrate it by calling those verbs — bd never calls gc. Until then this
> procedure is how an existing city moves, and it is deliberately fail-closed:
> every scope it will not touch is refused by name.
>
> **Companion to:** `engdocs/design/beads-proxied-local-default.md` (what a
> proxied scope is and who owns its processes).

## What "legacy GC-managed" means

A city initialised before the proxied-local default runs **one gc-owned
`dolt sql-server`** in Dolt's multi-database data-dir mode over
`<city>/.beads/dolt`. Every rig's database lives inside that same directory —
`hq/` for the city, one directory per rig — and no rig has a Dolt root of its
own. On disk:

- `<scope>/.beads/metadata.json` → `"dolt_mode": "server"`
- `<scope>/.beads/config.yaml` → `gc.endpoint_origin: managed_city` (city) or
  `inherited_city` (rig), plus gc's `dolt.mode: server` mirror
- `<city>/.gc/runtime/packs/dolt/dolt-state.json` while the server is up
- no `.gc/scope-ownership.json` — the journal postdates these cities

After the migration bd owns the process: a `bd db-proxy-child` supervising a
`dolt sql-server`, rooted at the city's data dir, serving the city database and
every rig database exactly as the one gc-owned server used to.

## The command

```bash
gc beads city migrate-proxied [--dry-run] [--json] [--rig NAME]...
```

It is a **thin orchestrator of bd's own migration**, not a second
implementation. bd owns the journal, the mode flip and the sidecar; gc owns the
ordering, the refusals and the residue bd cannot see. It is idempotent: an
already-proxied scope reports `already-migrated` and the command exits 0, so a
partially failed run is finished by rerunning it.

Per scope, in order, city first:

1. **Classify.** Migrate only a legacy GC-managed direct Dolt scope. Anything
   else refuses typed and names why: an already-proxied scope
   (`already-migrated`, exit 0), an embedded or doltlite scope, a scope that
   tracks an external Dolt endpoint, a scope already handed to the provider by
   the ownership handoff, or a scope journaled in `.gc/scope-ownership.json`.
2. **Fence the legacy server.** Refuse unless gc's managed Dolt is
   demonstrably down: no `<city>/.gc/runtime/packs/dolt/dolt-state.json`, no
   live process holding a Dolt store lock under the data dir, and nothing
   listening on the port in `<city>/.beads/dolt-server.port`. Re-checked
   immediately before each `bd migrate`, never once at entry.
3. **`dolt init` the city's data dir** when `.dolt/repo_state.json` is missing.
   bd's migrate validates that its root is a real Dolt repository; gc's
   multi-database data dir never was one. This is additive and non-destructive
   — it writes `.dolt/{config.json,noms/,repo_state.json}` beside the existing
   database directories and leaves every byte of them alone.

   It is the **city's** data dir and no other, and only when the city's own
   database is in there (or the directory is empty, in which case there is
   nothing to lose). A data dir holding databases but not this city's refuses:
   the init would succeed, bd would come up on a fresh empty store, and every
   database beside it would be orphaned without a word from either side.
4. **Point a shared-root rig at the city's data dir.** A rig whose database
   lives in the city's dir gets a **relative** `dolt_data_dir` (e.g.
   `../../.beads/dolt`) in its `metadata.json`, so bd roots it where its data
   actually is. Relative is mandatory: beads silently drops an absolute
   `dolt_data_dir` when it saves the config, and `bd migrate` saves the config
   partway through the flip. A rig with its own non-empty `.beads/dolt` *and* a
   database in the city's dir refuses — two candidate stores is not gc's
   ambiguity to resolve silently. So does a rig gc cannot place at all: one
   whose `.beads/dolt` is not a Dolt repository and whose database is not in
   the city's dir either. gc does not `dolt init` a rig.

   The classifier reads the `dolt_data_dir` a scope already records, so a rig
   whose key was written by a run that then failed at `bd migrate` resumes on
   the next run instead of refusing.
5. **Run `bd migrate from-server-to-proxied-server --idle-timeout 0`** in the
   scope. `--idle-timeout 0` is bd's `IdleTimeoutNever` and is required: without
   it the proxy and its Dolt child retire after 30s idle and every later command
   pays a cold start.
6. **Verify and normalise.** Confirm bd's outcome from `metadata.json` and the
   absence of its in-flight journal, then rewrite `.beads/config.yaml` through
   gc's canonical writer. For a proxied scope that state carries no `dolt.mode`
   and no `dolt.host`/`port`/`user`, so gc's pre-migration keys are dropped.
7. **Retire gc's own publication.** `<scope>/.beads/dolt-server.port`, and for
   the city `<city>/.gc/runtime/packs/dolt/{dolt-state.json,
   dolt-provider-state.json,dolt.pid,dolt.lock,dolt-config.yaml,dolt.log}` plus
   the directory itself when nothing else is in it. All of it describes a
   server that will not run for this city again, and no lifecycle comes back
   for it: `clearManagedDoltRuntimeStateUnlessBound` returns early for a scope
   with a complete bd binding, which is exactly what the migration created.
8. **`bd ping`** the migrated scope and report.

The command prints a per-scope table (or `--json`) and exits non-zero if any
scope failed. **Completed scopes stay migrated** — the failure is per scope, not
a rollback.

Nothing is written to `.gc`. A scope whose `metadata.json` says
`proxied-server` is classified provider-owned on the strength of that binding
alone, which is what makes an un-journaled migrated city work.

## Procedure

```bash
# 0. Know what you are about to change.
gc beads city migrate-proxied --dry-run

# 1. Stop the city. This is mandatory, not hygiene — see the hazard below.
gc stop <city>

# 2. Migrate. --json if you are scripting it.
gc beads city migrate-proxied

# 3. Backfill anything the old init predates (custom bead types, typically),
#    then confirm the city is green.
gc doctor --fix
gc doctor

# 4. Bring it back up and spot-check the data through the front door.
gc start
gc bd list
gc status
```

To migrate only some rigs, pass `--rig NAME` (repeatable). The city is always
included: a rig that shares the city's data dir cannot be proxied while the city
is still direct.

## The hazard this fences

**bd cannot see a server Gas City started.** `bd migrate`'s running-server
precondition consults only bd's own `.beads/dolt-server.pid`, and gc never
writes that file — gc writes a `.port` mirror and its own runtime state. So bd,
finding no pid file, concludes nothing is running, **commits the mode flip**,
and then fails to start its proxy because Dolt still holds the exclusive
data-dir lock:

```
Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting
       for proxy to become ready on its OS-assigned port
# .beads/dolt/server.log:
#   database "dolt" is locked by another dolt process
```

The scope is then unusable through bd until the gc-owned server dies. It is an
outage, not data loss — `gc stop` followed by `bd ping` recovers it — but
`migrate-proxied` refuses before bd is ever invoked so it cannot happen.

Related rc.2 sharp edge: `bd migrate --dry-run` returns *before* its Dolt-root
validation, so a dry run "passes" for a root the real run refuses. gc's own
`--dry-run` reports the `dolt init` step it would take, and does not rely on
bd's.

## Verifying the result

```bash
# Topology: bd's proxy and its Dolt child, rooted at the city's data dir.
pgrep -af 'db-proxy-child|dolt sql-server'

# Mode lives in metadata.json, and only there.
cat <city>/.beads/metadata.json          # "dolt_mode": "proxied-server"
grep dolt.mode <city>/.beads/config.yaml # no match

# A shared-root rig names the city's data dir, relatively.
cat <rig>/.beads/metadata.json           # "dolt_data_dir": "../../.beads/dolt"

# Sidecar: idle-never, rooted at the city.
cat <scope>/.beads/proxied_server_client_info.json   # "idle_timeout": -1
```

`gc start` must **not** republish `<city>/.gc/runtime/packs/dolt/dolt-state.json`
and must not start a `dolt sql-server` of its own. `gc stop` retires every
proxy and is re-runnable. Because the rigs share the city's proxy root, the
process count after the migration is the same as before it: one server, one
database per rig.

## Recovery

- **A scope failed, the rest succeeded.** Fix what the message names and rerun
  the command. Completed scopes report `already-migrated` and are skipped.
- **The migration committed but bd cannot open the store.** Almost always a
  live legacy server. `gc stop <city>`, then `bd ping` in the scope.
- **Doctor is red with `dolt runtime state unavailable`.** A stale
  `dolt.mode: server` in `config.yaml` shadowing `metadata.json`. Any canonical
  write clears it now; rerunning `gc beads city migrate-proxied` is the
  supported way to force one.
- **Going back.** There is no supported reverse migration on rc.2. Restore the
  city from backup.

## Residue

There is nothing to delete by hand. Everything the old lifecycle published
about this city — the whole `<city>/.gc/runtime/packs/dolt` publication and
every scope's `.beads/dolt-server.port` mirror — is retired by the command, on
the run that migrates the scope and on any later rerun over an already-migrated
one. Rerunning `gc beads city migrate-proxied` is the documented way to clear a
city that was migrated by an earlier build and still carries the publication.

`<scope>/.beads/dolt.gate.lock` survives, and should: it is bd's file, under
bd's directory, and gc does not delete other owners' state.

## Testing

`TestBeadsMigrateLegacyCityToProxied` and
`TestBeadsMigrateProxiedRefusesLiveLegacyServer`
(`test/acceptance/beads_migrate_proxied_test.go`, tag `acceptance_a`) run this
whole path through the real front doors. They need two binaries and skip typed
without either:

| Variable | Meaning |
|---|---|
| `GC_ACCEPTANCE_BD_BIN` | a bd ≥ `1.3.0-rc.2` with proxied-server support (a real `dolt` must also be on PATH) |
| `GC_ACCEPTANCE_LEGACY_GC_BIN` | a gc built from a revision that still initialises the legacy GC-managed direct topology — the fixture has to be written by the old binary, not simulated by the new one |

```bash
GC_ACCEPTANCE_BD_BIN=/path/to/bd \
GC_ACCEPTANCE_LEGACY_GC_BIN=/path/to/legacy/gc \
  go test -tags acceptance_a -run TestBeadsMigrate ./test/acceptance
```
