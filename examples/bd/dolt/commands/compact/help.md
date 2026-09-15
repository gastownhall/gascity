# gc dolt compact

Flatten Dolt commit history on managed databases to reduce storage, then run a
full garbage collection to reclaim the orphaned chunks. Without flags, compact
runs in scheduled mode: it skips any database below the commit-count threshold
(default 2000, `GC_DOLT_COMPACT_THRESHOLD_COMMITS`), flattens the rest, verifies
row preservation, and runs `CALL DOLT_GC('--full')`.

## Runtime guards

Every invocation reads the target city's resolved `[maintenance.dolt] enabled`
flag using `gc --city "$GC_CITY_PATH" config show --json` (parsed with `jq`).
When false, compact exits successfully with
`compact: maintenance.dolt enabled=false, skipping` before any Dolt operation.
This includes `--gc-only`, `--dry-run`, and `GC_DOLT_COMPACT_BARE_GC`.
An omitted flag resolves to false under the existing maintenance config default.
An unreadable config or non-boolean flag fails closed. Set
`GC_DOLT_COMPACT_FORCE=1` for a manual announced run that overrides this flag.

Flatten mode also skips any database with **any configured Dolt remote**, naming
the database and a remote in its output. This protects shared history even with
`.no-sync`, `--skip-fetch`, or `--dry-run`, and leaves deferred GC/push markers
untouched. Remote probe failures fail closed. Set
`GC_DOLT_COMPACT_ALLOW_FEDERATED=1` only during an announced compaction window
coordinated with every city sharing that history. FORCE does not bypass this
guard, and ALLOW_FEDERATED does not bypass the maintenance flag; a disabled city
needs both switches to flatten a federated database.

The remote guard does not apply to bare working-set GC
(`GC_DOLT_COMPACT_BARE_GC=1`) or `--gc-only`, which reclaim chunks without
flattening history or pushing remotes. Both still honor the maintenance flag
and quarantine checks.

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
