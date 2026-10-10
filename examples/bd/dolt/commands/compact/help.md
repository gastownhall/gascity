# gc dolt compact

Flatten Dolt commit history on managed databases to reduce storage, then run a
full garbage collection to reclaim the orphaned chunks. Without flags, compact
runs in scheduled mode: it skips any database with fewer new commits than the
threshold (default 2000, `GC_DOLT_COMPACT_THRESHOLD_COMMITS`), flattens the
rest, verifies row preservation, and runs `CALL DOLT_GC('--full')`.

## History protection

Compact only rewrites history that this city grew.

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
    only if this city grew the whole history.

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
