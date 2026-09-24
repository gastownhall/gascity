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
  - A pending-push marker is held untouched and reported. It is not
    force-pushed.
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
  - A flatten squashes only the commits after the tag, and the threshold
    counts only those commits.
  - If the tag is not an ancestor of HEAD (for example after a rollback or
    restore to an earlier point), compact refuses the flatten and fails with
    instructions. Delete the tag with `CALL DOLT_TAG('-d', 'gc-compact-base')`
    to set it again at HEAD. To set it at root instead, create
    `.compact-full-history` before deleting the tag.

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

See `docs/troubleshooting/dolt-bloat-recovery.md` for the full bloat-recovery
runbook, including quarantine marker evidence, the safe marker-clear procedure,
and when to stop writers and take a safety backup first.
