---
title: bd Auto-Backup Cleanup
description: Reclaim space when bd's `.beads/backup/` directory grows large enough to threaten disk pressure.
---

## Overview

`bd` (the beads CLI) maintains a Dolt-native backup of every workspace it
touches. The backup writes to `<root>/.beads/backup/` on a hardcoded
remote named `backup_export`, throttled to one sync per fifteen minutes
per bd process (`gastownhall/beads`: `cmd/bd/backup_auto.go`).

The backup remote is **append-only**. There is no retention, rotation,
or garbage-collection logic in the bd auto-backup path today; the
architectural redesign for snapshot collections is tracked at
[gastownhall/beads#2993](https://github.com/gastownhall/beads/issues/2993).
Over weeks of normal use the directory grows monotonically — and in busy
multi-agent cities (where every controller, agent, and order touches
bd), the growth is fast enough to fill the disk in days.

`gc doctor` emits a `bd-backup-size` warning at 5 GB and an error at
15 GB to catch this before the next cascade. The canary checks the city
workspace and managed rig scope roots, reporting the largest individual
`.beads/backup/` directory and the aggregate backup footprint.

## Why it matters — the disk-full cascade

`bd auto-backup`'s unbounded growth was the proximate root cause of a
multi-hour outage on 2026-05-20/21 (qlandia incident, `qlandia/gc-p831i`):

1. `.beads/backup/` hit 34 GB and filled the disk.
2. Dolt's mmap writes started returning `ENOSPC`.
3. Dolt entered a 95% CPU retry loop that **did not** recover when disk
   was freed.
4. Gas City's supervisor reconciler wedged on Dolt writes; named agents
   stuck `stopped`; recovery flags stuck.
5. Recovery required SIGTERM-to-dolt, supervisor restart, plist
   regeneration, and session-kill of every named-always agent — two days
   of disruption.

The gas-city-side amplifier (auto-recover-on-ENOSPC) was fixed in
[gastownhall/gascity#2347](https://github.com/gastownhall/gascity/pull/2347).
This doc covers the still-live bd-side root cause.

## Inspect

When `gc doctor` flags `bd-backup-size`, first inspect the directory:

```bash
du -sh ~/qlandia/.beads/backup       # total size
ls -lh ~/qlandia/.beads/backup       # file mix (.darc vs nbs_table_*)
cat ~/qlandia/.beads/backup/backup_state.json  # last sync watermark
```

You're looking for:

- A current `last_dolt_commit` in `backup_state.json` (within minutes of
  now). If the backup is current, you can rotate it safely.
- A mix of `.darc` archive files (one per conjoined chunk) and
  `nbs_table_*` files (raw uncompacted writes). Many `nbs_table_*` files
  indicate that conjoin has been failing — see "Atomicity / sync
  failure" below.

## Cleanup options

### Option A — disable bd auto-backup (least invasive)

If the only consumer of `.beads/backup/` was the implicit auto-backup,
turn it off and let `mol-dog-backup` (which writes to `.dolt-backup/`)
handle backups instead:

```bash
bd config set backup.enabled false
```

After this, you can `rm -rf .beads/backup/`. Verify next `gc doctor` run
that `bd-backup-size` reports `no bd backup directory present`.

You retain coverage from `mol-dog-backup` writing to `.dolt-backup/`,
which is run by the dolt pack on a 6h cooldown and is operator-managed
through gas-city.

### Option B — rotate the backup (preserve coverage)

Use this when you want to keep auto-backup running but reclaim the
accumulated bloat.

> **Risk**: bd auto-backup is non-atomic
> ([gastownhall/beads#4070](https://github.com/gastownhall/beads/issues/4070)).
> Mid-sync interruption can leave a broken backup. Schedule rotation at
> a quiet moment and verify the result.

```bash
# 1. Stop the supervisor so no agent is writing beads.
gc stop

# 2. Capture the existing backup as a safety net.
mv ~/qlandia/.beads/backup ~/qlandia/.beads/backup.old-$(date +%Y%m%d)

# 3. Trigger bd's auto-backup pipeline while the supervisor remains
#    stopped. Do not use `bd backup sync` here; that syncs the separate
#    explicit "default" backup remote, not the auto-backup `backup_export`
#    remote documented above.
bd -C ~/qlandia list --status open --limit 1 --json >/tmp/qlandia-bd-autobackup-smoke.json

# 4. Verify the new backup is sane.
du -sh ~/qlandia/.beads/backup        # should be ≲ source database size
cat ~/qlandia/.beads/backup/backup_state.json  # current timestamp

# 5. Restart the supervisor only after the first backup is known good.
gc start

# 6. After confirming a clean run for ~1 day, delete the safety net.
rm -rf ~/qlandia/.beads/backup.old-*
```

### Option C — operator wipe (when the database has shrunk hard)

After running `dolt gc --full` on the source database (see
[dolt-bloat-recovery.md](/troubleshooting/dolt-bloat-recovery)), the source's noms
directory shrinks but the backup remote does not. The full backup
chain — every chunk ever written — is still on disk in
`.beads/backup/`.

If you've just run a source GC and have a verified safety copy of the
data, Option B's wipe-and-resync is the only mechanism that propagates
the GC into the backup.

## Atomicity / sync failure

If you see every bd command print

```
Warning: auto-backup failed: sync to backup: sync backup backup_export:
Error 1105: error opening table file: table file not found:
/path/to/.beads/backup/<chunk-hash>
```

you've hit [gastownhall/beads#4070](https://github.com/gastownhall/beads/issues/4070):
the backup manifest references chunks that were never delivered. Manual
repair until upstream lands a fix:

```bash
# Identify missing chunks
python3 -c "
import os, sys
manifest = open(os.path.expanduser('~/qlandia/.beads/backup/manifest')).read()
parts = manifest.strip().split(':')
chunks = [parts[i] for i in range(5, len(parts), 2)]
existing = set()
for f in os.listdir(os.path.expanduser('~/qlandia/.beads/backup/')):
    if f.endswith('.darc'):
        existing.add(f[:-len('.darc')])
missing = [c for c in chunks if c not in existing]
for c in missing:
    print(c)
"
# If nbs_table_* files are present, treat the output as a candidate
# list and cross-check with bd backup status or Dolt tooling before
# copying. Raw table files can contain valid chunks that are not visible
# as standalone .darc files.
# Then copy each missing chunk from the live noms store:
#   cp ~/qlandia/.beads/dolt/<db>/.dolt/noms/<hash> ~/qlandia/.beads/backup/
```

## Prevention

- **Track [gastownhall/beads#2993](https://github.com/gastownhall/beads/issues/2993).**
  The architectural fix is snapshot-collection semantics with explicit
  retention. Until it lands, every bd-driven city accumulates this
  bloat.
- **Run `gc doctor` regularly.** The `bd-backup-size` canary fires at
  5 GB warn, 15 GB error — well before disk pressure breaks Dolt.
- **Prefer `mol-dog-backup`'s `.dolt-backup/` path** for primary backup
  coverage. It's operator-managed through gas-city, runs on a 6h
  cooldown rather than per-command, and is the one targeted by
  retention work in `gastownhall/gascity`.
- **Watch `gc doctor` for `dolt-noms-size` alongside this check.** Both
  surfaces grow together on busy cities; treating them as a single
  disk-pressure signal works better than reasoning about each in
  isolation.
