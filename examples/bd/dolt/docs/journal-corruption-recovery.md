# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

**Automatic recovery comes first.** For gc-managed databases, `gc start` already
recovers from journal corruption on its own: when the server fails with a journal/noms
error, it probes each database offline, moves every confirmed-corrupt store to
`/path/to/city/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>` (never deleted),
and restores it with `dolt backup restore` from the database's registered local backup —
bd's `default` backup, which the `mol-dog-backup` order registers with `bd backup init`
under `<city>/.dolt-backup/<dbname>` (the legacy `<dbname>-backup` entry only when
`default` is not registered). Every step is logged by `gc start`. If the selected backup is
not a non-empty local `file://` directory, or the restore fails, it refuses: the store is
left in place, `gc start` fails with the original error, and the log lists the backups it
checked and the exact manual `dolt backup restore` command. This runbook is for that case.

In the steps below, `<data_dir>` is the managed Dolt data directory:
`/path/to/city/.beads/dolt` unless `GC_DOLT_DATA_DIR` overrides it.

---

## When to Use

Use this runbook when:
- `gc start` fails with a journal or noms error and its log says
  `NOT auto-recovering '<dbname>'` (no usable backup) or `backup restore failed`
- `gc doctor` reports `dolt-journal-size` at error level AND the server will not start
- You need to recover a managed Dolt database that fails to open

Do **not** use this runbook for:
- A running-but-slow server (use `gc dolt compact` instead)
- Backup staleness warnings without a startup failure (monitor and schedule compaction)
- Non-managed Dolt stores (external endpoint configured; recovery is out of scope here)

---

## Quick Reference

```bash
# 1. Confirm server is down and identify the corrupt database
gc dolt status 2>&1 || true
ls -lh <data_dir>

# 2. Locate the corrupt journal files
ls -lh <data_dir>/<dbname>/.dolt/noms/*.journal

# 3. Stop everything
gc stop 2>/dev/null || true

# 4a. Restore from the registered backup (preferred; works with the server down)
# WARNING: all changes after the backup snapshot are lost
url=$(jq -r '.backups.default.url // .backups["<dbname>-backup"].url // empty' <data_dir>/<dbname>/.dolt/repo_state.json)
mv <data_dir>/<dbname> <somewhere-safe>
(cd <data_dir> && dolt backup restore "$url" <dbname>)

# 4b. Reconstruct from the core JSONL archive (lossy last resort)
# WARNING: JSONL recovery loses all non-issue history (events, wisps, comment and
#          audit history); issue rows are as of the last 15-minute export
gc bd import /path/to/city/.gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

# 5. Verify
dolt --host 127.0.0.1 --port "$GC_DOLT_PORT" --user root sql -q "SELECT active_branch()"
gc doctor
gc start
```

---

## Step 1: Confirm the Failure

Identify that the Dolt server is down due to journal corruption, not a port conflict or
config error:

```bash
# Attempt to start Dolt and capture the error
gc dolt start 2>&1 | tee /tmp/dolt-start-error.txt

# Look for noms/journal errors
grep -i "journal\|noms\|corrupt\|manifest" /tmp/dolt-start-error.txt
```

If the error is unrelated to journals (e.g. port in use, missing config), stop here and
diagnose normally. This runbook applies only to journal/noms startup failures.

---

## Step 2: Locate the Corrupt Store

Find which database directory contains the oversized or corrupt journal:

```bash
# List journal files across all managed databases
find <data_dir> -name "*.journal" -exec du -sh {} \; 2>/dev/null | sort -rh

# The largest .journal file is the likely culprit
# Note the <dbname> (directory immediately under data_dir)
```

If automatic recovery already ran, the `gc start` log names the database (`journal
corruption confirmed in database '<dbname>'`).

---

## Step 3: Stop All Running Processes

Ensure no processes hold the Dolt data directory before recovery:

```bash
gc stop 2>/dev/null || true

# Wait for the port to clear
sleep 2
lsof -i :"${GC_DOLT_PORT:-28231}" 2>/dev/null || true
```

---

## Step 4: Restore From Backup

> **[WARNING]** Restoring from backup permanently replaces the current database with the
> backup snapshot. All writes after the backup timestamp are lost. Confirm the backup's
> freshness with `gc bd backup status --json` (`.dolt.backup_url`, `.dolt.last_sync`)
> before proceeding.

**gc-managed databases.** Restore from the backup registered in the database's
`repo_state.json`, which stays readable when the noms store does not. Prefer bd's
`default` backup; use the legacy `<dbname>-backup` entry only when `default` is not
registered. The restore runs offline, with the server down:

```bash
# The registered backup URL (bd's default backup, else the legacy entry)
url=$(jq -r '.backups.default.url // .backups["<dbname>-backup"].url // empty' <data_dir>/<dbname>/.dolt/repo_state.json)
echo "$url"   # expect file:///path/to/city/.dolt-backup/<dbname>

# Keep the corrupt store; never delete it
mv <data_dir>/<dbname> <somewhere-safe>

# Restore the database from the backup
(cd <data_dir> && dolt backup restore "$url" <dbname>)
```

If the restore fails, move the corrupt store back before trying anything else.

**bd-owned proxied scopes** (the default for new cities; `dolt_mode` is
`proxied-server` in the scope's `.beads/metadata.json`) are backed up by the
`mol-dog-backup` order with `bd backup` into `<city>/.dolt-backup/<dbname>`, and
restored with bd itself. `bd backup restore` refuses while any other bd client
is attached to the scope, so stop the city first (Step 3), and it leaves the
scope's store stopped:

```bash
gc stop
gc bd backup status --json              # the city scope; add --rig <rig> for a rig
gc bd backup restore /path/to/city/.dolt-backup/<dbname> --force
gc start
```

No maintenance order ever runs a restore.

If no backup is registered or the backup itself is corrupt, proceed to the JSONL fallback.

---

## Step 4 (fallback): Reconstruct From the JSONL Archive

> **[WARNING]** This is a lossy last resort. JSONL recovery loses all non-issue history
> (events, wisps, comment and audit history). The JSONL archive snapshot is **passive**: the
> core `jsonl-export` order writes it every 15 minutes, so bead changes after the last
> export are not reflected. In-progress beads may require manual status correction after
> import. Use the core jsonl-archive below: gc turns bd's own JSONL auto-export off, so the
> scope's export file is not a current recovery source.

When neither a usable backup nor a jsonl-archive snapshot for `<dbname>` exists, stop and
escalate. Never `bd init` an empty store in place of the corrupt one.

```bash
# Confirm the archive snapshot exists before touching the store
ls -l /path/to/city/.gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

# Keep the corrupt store; never delete it
mv <data_dir>/<dbname> <somewhere-safe>

# Re-initialize the store that the import fills
gc bd init --db <dbname>

# Import the latest JSONL archive snapshot of this database (written by the
# core jsonl-export order in bd export format)
gc bd import /path/to/city/.gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

# Inspect open/in-progress beads and correct state if needed
gc bd list --status in_progress --json | jq '.[] | {id, title, status}'
```

---

## Step 5: Verify

Confirm the database starts cleanly and the city is healthy before resuming work:

```bash
# Offline Dolt check: start the server in isolation
gc dolt start

# Confirm the SQL interface responds
dolt --host "${GC_DOLT_HOST:-127.0.0.1}" --port "${GC_DOLT_PORT:-28231}" \
  --user "${GC_DOLT_USER:-root}" sql -q "SELECT active_branch(), @@GLOBAL.max_connections"

# Run the full doctor scan
gc doctor

# Start the city
gc start
```

If `gc doctor` still shows `dolt-journal-size` at warning or error level after recovery,
run compaction immediately before resuming agent work:

```bash
gc dolt compact
```

---

## Post-Recovery Checklist

- [ ] Dolt server starts and responds to SQL queries
- [ ] `gc doctor` passes with no errors (warnings on non-critical checks are acceptable)
- [ ] `gc start` brings all configured sessions online
- [ ] The database registers bd's `default` backup and `gc bd backup status --json` shows a
      recent `.dolt.last_sync`
- [ ] The corrupt store under `corrupt-aside/` (or `<somewhere-safe>`) is kept until the
      restored database is confirmed
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
