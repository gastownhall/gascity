# gc dolt compact

Flatten Dolt commit history on managed databases to reduce storage, then run a
full garbage collection to reclaim the orphaned chunks. Without flags, compact
runs in scheduled mode: it skips any database with fewer new commits than the
threshold (default 2000, `GC_DOLT_COMPACT_THRESHOLD_COMMITS`), flattens the
rest, verifies row preservation, and runs `CALL DOLT_GC('--full')`.

## Which databases compact touches

Compact works on one Dolt server, the one `GC_DOLT_HOST` and `GC_DOLT_PORT`
name for the city: its managed server, or a loopback server the city uses as
an external endpoint. It finds database names in the city's and each rig's
`.beads/metadata.json` and in that server's data directory.

- **A rig pinned to another Dolt endpoint is skipped.** Its database lives on
  that endpoint, not on this server, and gc does not compact a server it does
  not manage. This covers `gc rig set-endpoint <rig> --external`, a rig that
  runs its own Dolt (`--self`, or `[[rigs]] dolt_host`/`dolt_port`), a rig
  whose `.beads/config.yaml` names a Unix socket (`dolt.socket`), and a rig
  that bd binds to a remote host. Compact logs the rig, its endpoint, and
  `— skip`, and the run does not fail.
- **A same-named database on this server is not that rig's store.** It is
  usually the copy left behind when the rig moved. Compact leaves it alone,
  `--only-db` included, and warns, unless another rig on this server uses the
  same database name; then that rig's database is compacted as usual. Drop or
  archive a stale copy once you no longer need it. Check first when the rig's
  endpoint is a Unix socket, which may be this server's, or another port on
  this server's host, which may be stale: the database here may then still be
  the rig's live store, and compact's warning says so.
- **A rig whose `.beads/config.yaml` cannot be read is skipped too.** Compact
  cannot tell which endpoint the rig uses, so it warns with the file's path
  and treats the rig like a pinned one: its database, and a same-named copy
  on this server, wait until the file can be read. The run goes on.

Compact does not yet recognize every rig that uses another Dolt server. A rig
on a server that bd proxies, started itself, or binds through a Unix socket
without a gc pin, and a rig whose legacy `dolt.host` or bd binding names a
loopback address on another port, still have their databases looked up on
this server. The reverse also happens: a rig that bd binds to this server's
own host name on another port, which may be stale, or that names this
server's own Unix socket is skipped, though its database may still live here.

## History protection

Compact protects history that may be shared through a Dolt remote, and history
from before it first saw the database. The protection is by position, not by
author: every commit after the watermark is squashed, whichever city wrote it.

- **External endpoints are compacted only on a loopback host.** For an
  external endpoint on a non-loopback `GC_DOLT_HOST` (a remote or
  cloud-hosted server), compact exits 0 without connecting: no flatten, no
  GC, no watermark. Its history grows without bound. The city's own managed
  server is compacted however `GC_DOLT_HOST` addresses it. A bd-owned proxied
  store (the default for new cities) is not compacted.
- **A database several cities write to is squashed for all of them** when
  this city's managed server holds it or this city reaches it over loopback.
  Compact never filters by author, so every city's commits after the
  watermark go into the same flatten. To keep the shared database out, list
  the city's other databases in `GC_DOLT_COMPACT_ONLY_DBS` on the
  `mol-dog-compactor` order, or disable the order. See "Shared and non-local
  databases" in `docs/troubleshooting/dolt-bloat-recovery.md` (#7896).

- **Databases with a remote are never flattened.** Any configured Dolt remote
  (a team remote, bd's `refs/dolt/data`, a federated city) means other clones
  may share the history, and a flatten would have to be force-pushed over it.
  Compact skips the flatten and the push, and runs a bare `CALL DOLT_GC()`
  instead. `.no-sync`, `--skip-fetch`, and `--dry-run` do not bypass this.
  - A pending-GC marker left by an earlier flatten still gets its local
    `DOLT_GC('--full')`. The deferred push is dropped, and the log says so.
    The log also records the marker's `compacted_from_head` (the pre-flatten
    HEAD) before the GC runs.
  - A pending-push marker is held untouched. It is not force-pushed. Compact
    reports it on stderr, emits a `dolt.compact.quarantine` event, and mails
    the compactor alert recipient (`GC_DOLT_COMPACT_ALERT_TO`, default
    `mayor`), with the same reminder cadence as quarantine alerts.
  - `GC_DOLT_COMPACT_ALLOW_FEDERATED=1` lifts the guard. Flattened history is
    then **force-pushed** to the remote. Set it only for a compaction window
    you have announced to every clone of that history.
- **The `gc-compact-base` tag marks where compactable history starts.** Compact
  creates this Dolt tag the first time it sees a database, at any commit count.
  `--dry-run` never creates it.
  - The tag goes on the root commit when the commit after root is a
    `compaction: flatten history` commit, meaning compact already manages the
    database. It also goes on root when
    `<data_dir>/<db>/.compact-full-history` exists.
  - Otherwise the tag goes on HEAD. Adopted, imported, or pre-existing history
    is then kept as it is.
  - The tag also goes on HEAD, overriding both rules above, when more than one
    parentless commit is reachable from HEAD (unrelated histories merged).
  - A flatten squashes only the commits after the tag, and the threshold
    counts only those commits.
  - If the tag is not an ancestor of HEAD (for example after a rollback or
    restore to an earlier point), compact refuses the flatten and fails with
    instructions. Delete the tag with `CALL DOLT_TAG('-d', 'gc-compact-base')`
    and the next run sets it again by the same first-sight rule: at HEAD, or
    at root when the commit after root is a flatten commit or
    `.compact-full-history` exists. Create that marker before deleting the tag
    only if no other city, clone, or client outside this city has ever written
    to this database, including before this city started using it.

## Flags

- `--gc-only` — Reclaim orphaned chunks via `CALL DOLT_GC('--full')` on each
  database **regardless of commit count**, skipping the flatten path entirely.
  This is the sanctioned reclaim path for a database stranded *below* the
  flatten threshold with orphaned `oldgen` archives — for example after a prior
  flatten dropped its commit count below the threshold but its post-flatten full
  GC was deferred or never ran, so scheduled compaction skips it forever and the
  disk space is never reclaimed. Unlike a bare working-set GC, `--full` rewrites
  `oldgen`, so the orphaned history is actually freed. Refuses any database that
  carries an integrity-quarantine marker and prints the marker evidence plus the
  safe clear/retry requirements.

- `--only-db <name>` — Restrict the run to the named database. Repeatable, and
  augments `GC_DOLT_COMPACT_ONLY_DBS`. Use this to reclaim a single stranded
  database without touching the rest of the store.

- `--dry-run` — Print the intended actions without issuing any
  `DOLT_RESET` / `DOLT_COMMIT` / `DOLT_GC`.

- `--skip-fetch` — Bypass `CALL DOLT_FETCH` for every database (sets
  `GC_DOLT_COMPACT_SKIP_FETCH=1`). Against an uncredentialed git+https remote
  the fetch crashes the managed Dolt sql-server and cascades to every remaining
  database, so this opt-out lets compaction proceed from the local source of
  truth; the post-compaction remote push is deferred via a pending-push marker.
  To skip only specific known-uncredentialed databases while others fetch and
  push normally, set `GC_DOLT_COMPACT_SKIP_FETCH_DBS=<db>[,<db>...]` instead.

## Examples

```bash
# Recover a single database that scheduled compaction skips because it fell
# below the commit threshold but still holds orphaned oldgen chunks.
gc dolt compact --gc-only --only-db hq

# Preview what a full reclaim pass would touch, without mutating Dolt.
gc dolt compact --gc-only --dry-run
```

## The full GC and the listener read timeout

`CALL DOLT_GC('--full')` runs as one statement over the sql-server, and the
listener's `read_timeout_millis` caps any statement that produces no rows for
that long. The GC produces none until it finishes, so a reclaim that needs
longer than the ceiling fails with `context canceled`, `connection was closed`,
or `row read wait bigger than connection timeout` however healthy the store is.
The compactor names the setting and the live value when the GC ran as long as
the ceiling, and says the timeout did not end it when the GC was canceled
sooner. Only the full GC gets that line; a failed bare `CALL DOLT_GC()` prints
the server error without it.

A GC that fails after a flatten leaves a pending-GC marker that retries
`DOLT_GC('--full')` on every later run; `--gc-only` leaves no marker. When the
compactor blamed the ceiling, every retry hits the same ceiling until it moves;
when it said the timeout did not end the GC, raising `read_timeout_millis` is
not the remedy.

To raise the managed server's ceiling, set it in `city.toml` under `[dolt]
read_timeout_millis` and run `gc dolt restart` before retrying. Keep it under
half of `write_timeout_millis` (default `300000`): past `150000`, raise both, or
raise it only for the reclaim and put it back afterward. The compactor also caps
each call at `GC_DOLT_COMPACT_CALL_TIMEOUT_SECS` (default `1800`); a GC that
outruns that fails with `rc=124` and no server error. A reclaim that needs
longer belongs offline: follow the Recovery Procedure in
`docs/troubleshooting/dolt-bloat-recovery.md` with
`dolt gc --full --archive-level=1` as step 3.

See `docs/troubleshooting/dolt-bloat-recovery.md` for the full bloat-recovery
runbook, including quarantine marker evidence, the safe marker-clear procedure,
and when to stop writers and take a safety backup first.
