---
title: Dolt Storage Maintenance
description: How Gas City automatically flattens Dolt commit history and runs DOLT_GC, and how to configure the process.
---

## Overview

Every bead mutation creates a Dolt commit. Over time this builds a large commit graph that `DOLT_GC` alone cannot reclaim. The `mol-dog-compactor` order in the dolt pack runs `gc dolt compact` on a schedule to flatten the graph and then reclaim orphaned chunks.

In production cities, `mol-dog-compactor` handles compaction automatically. If you need to recover from an already-bloated store, see [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery).

## How It Runs

The `mol-dog-compactor` order fires `gc dolt compact` every 2 hours (configurable via the `interval` field in `orders/mol-dog-compactor.toml`). For each eligible database without shared remote history, `gc dolt compact` collapses history after the provenance watermark into one commit, then runs `CALL DOLT_GC('--full')` to reclaim orphaned chunks. Databases with configured remotes default to working-set GC without rewriting shared history. If a database has fewer than `GC_DOLT_COMPACT_THRESHOLD_COMMITS` compactable commits (default: 2000), that database is skipped on that run.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `GC_DOLT_COMPACT_THRESHOLD_COMMITS` | `2000` | Skip flatten when the compactable commit count is below this. |
| `GC_DOLT_COMPACT_MIN_FREE_BYTES` | `5368709120` (5 GiB) | Skip compact if disk free falls below this. Set to `0` to disable. |
| `GC_DOLT_COMPACT_CALL_TIMEOUT_SECS` | `1800` | Hard timeout for each SQL CALL (flatten or GC). |

## Disk Preflight

Before compacting, `gc dolt compact` checks free space on the Dolt data volume. If free bytes fall below `GC_DOLT_COMPACT_MIN_FREE_BYTES` (default 5 GiB), a normal run is skipped and retried at the next 2-hour interval. A targeted `--only-db` retry for an existing pending-GC marker may finish that interrupted GC, but cannot start a new flatten. Set the threshold to `0` to disable the check.

If free-space probing fails, the compactor exits without changing a database. Fix the filesystem probe or set the threshold to `0` only when deliberately disabling this guard.

## Observability

### Quarantine alerts

If the post-flatten integrity check detects unexpected data changes, the database is quarantined and a mail alert is sent to the configured recipient (`GC_DOLT_COMPACT_ALERT_TO`, default: `mayor`). A later scheduled run may clear one of the four documented value-hash drift markers only after `DOLT_DIFF_STAT` proves that drift is confined to content-preserved tables. Other markers remain until an operator verifies the recorded evidence and clears the affected database marker.

### Doctor checks

`gc doctor` includes a `dolt-compact-state` check that surfaces quarantine, pending-GC, authoritative pending-push, and backup pending-push markers. Its recovery hints use `gc dolt compact --only-db <database>`, after any required marker inspection or remote reconciliation. The `dolt-noms-size` check warns when a managed database's aggregate on-disk footprint crosses a warning or error byte-size threshold — an absolute size measurement, not a row-count ratio. A healthy compact cadence keeps both green.

## Troubleshooting Quick Reference

| Symptom | Meaning | Action |
|---------|---------|--------|
| `compact: disk CRITICAL` in logs | Disk free below `GC_DOLT_COMPACT_MIN_FREE_BYTES`; new compaction skipped. | Free disk space; compact retries automatically. For a pending-GC marker, follow the targeted `gc doctor` recovery hint. |
| Database appears in quarantine log | Post-flatten integrity check failed; DB flagged for manual review. | See [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) for manual GC procedure. |

## See Also

- [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) — manual GC recovery for a bloated store
- [Configuration Reference](/reference/config) — full `city.toml` configuration reference
