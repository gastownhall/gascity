# Runbook: Dolt store maintenance

Periodic compaction (`CALL DOLT_GC()`) and pre-gc snapshotting
(`dolt backup sync`) for the city's `.beads/dolt/` store.

The loop runs **inside the supervisor process** when opted in via
`city.toml`. There is no separate daemon, cron, or systemd unit.

## Current wiring status

**Observe-only mode (as of this release).** The SQL connection to Dolt
(`OpenDoltOps`) and the backup runner (`OpenDoltBackup`) are not yet
wired in the production controller. When `[maintenance.dolt]
enabled=true`, the loop runs on schedule, emits events, and records
history — but steps 2 (snapshot) and 3 (compaction) below are no-ops.
The supervisor logs `store-maintenance: enabled in observe-only mode
(snapshot and DOLT_GC not yet wired)` at startup.

Production wiring of the snapshot and GC factories will land in a
follow-up. Until then, `gc maintenance status` and the events are
meaningful for scheduling/alert testing, but a `gc.store.maintenance.done`
event does **not** imply compaction occurred.

## What this runs

For each scheduled cycle the supervisor:

1. Acquires the in-process maintenance lease (manual triggers contend
   on the same lease and return `409` on overlap).
2. **Snapshot** — `dolt backup sync` to
   `<city>/.beads/dolt-backups/current/`, then atomically rotates
   `current/` to `success/<timestamp>/` (see *Snapshot layout* below).
3. **Compaction** — `CALL DOLT_GC()` against the managed Dolt server,
   bounded by `gc_timeout` (default `10m`).
4. **Smoke test** — `SELECT COUNT(*) FROM issues` (5 s timeout) to
   confirm the store is readable.
5. **Prune** — keep the 3 newest successful snapshots and the most
   recent failed snapshot; older entries are removed. Prune errors
   are logged but do not regress a successful run.
6. **Emit event** — `gc.store.maintenance.done` (with
   `duration_s`, `before_bytes`, `after_bytes`) on success, or
   `gc.store.maintenance.failed` (with `stage`, `error_msg`,
   `snapshot_path`) on failure.
7. On failure, if `alert_to` is set, send one operator alert mail.

The default cadence is **weekly** (168 h), with ±10 % jitter.

## Opt in

Maintenance is **off by default**. Existing cities are unaffected
until they explicitly opt in.

1. Add to `city.toml`:

   ```toml
   [maintenance.dolt]
   enabled = true
   # All other keys are optional; defaults shown:
   # interval   = "168h"   # weekly
   # gc_timeout = "10m"
   # alert_to   = ""       # no alert until set; see "Alerting" below
   ```

2. Restart the controller so the loop picks up the new config:

   ```sh
   gc stop && gc start
   ```

   `gc reload` does **not** start the maintenance loop; the loop
   wires into supervisor bootstrap and only starts on (re)start.

> **First-run warm-up.** The first cycle after enablement on a large
> store may take **10–60 s** (chunk-pool walk during gc plus the
> initial backup). Schedule the restart in an off-peak window, or
> trigger it manually (see *Manual trigger*) so you can watch the
> outcome.

## Alerting

When `alert_to` names a recipient, every failed run sends one mail:

- **To:** the value of `alert_to` (e.g. `gascity/mayor`).
- **Subject:** `[ALERT] Dolt store maintenance failed: <stage>`
  where `<stage>` is one of `backup`, `gc`, `smoke-test`, or `prune`.
- **Body:** the failing stage, error message, and snapshot path
  (when one was taken).

Mail delivery is **best effort**: a mail send failure is logged but
does not affect the maintenance run's recorded outcome. Failures with
`alert_to = ""` skip the mail and are visible only via events and
`gc maintenance status`.

## Observability

Two surfaces report maintenance state:

**`gc maintenance status [--json]`** — loop state and recent runs:

```text
Maintenance: enabled=yes interval=168h0m0s
Last run: stage=done at 2026-04-22T10:00:00Z (12.4s)
Next scheduled: 2026-04-29T10:00:00Z
History (3):
  2026-04-22T10:00:00Z  stage=done       duration=12.4s
  2026-04-15T10:00:00Z  stage=done       duration=11.8s
  2026-04-08T10:00:00Z  stage=gc         duration=600.0s  err=context deadline exceeded
```

The status command routes through the supervisor API; if the
controller is down the command exits **2** with
`gc maintenance status: supervisor not running (...)`.

**`gc status` / `gc citystatus`** — appends a `Store health:` block:

```text
Store health:
  Path:        /path/to/city/.beads/dolt
  Size:        420.0 MB
  Live rows:   221
  Ratio:       1.9 MB/row  (threshold 1.0 MB/row)  ⚠ maintenance overdue
  Last GC:     2026-04-22T10:00:00Z (success)
```

The `⚠ maintenance overdue` suffix appears when
`size_bytes > 1.0 MB × live_rows`. The same data is available under
`store_health` in `gc status --json`.

**Events.** Every cycle emits exactly one event:

- `gc.store.maintenance.done` — payload includes `duration_s`,
  `before_bytes`, `after_bytes`, `snapshot_path`.
- `gc.store.maintenance.failed` — payload includes `stage`,
  `error_msg`, `snapshot_path` (empty when the failure happened
  before any bytes were written).

Tail with `gc events --type gc.store.maintenance.done` (or `.failed`).

## Manual trigger

Run a maintenance cycle on demand — e.g. after a bulk import, or to
verify a config change before waiting a week.

```sh
# Fire and forget — returns as soon as the supervisor accepts the request.
gc maintenance dolt-gc

# Synchronous — block until the run completes; exits 1 on failure.
gc maintenance dolt-gc --wait

# Machine-readable.
gc maintenance dolt-gc --wait --json
```

Exit codes:

| Code | Meaning                                                  |
|------|----------------------------------------------------------|
| 0    | Accepted (async) or completed successfully (`--wait`).   |
| 1    | `--wait` and the run failed (stage ≠ `done`).            |
| 2    | Supervisor unreachable, or `[maintenance.dolt]` disabled. |
| 3    | A run is already in progress (manual or scheduled).      |

A `--wait` run holds the lease for the duration of the cycle (up to
`gc_timeout` for the gc stage); other manual triggers in that window
get `3 / 409 Conflict` with the in-flight start time so you can tell
whether the existing run is fresh or stuck.

## Snapshot layout

Snapshots live next to the bead store at
`<city>/.beads/dolt-backups/`:

```
.beads/dolt-backups/
  current/                 # in-flight snapshot (rotated at end of cycle)
  success/
    2026-04-08T10-00-00Z/  # successful snapshots, newest 3 retained
    2026-04-15T10-00-00Z/
    2026-04-22T10-00-00Z/
  failed/
    2026-04-01T10-00-00Z/  # most recent failed snapshot retained
```

Timestamps are RFC3339 UTC with colons replaced by hyphens for
filesystem portability. Each subdirectory is a complete `dolt backup`
target — usable directly with `dolt clone file://...`.

## Emergency rollback

Use this if `dolt gc` corrupts the store (a future Dolt regression
slipping past the smoke test). The most recent successful snapshot
is the recovery point; failed snapshots are retained for postmortem,
not for restore.

1. **Stop the controller:**

   ```sh
   gc stop
   ```

2. **Move the broken store aside (do not delete — keep it for
   postmortem):**

   ```sh
   cd <city>/.beads
   mv dolt dolt.broken-$(date -u +%Y%m%dT%H%M%SZ)
   ```

3. **Pick the most recent successful snapshot** under
   `.beads/dolt-backups/success/` (sorted lexicographically — the
   timestamp format makes lexical order equal to time order).

4. **Restore it as the new bead store** with `dolt clone`:

   ```sh
   cd <city>/.beads
   dolt clone file://$(pwd)/dolt-backups/success/<timestamp> dolt
   ```

5. **Restart the controller and verify:**

   ```sh
   gc start
   gc maintenance status
   bd list --json --limit 1
   ```

If the most recent snapshot is also unreadable, repeat with the
next-newest entry under `success/`. The retention policy keeps the
last 3, which covers ≥ 2 weeks at the default cadence.

## Disabling

To stop the loop without uninstalling:

1. Edit `city.toml`:

   ```toml
   [maintenance.dolt]
   enabled = false
   ```

2. Restart the controller:

   ```sh
   gc stop && gc start
   ```

Existing snapshots under `.beads/dolt-backups/` are not removed —
prune them by hand if you want the disk back. Disabling does **not**
purge event history; prior `gc.store.maintenance.{done,failed}`
events remain visible via `gc events`.

## References

- Implementation:
  - `internal/supervisor/maintenance.go` (loop + lease)
  - `internal/supervisor/maintenance_snapshot.go` (backup + retention)
  - `cmd/gc/cmd_maintenance.go` (CLI)
  - `cmd/gc/store_health.go` (status block)
- Dolt: `dolt gc`, `dolt backup`, `dolt clone`
