# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

---

## When to Use

Use this runbook when:
- `gc start` fails and `dolt` or `gc dolt start` reports a journal or noms error on startup
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
ls -lh "$(gc config get dolt.data_dir 2>/dev/null || echo .dolt-data)"

# 2. Locate the corrupt journal files
ls -lh /path/to/city/.beads/dolt/<dbname>/.dolt/noms/*.journal

# 3. Stop everything
gc stop --all 2>/dev/null || true
gc dolt stop 2>/dev/null || true

# 4a. Restore from backup (preferred)
# WARNING: all changes after the backup snapshot are lost
gc dolt backup restore <dbname>-backup <dbname>

# 4b. Reconstruct from JSONL export (fallback)
# WARNING: reconstructs from the passive export; open/in-progress bead state
#          may be partially stale compared to the last committed Dolt state
gc bd import --from-file /path/to/city/.beads/issues.jsonl --db <dbname>

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
find "$(gc config get dolt.data_dir 2>/dev/null || echo .beads/dolt)" \
  -name "*.journal" -exec du -sh {} \; 2>/dev/null | sort -rh

# The largest .journal file is the likely culprit
# Note the <dbname> (directory immediately under data_dir)
```

Also capture the current `.beads/issues.jsonl` as a fallback snapshot before any mutation:

```bash
cp .beads/issues.jsonl /tmp/issues-snapshot-$(date +%Y%m%d-%H%M%S).jsonl
```

---

## Step 3: Stop All Running Processes

Ensure no processes hold the Dolt data directory before recovery:

```bash
gc stop --all 2>/dev/null || true
gc dolt stop 2>/dev/null || true

# Wait for the port to clear
sleep 2
lsof -i :"${GC_DOLT_PORT:-28231}" 2>/dev/null || true
```

---

## Step 4: Restore From Backup

> **[WARNING]** Restoring from backup permanently replaces the current database with the
> backup snapshot. All writes after the backup timestamp are lost. This is a destructive
> operation. Confirm the backup timestamp with `gc dolt backup status <dbname>-backup`
> before proceeding.

```bash
# Identify available backup remotes
gc dolt backup list <dbname>

# Restore the database from the <dbname>-backup remote
gc dolt backup restore <dbname>-backup <dbname>

# If the restore command is unavailable, drop and re-clone manually:
# rm -rf /path/to/city/.beads/dolt/<dbname>
# dolt clone <backup-url> /path/to/city/.beads/dolt/<dbname>
```

If no backup is configured or the backup itself is corrupt, proceed to Step 4b.

---

## Step 4 (fallback): Reconstruct From JSONL Export

> **[WARNING]** The `.beads/issues.jsonl` export is a **passive** snapshot updated on each
> Dolt commit. It may lag the last committed state by one reconciler tick. Any bead changes
> between the last Dolt commit and the corruption event are not reflected. In-progress beads
> may require manual status correction after import.

```bash
# Drop the corrupt database directory
rm -rf /path/to/city/.beads/dolt/<dbname>

# Re-initialize an empty store
gc bd init --db <dbname>

# Import from the JSONL export
gc bd import --from-file .beads/issues.jsonl --db <dbname>

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
- [ ] Backup remote is configured and `gc dolt backup status` shows a recent artifact
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
