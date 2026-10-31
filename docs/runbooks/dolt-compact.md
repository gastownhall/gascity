---
title: Dolt Storage Maintenance
description: How Gas City automatically flattens Dolt commit history and runs DOLT_GC, and how to configure the process.
---

## Overview

Every bead mutation creates a Dolt commit. Over time this builds a large commit graph that `DOLT_GC` alone cannot reclaim. The `mol-dog-compactor` order in the dolt pack runs `gc dolt compact` on a schedule to flatten the graph and then reclaim orphaned chunks.

In production cities, `mol-dog-compactor` handles compaction automatically. If you need to recover from an already-bloated store, see [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery).

## How It Runs

The `mol-dog-compactor` order fires `gc dolt compact` every 2 hours. For each eligible database without shared remote history, `gc dolt compact` collapses history after the provenance watermark into one commit, then runs `CALL DOLT_GC('--full', '--archive-level=0')` to reclaim orphaned chunks. The flag keeps Dolt from writing archives whose duplicated zstd dictionaries can exhaust memory mid-GC (see [Dolt Bloat Recovery](/troubleshooting/dolt-bloat-recovery)). Databases with configured remotes default to working-set GC without rewriting shared history. If a database has fewer than `GC_DOLT_COMPACT_THRESHOLD_COMMITS` compactable commits (default: 2000), that database is skipped on that run.

The order file ships in the dolt pack, which is synced automatically, so do not edit `orders/mol-dog-compactor.toml` directly — local edits are overwritten. To change the cadence or the compactor's environment, add a city-level override to `city.toml`:

```toml
[[orders.overrides]]
name = "mol-dog-compactor"
interval = "4h"
env = { GC_DOLT_COMPACT_THRESHOLD_COMMITS = "5000" }
```

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `GC_DOLT_COMPACT_THRESHOLD_COMMITS` | `2000` | Skip flatten when the compactable commit count is below this. |
| `GC_DOLT_COMPACT_MIN_FREE_BYTES` | `5368709120` (5 GiB) | Skip compact if disk free falls below this. Set to `0` to disable. |
| `GC_DOLT_COMPACT_CALL_TIMEOUT_SECS` | `1800` | Hard timeout for each SQL CALL (flatten or GC). |

## Disk Preflight

Before compacting, `gc dolt compact` checks free space on the Dolt data volume. If free bytes fall below `GC_DOLT_COMPACT_MIN_FREE_BYTES` (default 5 GiB), no history is flattened and no pending-GC marker gets its full GC; the run retries at the next 2-hour interval. Databases with remotes still get their working-set GC, and `--gc-only` and bare-GC runs skip the check. Below the floor, only `gc dolt compact --only-db <database>` naming a single database with a pending-GC marker may finish that interrupted GC, and it cannot start a new flatten. A `GC_DOLT_COMPACT_ONLY_DBS` scope, including one set in an order override's `env`, never grants that recovery. Set the threshold to `0` to disable the check.

A run that holds work below the floor still exits successfully, so `mol-dog-compactor` records an `order.skipped` event rather than a plain completion. The outcome is `skipped` when no GC ran and `partial` when a working-set GC still ran. The reason carries the free bytes and the floor, and each held database is listed with why it was held:

```bash
gc events --type order.skipped --payload-match order_name=mol-dog-compactor
```

If free-space probing fails, the compactor exits without changing a database. Fix the filesystem probe or set the threshold to `0` only when deliberately disabling this guard.

## Observability

### Quarantine alerts

If the post-flatten integrity check detects unexpected data changes, the database is quarantined and a mail alert is sent to the configured recipient (`GC_DOLT_COMPACT_ALERT_TO`, default: `mayor`). A later scheduled run may clear one of the four documented value-hash drift markers only after `DOLT_DIFF_STAT` proves that drift is confined to content-preserved tables. Other markers remain until an operator verifies the recorded evidence and clears the affected database marker.

### Doctor checks

`gc doctor` includes a `dolt-compact-state` check that surfaces quarantine, pending-GC, authoritative pending-push, and backup pending-push markers. Its recovery hints use `gc dolt compact --only-db <database>`, after any required marker inspection or remote reconciliation. For a pending-GC marker on a database with a remote, the hint names two routes: the plain `--only-db` run finishes the GC locally and drops the deferred push, so `gc dolt sync` reports the database as diverged until you reconcile it; `GC_DOLT_COMPACT_ALLOW_FEDERATED=1 gc dolt compact --only-db <database>` completes the push, and belongs only in a compaction window announced to every clone. The `dolt-noms-size` check warns when a managed database's aggregate on-disk footprint crosses a warning or error byte-size threshold — an absolute size measurement, not a row-count ratio. A healthy compact cadence keeps both green.

## Troubleshooting Quick Reference

| Symptom | Meaning | Action |
|---------|---------|--------|
| `order.skipped` event for `mol-dog-compactor`, or `compact: disk CRITICAL` from a manual `gc dolt compact` | Disk free below `GC_DOLT_COMPACT_MIN_FREE_BYTES`; new compaction held. The event lists each held database. | Free disk space; compact retries automatically. List held runs with `gc events --type order.skipped --payload-match order_name=mol-dog-compactor`. For a pending-GC marker, run `gc dolt compact --only-db <database>` for that one database, as the `gc doctor` recovery hint describes. |
| Database appears in quarantine log | Post-flatten integrity check failed; DB flagged for manual review. | See [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) for manual GC procedure. |

## See Also

- [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) — manual GC recovery for a bloated store
- [Configuration Reference](/reference/config) — full `city.toml` configuration reference
