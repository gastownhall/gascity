---
title: Dolt Storage Maintenance
description: How Gas City automatically flattens Dolt commit history and runs DOLT_GC, and how to configure the process.
---

## Overview

Every bead mutation creates a Dolt commit. Over time this builds a large commit graph that `DOLT_GC` alone cannot reclaim. The `mol-dog-compactor` order in the dolt pack runs `gc dolt compact` on a schedule to flatten the graph and then reclaim orphaned chunks.

In production cities, `mol-dog-compactor` handles compaction automatically. If you need to recover from an already-bloated store, see [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery).

## How It Runs

The `mol-dog-compactor` order fires `gc dolt compact` every 2 hours (configurable via the `interval` field in `orders/mol-dog-compactor.toml`). For each database, `gc dolt compact` does two things: it flattens commit history into a single commit, then runs `CALL DOLT_GC('--full')` to reclaim orphaned chunks. If a database has fewer than `GC_DOLT_COMPACT_THRESHOLD_COMMITS` commits (default: 2000), that database is skipped on that run.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `GC_DOLT_COMPACT_THRESHOLD_COMMITS` | `2000` | Skip flatten when commit count is below this. |
| `GC_DOLT_COMPACT_MIN_FREE_BYTES` | `5368709120` (5 GiB) | Skip compact if disk free falls below this. Set to `0` to disable. |
| `GC_DOLT_COMPACT_BACKUP_REMOTE` | _(none)_ | When set, runs `dolt backup sync <remote>` for each database before flattening. On failure, compact aborts. |
| `GC_DOLT_COMPACT_BACKUP_TIMEOUT_SECS` | `300` | Wall-clock timeout for each backup sync call. |
| `GC_DOLT_COMPACT_CALL_TIMEOUT_SECS` | `1800` | Hard timeout for each SQL CALL (flatten or GC). |

## Optional Safety Features

### Pre-compact backup

Set `GC_DOLT_COMPACT_BACKUP_REMOTE=<remote-name>` to snapshot each database before its history is rewritten. If backup fails, compact aborts rather than proceeding without a rollback point. Requires a named dolt backup remote configured in each database directory.

### Disk preflight

Before compacting, `gc dolt compact` checks free space on the Dolt data volume. If free bytes fall below `GC_DOLT_COMPACT_MIN_FREE_BYTES` (default 5 GiB) the run is skipped and retried at the next 2-hour interval. This prevents compaction from aggravating a full-disk situation. Set to `0` to disable.

## Observability

### Quarantine alerts

If the post-flatten integrity check detects unexpected data changes, the database is quarantined and a mail alert is sent to the configured recipient (`GC_DOLT_COMPACT_ALERT_TO`, default: `mayor`). A quarantined database is skipped on future compact runs until the marker is manually cleared. Known false positives (writer-race conditions) are cleared automatically.

### Doctor checks

`gc doctor` includes a `dolt-compact-state` check that surfaces quarantine markers, pending-GC state, and store-size heuristics. The `dolt-noms-size` check warns when the noms store has grown beyond the size-to-row-count ratio threshold. A healthy compact cadence keeps both green.

## Troubleshooting Quick Reference

| Symptom | Meaning | Action |
|---------|---------|--------|
| `compact: disk CRITICAL` in logs | Disk free below `GC_DOLT_COMPACT_MIN_FREE_BYTES`; compact skipped. | Free disk space; compact retries automatically at next interval. |
| `compact: db=<name> backup sync … failed` | Pre-compact backup failed; compact aborted. | Check backup remote reachability; fix remote and wait for next run. |
| Database appears in quarantine log | Post-flatten integrity check failed; DB flagged for manual review. | See [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) for manual GC procedure. |

## See Also

- [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) — manual GC recovery for a bloated store
- [Configuration Reference](/reference/config) — full `city.toml` configuration reference
