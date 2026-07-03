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

In production cities, the `mol-dog-compactor` order handles compaction
automatically — see [Dolt Storage Maintenance](/runbooks/dolt-compact).

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
  It ships embedded in the dolt pack and runs `gc dolt compact` once a
  managed database crosses the commit threshold. Compaction fetches the
  configured remote, flattens live history, runs `CALL DOLT_GC('--full')`,
  and pushes the rewritten main branch back upstream. Dolt 1.86.x does not
  support an atomic `DOLT_PUSH('--force-with-lease', ...)`, so the script
  re-fetches and compares the remote head immediately before its force push.
  That check prevents known drift but cannot eliminate a remote write in the
  small fetch-to-push window.
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
| `post-flatten table value hash changed with row-count increase` | A table gained rows and its value hash changed. |
| `post-flatten table value hash changed without row-count increase` | A table's value hash changed without a row-count gain. |
| `post-flatten table list changed` | A table appeared or an invalid table name was observed after preflight. |
| `post-flatten table list probe failed` | The post-flatten `information_schema.tables` query failed. |
| `post-flatten value hash probe failed` | The database hash query failed after flatten. |
| `post-flatten value hash probe returned empty value` | The database hash query returned an empty value after flatten. |
| `post-flatten value hash changed with row-count increase` | The database hash changed after at least one stable-table row-count gain. |
| `post-flatten value hash changed without row-count increase` | The database hash changed without a row-count gain. |

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
