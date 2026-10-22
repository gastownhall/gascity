# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

The data dir (`.beads/dolt` below) is shared: it holds one store per database
of the city, and recovery is always per database. Move only the corrupt
`<data_dir>/<dbname>` aside, to a path outside the data dir, and leave every
other database where it is. Never move, delete, clone over or re-create the
data dir itself: the databases you do not name are lost with it.

---

## What `gc start` Already Does

On every `gc start`, gc checks each managed database for a corrupt journal.
For a corrupt `<dbname>` with a usable local backup (a non-empty `file://`
directory registered as bd's `default` backup or the legacy
`<dbname>-backup`), it:

1. moves only `<data_dir>/<dbname>` to
   `.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>`;
2. runs `dolt backup restore <backup-url> <dbname>` from the data dir;
3. logs `RESTORED '<dbname>' from backup <name> (<url>)` to `gc dolt logs`.

Writes made after the last backup sync are not in the restored copy. The
`dolt-corrupt-aside` doctor check reports the aside copy until you review and
remove it. With no usable backup, `gc start` fails closed, leaves `<dbname>`
in place, and prints the backups it checked, the commands to restore
`<dbname>` alone, the JSONL fallback path, and the databases to leave in place.
Follow those printed steps or the steps below.

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

# 4a. Restore this database from backup (preferred)
# WARNING: all changes after the backup snapshot are lost
mkdir -p /path/to/city/.gc/runtime/packs/dolt/corrupt-aside
mv /path/to/city/.beads/dolt/<dbname> /path/to/city/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>
(cd /path/to/city/.beads/dolt && dolt backup restore <backup-url> <dbname>)

# 4b. Reconstruct from the JSONL archive (fallback)
# WARNING: reconstructs from the passive 15-minute export; open/in-progress
#          bead state may be partially stale compared to the last committed Dolt state
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

# Wait for the port to clear
sleep 2
lsof -i :"${GC_DOLT_PORT:-28231}" 2>/dev/null || true
```

---

## Step 4: Restore From Backup

> **[WARNING]** Restoring from backup replaces this database with the backup snapshot.
> All writes after the backup timestamp are lost. Check the modification time of the
> backup directory before proceeding. The pre-corruption store is kept aside, not deleted.

```bash
# Identify the backups registered for this database: bd's "default" and the
# legacy "<dbname>-backup"; use a file:// URL whose directory is not empty
jq '.backups' /path/to/city/.beads/dolt/<dbname>/.dolt/repo_state.json

# Move only this database aside, outside the data dir
mkdir -p /path/to/city/.gc/runtime/packs/dolt/corrupt-aside
mv /path/to/city/.beads/dolt/<dbname> /path/to/city/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>

# Restore it from the data dir, next to the databases left in place
(cd /path/to/city/.beads/dolt && dolt backup restore <backup-url> <dbname>)
```

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

If no backup is configured or the backup itself is corrupt, proceed to Step 4b.

---

## Step 4 (fallback): Reconstruct From JSONL Export

> **[WARNING]** The JSONL archive snapshot is **passive**: the core `jsonl-export` order
> writes it every 15 minutes. Any bead changes after the last export are not reflected, and
> wisps are not archived. In-progress beads may require manual status correction after import.

```bash
# Move only this database aside, outside the data dir
mkdir -p /path/to/city/.gc/runtime/packs/dolt/corrupt-aside
mv /path/to/city/.beads/dolt/<dbname> /path/to/city/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>

# Re-initialize an empty store
gc bd init --db <dbname>

# Import the latest JSONL archive snapshot of this database (written by the
# core jsonl-export order in bd export format)
gc bd import .gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

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
- [ ] Backup is configured and `gc doctor` reports no `dolt-backup` or `dolt-unbound-databases` warning
- [ ] The `corrupt-aside` copy reported by `gc doctor` is reviewed, then removed
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
