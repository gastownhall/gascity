# Plan: vp-pt73.5 — Bulk-delete/prune guard (backup-first + confirm)

## Context

Root cause confirmed in bead notes: `runOrderTrackingRetentionWatchdog`
(`cmd/gc/city_runtime.go:1512`) runs automatically inside every controller patrol
tick, calling `sweepClosedOrderTrackingRetentionAcrossStoresBounded` with no backup
freshness check. On 2026-06-22 at 20:55:56–20:56:14 it deleted 28 va-* closed
order-tracking beads (1/sec) after a 121 s patrol tick.

Investigation phase is complete. This plan covers the implementation of three guards:

1. **Watchdog path** — skip + warn when no recent backup exists.
2. **Manual `gc order sweep-tracking`** — require `--confirm` when projected
   deletions exceed a configurable threshold (default 20).
3. **`reaper.sh` bd prune block** — skip prune + record anomaly when backup is
   stale/absent.

## Env vars introduced

| Var | Default | Meaning |
|-----|---------|---------|
| `GC_BACKUP_MAX_AGE_FOR_BULK_DELETE` | `24h` (86400 s) | Maximum backup age for bulk deletes to proceed |
| `GC_BULK_DELETE_CONFIRM_THRESHOLD` | `20` | Manual sweep requires `--confirm` above this count |

## Files touched

| File | Change |
|------|--------|
| `internal/doctor/checks_bd_backup_freshness.go` | Export `BulkDeleteSafe` helper |
| `internal/doctor/checks_bd_backup_freshness_test.go` | Tests for `BulkDeleteSafe` |
| `cmd/gc/city_runtime.go` | Backup-age gate in `runOrderTrackingRetentionWatchdog` |
| `cmd/gc/city_runtime_test.go` | Test for watchdog gate |
| `cmd/gc/order_dispatch.go` | `countClosedOrderTrackingRetentionEligible` helper |
| `cmd/gc/order_dispatch_test.go` | Test for count helper |
| `cmd/gc/cmd_order.go` | `--confirm` flag + threshold check |
| `cmd/gc/cmd_order_test.go` | Test for confirm gate |
| `internal/bootstrap/packs/core/assets/scripts/reaper.sh` | Backup-age check before bd prune |
| `test/reaper_prune_backup_guard_test.sh` | Shell test for reaper guard |

## GDPR data-flow impact

None. Order-tracking beads contain internal job metadata only (order names, timestamps,
execution IDs). No personal data, no change to data-subject flows.

## MDR Class I traceability

No clinical pipeline involvement. Guards operate on infrastructure beads, not the
voxmemo→voxist-api clinical chain.

## Micro-tasks

| id | description | acceptance | est_minutes | slings |
|----|-------------|------------|-------------|--------|
| T-001 | Write failing tests for `BulkDeleteSafe(cityPath string, cfg *config.City, maxAge time.Duration, now time.Time) (safe bool, reason string)` in `internal/doctor/checks_bd_backup_freshness_test.go`: (a) all scopes fresh → safe=true; (b) one scope stale → safe=false with scope label in reason; (c) no backup_state.json in any scope → safe=true (no backup configured is not this check's responsibility) | `TestBulkDeleteSafe` is red | 4 | — |
| T-002 | Implement `BulkDeleteSafe` in `internal/doctor/checks_bd_backup_freshness.go`: collect managed scope roots via `managedDoltScopeRootsForConfig(cityPath, cfg, nil)`, call `scanBackupFreshness` per scope, return safe=false + label+age in reason for any stale scope | `TestBulkDeleteSafe` is green; `go test ./internal/doctor/...` passes | 4 | — |
| T-003 | Write failing test for backup gate in watchdog: `TestOrderTrackingRetentionWatchdogSkipsStaleBackup` in `cmd/gc/city_runtime_test.go` — set up a city with a stale backup_state.json, call `runOrderTrackingRetentionWatchdog`, assert zero beads deleted and a non-empty stderr warning | `TestOrderTrackingRetentionWatchdogSkipsStaleBackup` is red | 4 | — |
| T-004 | Implement backup-age gate in `runOrderTrackingRetentionWatchdog` (`city_runtime.go:1512`): before calling `sweepClosedOrderTrackingRetentionAcrossStoresBounded`, call `doctor.BulkDeleteSafe(cr.cityPath, cr.cfg, bulkDeleteMaxAge(cr.cfg), now)`; if !safe, write `cr.logPrefix + ": order-tracking retention watchdog: skipping bulk delete — " + reason` to stderr and return | `TestOrderTrackingRetentionWatchdogSkipsStaleBackup` is green; `go test ./cmd/gc/...` passes | 4 | — |
| T-005 | Write failing tests for two things in `cmd/gc/`: (a) `countClosedOrderTrackingRetentionEligible(stores, now, policy, onlyOrders)` returns correct count without deleting; (b) `cmdOrderSweepTrackingWithOptions` returns exit 1 with descriptive message when count > `GC_BULK_DELETE_CONFIRM_THRESHOLD` and confirm=false | `TestCountClosedOrderTrackingRetentionEligible` and `TestOrderSweepTrackingRequiresConfirm` are red | 4 | — |
| T-006 | Implement: (a) `countClosedOrderTrackingRetentionEligible` in `order_dispatch.go` — mirrors List logic of `sweepClosedOrderTrackingRetention` but returns count with no deletes; (b) add `confirmFlag bool` param to `cmdOrderSweepTrackingWithOptions` and `--confirm` cobra flag; before the real sweep, count eligible deletions and if count > threshold and !confirm, print `"gc order sweep-tracking: %d beads would be deleted — rerun with --confirm to proceed (GC_BULK_DELETE_CONFIRM_THRESHOLD=%d)"` and return 1 | `TestCountClosedOrderTrackingRetentionEligible` and `TestOrderSweepTrackingRequiresConfirm` are green; `go test ./cmd/gc/...` passes | 5 | — |
| T-007 | Write `test/reaper_prune_backup_guard_test.sh` following the pattern of `test/reaper_session_pattern_test.sh`: extract Step 6 block, run with (a) no backup_state.json present (expected: bd NOT called, anomaly recorded), (b) fresh backup_state.json (expected: bd IS called), (c) stale backup_state.json > 86400 s (expected: bd NOT called, anomaly recorded) | `bash test/reaper_prune_backup_guard_test.sh` exits 0 with FAIL lines only (red test) | 4 | — |
| T-008 | Implement backup-freshness guard in `reaper.sh` bd prune block (before current line 1119): read `$CITY_BEADS_DIR/backup/backup_state.json`; compute age in seconds; if absent or age > `${GC_BACKUP_MAX_AGE_FOR_BULK_DELETE:-86400}`, call `record_anomaly session "bulk prune skipped: backup stale or absent (age=${AGE}s threshold=${MAX_AGE}s)"` and skip the `bd prune` call | `bash test/reaper_prune_backup_guard_test.sh` exits 0 with PASS lines (green) | 4 | — |

## Open questions

None. Root cause is confirmed; guard approach is directly specified in the bead notes.
