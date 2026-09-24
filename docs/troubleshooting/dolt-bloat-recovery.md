---
title: Recover from Dolt Bloat
description: Recover a Gas City beads store whose Dolt noms directory has grown out of proportion.
---

## Overview

Gas City stores beads in a managed Dolt server. Dolt records every write as
immutable chunks under `.beads/dolt/<database>/.dolt/noms/`. Chunks are
only reclaimed by garbage collection. Dolt's auto-GC fires when ~125 MB of
new chunks have accumulated since the last GC, which means a database that
bloated once during an agent storm and then went quiet will not auto-GC on
its own — it sits at its peak size indefinitely.

This runbook walks an operator through recovering such a database: stopping
writers, taking a safety backup, running a full GC with archive compression,
and verifying the result.

## Symptoms

1. `gc doctor` reports `dolt-noms-size` as **Warning** or **Error**.
2. `du -sh <cityPath>/.beads/dolt/<database>/.dolt/` returns more than
   20 GB.
3. `bd` writes feel slower than usual, or `bd ready` takes noticeable time.
4. Agents fail with Dolt connection or timeout errors, especially shortly
   after start.

## Preconditions

- **Stop all agents.** Run `gc stop` in the affected city so no session is
  writing to Dolt.
- **Ensure no external writers are connected.** If you have opened a
  `dolt sql` shell against the managed port, quit it.
- **Free disk space.** Dolt GC rewrites chunks into a new store before
  swapping; budget at least **2× the current `.dolt/` size** in free space
  on the same filesystem.
- **Final Dolt 2.1.0 or newer.** This matches the floor enforced by Gas
  City's managed Dolt tooling. Releases before 1.86.2 also have the upstream
  GC/writer deadlock fixed in dolthub/dolt commit `ccf7bde206`, which can hang
  `dolt_backup sync` under heavy write load. Check with
  `dolt version`. If your binary rejects `--archive-level=1` (rare on
  modern releases), drop the flag and run plain
  `dolt gc` — archive compression is default-on in 1.75+ so the flag is
  an optimization, not a requirement.

## Recovery Procedure

```bash
# 1. Stop the supervisor (and with it, all agents and the managed Dolt server).
gc stop <cityPath>

# 2. Capture a safety backup before touching the store.
cd <cityPath>/.beads/dolt/<database>
cp -a .dolt .dolt.bak-$(date +%Y%m%d-%H%M%S)

# 3. Run a full GC with archive compression. This is the step that actually
#    reclaims space. On a 120 GB store expect this to take tens of minutes.
dolt gc --archive-level=1

# 4. Restart the city and verify.
cd <cityPath>
gc start
gc doctor          # dolt-noms-size should now be OK
du -sh .beads/dolt/<database>/.dolt
```

If `gc doctor` reports a clean `dolt-noms-size` and agents come back up
cleanly, the recovery is complete. You may delete the `.dolt.bak-*`
directory at your leisure once you are confident in the new store.

## Reclaiming a database stranded below the compaction threshold

`gc dolt compact` skips any database with fewer commits than the threshold
(default 2000, `GC_DOLT_COMPACT_THRESHOLD_COMMITS`). A database can fall *below*
that threshold yet still carry orphaned chunks — most commonly after a prior
flatten squashed its history but the post-flatten full GC was deferred (a
concurrent writer raced the flatten), quarantined and later cleared, or
otherwise never completed. Scheduled compaction then skips the database forever
and the space is never reclaimed. The skip is visible in the compactor log as:

```
compact: db=<database> commits=<n> below_threshold=<t> oldgen_archives=present pending_gc=absent — skip ...
```

Use the operator-invoked reclaim path to recover such a database without waiting
for its commit count to climb back over the threshold:

```bash
# Reclaim one stranded database. Runs CALL DOLT_GC('--full') with no flatten,
# bypassing the commit-count threshold.
gc dolt compact --gc-only --only-db <database>

# Preview first, mutating nothing.
gc dolt compact --gc-only --only-db <database> --dry-run
```

`--gc-only` refuses any database under an integrity-quarantine marker; resolve
the underlying reason (see **Compact Quarantine Reasons** below) before
reclaiming. Unlike the full `dolt gc --archive-level=1` procedure above,
`--gc-only` runs against the live managed server and does not require stopping
the city — though quiescing writers still makes the GC faster and more
thorough.

## Compacting a city whose Dolt remote is uncredentialed

Before flattening (and again before pushing) the compactor runs
`CALL DOLT_FETCH('<remote>')` to reconcile against the remote. Against an
**uncredentialed git+https remote**, that call does not merely return an error —
it **crashes the managed Dolt sql-server process**. The shell tolerates a
non-zero return code ("proceeding from local source of truth") but cannot catch
a server-process death across the process boundary: the supervisor restarts the
server seconds later, but by then every remaining database's probe hits
`connection refused`, so one misconfigured remote takes down compaction for the
whole city.

If a city's remote is not (yet) credentialed, opt out of the fetch so
compaction runs entirely from the local source of truth. The post-compaction
remote push is deferred via a pending-push marker and resumes automatically on a
later run once the fetch path is healthy:

```bash
# Skip the fetch for every database this run.
gc dolt compact --skip-fetch

# Equivalent environment opt-out (e.g. set in a wrapper or on the city).
GC_DOLT_COMPACT_SKIP_FETCH=1 gc dolt compact

# Skip the fetch only for specific, known-uncredentialed databases (CSV);
# credentialed databases in the same city still fetch and push normally.
GC_DOLT_COMPACT_SKIP_FETCH_DBS=<database>[,<database>...] gc dolt compact
```

Prefer the per-database `GC_DOLT_COMPACT_SKIP_FETCH_DBS` form over the global
opt-out when only some databases are uncredentialed — the global form disables
remote sync for every database, including ones whose push would otherwise
succeed. Do **not** set the global opt-out in the shared `mol-dog-compactor`
order for the same reason; set the per-database env on the affected city
instead.

## Compacting a database that must never reach a remote

Some databases are deliberately local — a privacy boundary, or a store whose
remote was configured by accident. Mark those `.no-sync` and compaction skips
its remote phase entirely: no fetch, no push, and no deferred push recorded.
The database still flattens and GCs, so it keeps the disk benefit:

```bash
# Exclude one database from all remote sync, compaction included.
touch <cityPath>/.beads/dolt/<database>/.no-sync
```

`gc dolt sync` and `gc dolt pull` honor the same marker, so one file covers
every remote path.

Choose `.no-sync` over `--skip-fetch` when a database must never reach a
remote. `--skip-fetch` defers the push and waits for the remote to become
usable later; `.no-sync` states that it never will.

## History protection

`gc dolt compact` only squashes history that this city grew. Two rules
enforce this.

### Databases with remotes

A database with **any** configured Dolt remote is never flattened by the
scheduled compactor. The remote can be a team remote you cloned from, bd's
`refs/dolt/data` sync remote, a federated city, or a DoltHub mirror.
Flattening rewrites history, so the result would have to be force-pushed over
every other clone. Instead, compact logs:

```
compact: db=<database> remote=<remote> remotes=<n> — history may be shared with other clones; skipping flatten and remote push ...
```

It then runs a bare `CALL DOLT_GC()`, which reclaims journal and working-set
garbage without touching history. `.no-sync`, `--skip-fetch`, and `--dry-run`
do not bypass this guard.

Markers left by runs from before this guard existed:

- **`compact-pending-gc/<database>`**: the local full GC still runs. The
  deferred push is dropped, and the log warns that the remote still holds the
  full history. `gc dolt sync` then reports the database as diverged until you
  reconcile it. Either re-clone from the remote, or push the flattened history
  during an announced window (see below).
- **`compact-pending-push/<database>`**: the marker is held untouched and the
  log explains why. Nothing is force-pushed.

To flatten a database that has a remote, first announce a compaction window to
every clone of that history. Then run:

```bash
GC_DOLT_COMPACT_ALLOW_FEDERATED=1 gc dolt compact --only-db <database>
```

With this opt-in, compact flattens the database and **force-pushes** the
rewritten branch, plus any `file://` backup remotes. Every other clone must
then re-clone or hard-reset to the remote. Do not set the opt-in on the
scheduled `mol-dog-compactor` order.

### The `gc-compact-base` watermark

The first time compact sees a database, it creates the Dolt tag
`gc-compact-base` in it. The tag lives inside the database, so it survives
runtime-state wipes and travels with `dolt backup`. Branch pushes do not carry
it. Where the tag goes:

- **Root commit**: when the commit after root is a
  `compaction: flatten history` commit, meaning compact already manages the
  database. Flattening continues to squash all history, as before.
- **Root commit**: when the operator created
  `<cityPath>/.beads/dolt/<database>/.compact-full-history` before compact
  first saw the database.
- **HEAD**: in every other case, such as `gc rig add --adopt` of an existing
  `.beads/`, a `bd bootstrap` or `dolt clone`, or a database that was never
  compacted before the upgrade. Everything up to that point is kept as it is.

A flatten soft-resets to the tag, not to root, so it squashes only the
commits after the tag. The threshold counts only those commits too; the log
shows them as `commits_since_base=`. An adopted database with 4,000 commits
therefore keeps all 4,000 and gains one flatten commit each time it grows past
the threshold.

Inspect the tag with:

```bash
gc dolt sql -q "USE <database>; SELECT * FROM dolt_tags"
```

If the history is rolled back or restored to a point before the tag, the tag
is no longer an ancestor of HEAD. Compact then refuses to flatten that
database and fails the run until you act:

```
compact: db=<database> REFUSING flatten: gc-compact-base=<hash> is not an ancestor of HEAD=<hash> ...
```

- To keep the current history as it is, delete the tag. The next run sets it
  again at HEAD:

  ```bash
  gc dolt sql -q "USE <database>; CALL DOLT_TAG('-d', 'gc-compact-base')"
  ```

- If the whole history was grown by this city and should be fully compactable,
  run `touch <cityPath>/.beads/dolt/<database>/.compact-full-history` before
  deleting the tag. The next run then sets the tag at root.

A database adopted from **another** Gas City city whose compactor already
flattened it has a flatten commit right after root. Compact treats that
history as its own and squashes it on the next flatten. That history is
limited to what the source city's compactor would have squashed anyway. To
keep it, create the tag at HEAD before the first compact run:
`CALL DOLT_TAG('gc-compact-base', 'HEAD')`.

## Expected Outcome

DoltHub's archive format typically delivers ~30% compression on top of
normal GC ([DoltHub blog, archive storage](https://www.dolthub.com/blog/)).
Combined with reclamation of orphan chunks from agent churn, a 120 GB
pre-GC store typically drops to somewhere between **5 GB and 20 GB** —
depending on how much of the pre-GC size was live data versus orphan
chunks.

If GC finishes but the size barely moves, the chunks are nearly all live
(no garbage to collect). See **When to Escalate** below.

## Prevention

- **Keep Dolt at a final 2.1.0 or newer.** This matches Gas City's
  managed-Dolt floor; newer releases ship improved auto-GC heuristics and
  default archive compression.
- **Let the dolt pack's `mol-dog-compactor` order run continuously.**
  It ships embedded in the dolt pack and runs `gc dolt compact` every two
  hours. A database without remotes is flattened once the commits after its
  `gc-compact-base` watermark cross the threshold, and then gets
  `CALL DOLT_GC('--full')`. A database with remotes gets a bare
  `CALL DOLT_GC()` and is never flattened (see **History protection** above).
  Under the `GC_DOLT_COMPACT_ALLOW_FEDERATED=1` opt-in, compaction fetches the
  remote, flattens, and force-pushes the rewritten branch. Dolt does not
  support an atomic `DOLT_PUSH('--force-with-lease', ...)`, so the script
  fetches again and compares the remote head immediately before its force
  push. That check catches known drift, but a remote write can still land in
  the short window between fetch and push.
- **Mind `orders.max_timeout` if you set one.** The compactor order asks
  for a 24-hour timeout to accommodate serialized full-GC runs on large
  stores. A city-level `orders.max_timeout` below 24h will cap the
  compactor and may kill an in-progress GC; raise the cap or leave it
  unset if you want unattended recovery on big databases.
- **Run `gc doctor` regularly.** A daily cron or CI job is enough. The
  `dolt-noms-size` check gives early warning well before users notice.
- **Avoid long-lived `dolt sql` sessions from outside Gas City.** External
  clients hold open transactions that can block GC.

## Compact Quarantine Reasons

`gc dolt compact` writes exact reason strings into
`.gc/runtime/packs/dolt/compact-quarantine/<database>` when it detects
possible writer interference before full GC. Operator dashboards and runbooks
should treat these strings as the current vocabulary:

| Reason | Meaning |
|--------|---------|
| `post-flatten HEAD probe failed` | The compactor could not read the database HEAD after flatten. |
| `post-flatten integrity check failed` | A post-flatten integrity check failed before recording a more specific reason. |
| `post-flatten row count decreased` | A table lost rows after flatten. |
| `post-flatten row count probe failed` | The post-flatten row-count query failed or returned a non-number. |
| `post-flatten table value hash probe failed` | A post-flatten table hash query failed or returned empty. |
| `post-flatten table value hash changed with row-count increase` | A table gained rows and its value hash changed. *(auto-clearable — see below)* |
| `post-flatten table value hash changed without row-count increase` | A table's value hash changed without a row-count gain. *(auto-clearable — see below)* |
| `post-flatten table list changed` | A table appeared or an invalid table name was observed after preflight. |
| `post-flatten table list probe failed` | The post-flatten `information_schema.tables` query failed. |
| `post-flatten value hash probe failed` | The database hash query failed after flatten. |
| `post-flatten value hash probe returned empty value` | The database hash query returned an empty value after flatten. |
| `post-flatten value hash changed with row-count increase` | The database hash changed after at least one stable-table row-count gain. *(auto-clearable — see below)* |
| `post-flatten value hash changed without row-count increase` | The database hash changed without a row-count gain. *(auto-clearable — see below)* |

Four of these reasons are known writer-race false positives and are
**auto-clearable**. On its next scheduled run, `gc dolt compact`'s flatten
path re-proves content preservation with a fresh
`DOLT_DIFF_STAT(<marker flatten_preflight_head>, <current HEAD>)`: every
drifted table must report `rows_deleted=0` and `rows_modified=0`, and the
drift must stay confined to that proved set. On success compact removes the
marker, emits the usual alert and event, and continues through flatten and
full GC in the same cycle. Any probe failure, deleted or modified row, drift
outside the proved set, or any other reason keeps the marker and blocks GC.
You do not need to clear these by hand — check the compactor log first.

Quarantine markers also carry structured evidence. New markers include the
database name, the preflight/flatten/post-verify HEADs, preflight and
postflight database value hashes when available, `integrity_table_drift` for
table-level row/hash mismatches, `database_value_hash_drift` for aggregate hash
drift, and `decision=preserve_marker_manual_review_required`.

Alert-cadence bookkeeping is deliberately separate from that evidence. The
compactor stores `seen_count`, `notify_count`, and last-notified fields under
`.gc/runtime/packs/dolt/compact-notify-state/<marker-kind>/<database>`; repeated
checks must not rewrite the quarantine marker. The sidecar is operational
state, not integrity evidence. A changed reason or a replacement marker alerts
immediately, while an unchanged marker follows the bounded reminder cadence.

Safe marker-clear procedure:

1. Require a clean application worktree: `git status --short` should show no
   product/config/test changes you have not accounted for.
2. Confirm the Dolt server is reachable with `gc dolt status` and a live query
   such as `gc dolt sql --db <database> -q "SELECT COUNT(*) FROM issues"`.
3. Confirm bead queries are healthy for the affected store, for example
   `bd list --limit 1` in that rig or `gc bd list --rig <rig> --limit 1`.
4. Read the marker and retain it if the HEAD/hash/table evidence is incomplete
   or points at row loss. For table drift, compare the recorded HEADs with
   `DOLT_DIFF` / `DOLT_DIFF_STAT`; only clear when the diff proves preflight
   rows are still reachable and no unexpected table disappeared.
5. When the evidence proves no data loss, remove only that database's marker:
   `rm .gc/runtime/packs/dolt/compact-quarantine/<database>`.
6. Retry reclaim with `gc dolt compact --gc-only --only-db <database>`. If the
   marker returns or health checks fail, preserve the marker and escalate with
   the marker contents and command output.

`gc dolt compact --gc-only` and the bare-GC path refuse databases with
quarantine markers unconditionally. The scheduled `gc dolt compact` flatten
path also refuses, except for the four auto-clearable reasons above, which it
may clear on its own after proving content preservation. The refusal output
repeats the marker path, reason, key evidence fields, and the clear/retry
command so operators have the next action without opening this runbook first.

## When to Escalate

If a recovery GC reduces the store by less than ~10% and `gc doctor` still
flags `dolt-noms-size`:

1. All remaining chunks are probably live — the database legitimately
   contains this much history. Squashing Dolt history is not a supported
   self-service operation today; escalate instead.
2. File a `bd` issue with:
   - `dolt version` output
   - `du -sh` of the `.dolt/` directory
   - `dolt log --oneline | wc -l`
   - a sample of `dolt log --stat` from the busiest day

Attach the `gc doctor --verbose` output as well. Do not delete the
`.dolt.bak-*` directory while the issue is open.
