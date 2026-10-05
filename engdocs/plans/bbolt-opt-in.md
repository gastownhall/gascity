---
title: "bbolt Coordination Store"
description: Enable, verify, troubleshoot, and revert the embedded bbolt bead store.
---

Gas City uses the managed Dolt-backed `bd` store by default for city
coordination state. You can opt a city into the embedded bbolt backend when
you want a local single-file coordination store and do not need Dolt sync,
push, migration, or backup automation for beads.

Use bbolt for local development, isolated test cities, and small single-user
cities where the orchestrator is the only process opening the bead store. Keep
the default Dolt backend when you need existing bead history, multi-process
access, external sync or push workflows, or Dolt-based backup operations.

## Enable bbolt

Edit the city's `city.toml`:

```toml
[beads]
backend = "bbolt"
```

The `backend` setting applies only when the beads provider is the default
`bd` provider or is explicitly set to `provider = "bd"`. If you use
`provider = "file"` or an `exec:` provider, backend selection is inactive.

Restart the city after changing `city.toml`:

```bash
gc stop
gc start
```

On start, Gas City creates or opens:

```text
.gc/state/bbolt/beads.bolt
```

`gc start` prints one startup signal with the resolved city path:

```text
coord-store: using bbolt backend /path/to/city/.gc/state/bbolt/beads.bolt
```

The city-level managed Dolt server is not started for the coordination store
while bbolt is active. Rig-level Dolt checks and backup checks are separate
and are not disabled by the city coordination-store backend.

## Verify the Active Backend

Run doctor after the restart:

```bash
gc doctor --verbose
```

For a running bbolt-backed city, the `coord-store-backend` check reports:

```text
using bbolt coord-store backend at /path/to/city/.gc/state/bbolt/beads.bolt
```

If the file does not exist yet, doctor still treats the configuration as valid
and reports:

```text
using bbolt coord-store backend; store will be created on gc start
```

Verbose doctor output also includes the raw backend value, the beads provider,
and the resolved bbolt path.

For the default Dolt backend, the same check reports:

```text
using managed Dolt coord-store backend
```

## Fresh-Start Semantics

Switching from Dolt to bbolt does not migrate existing bead state. The first
`gc start` with `backend = "bbolt"` starts from the bbolt file's current
contents. If the file does not exist, the city starts with an empty bbolt bead
store.

The existing Dolt bead state is left in place but unused while bbolt is
active. If you later revert to Dolt, the bbolt file is preserved but unused,
and the Dolt-backed bead store becomes active again.

## Revert to Dolt

Stop the city:

```bash
gc stop
```

Then remove the `backend` line or set it back to Dolt:

```toml
[beads]
backend = "dolt"
```

Start the city again:

```bash
gc start
gc doctor --verbose
```

After reverting, `gc doctor --verbose` should report:

```text
using managed Dolt coord-store backend
```

The file at `.gc/state/bbolt/beads.bolt` remains on disk. Gas City does not
delete it during revert, and it does not copy bbolt-only beads into Dolt.

## Limitations

- bbolt is a single-process embedded store. Only one orchestrator should open a
  city's bbolt bead store at a time.
- bbolt state is local-only. There is no external sync, push, or shared-server
  workflow for bbolt-backed beads.
- Gas City does not migrate existing Dolt bead state into bbolt.
- Gas City does not migrate bbolt bead state back into Dolt.
- Dolt backup automation does not back up the bbolt coordination-store file.
  Back up `.gc/state/bbolt/beads.bolt` separately if you need to preserve it.

## Troubleshooting

### Invalid backend value

If `city.toml` contains an unsupported backend value, `gc start` fails instead
of silently falling back to Dolt. For example, `backend = "boltdb"` produces:

```text
bead store: unrecognized backend value "boltdb"
hint: valid values for [beads] backend are: "" (dolt, default), "dolt", or "bbolt"
hint: run `gc doctor` to see the currently active backend
```

Fix `city.toml`, then rerun:

```bash
gc doctor --verbose
gc start
```

`gc doctor` also reports invalid backend values through the
`coord-store-backend` check with this message:

```text
unrecognized backend value "boltdb"
```

and this fix hint:

```text
set [beads].backend to "" (dolt, default), "dolt", or "bbolt", then rerun gc doctor
```

### bbolt file is locked

If another orchestrator already has the bbolt file open, startup fails with a
lock-timeout message:

```text
bbolt bead store: open /path/to/city/.gc/state/bbolt/beads.bolt: timeout (5s): file is already locked
hint: another gc controller may already be running - run `gc status` to check
hint: if no other controller is running, remove /path/to/city/.gc/state/bbolt/beads.bolt.lock to clear a stale lock
```

First check whether the city is already running:

```bash
gc status
```

If an orchestrator is running, stop or reuse that orchestrator instead of
starting a second one. Only remove the `.lock` file after you have confirmed
that no other orchestrator is running for that city.

### bbolt path is a directory

If `.gc/state/bbolt/beads.bolt` exists as a directory, doctor reports:

```text
bbolt store path is a directory: /path/to/city/.gc/state/bbolt/beads.bolt
```

Move the directory aside so `gc start` can create the store file:

```bash
mv .gc/state/bbolt/beads.bolt .gc/state/bbolt/beads.bolt.dir-backup
gc start
```
