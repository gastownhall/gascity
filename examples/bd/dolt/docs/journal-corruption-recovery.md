# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

## The automatic path comes first

On a gc-managed server, `gc start` runs journal-corruption recovery by itself: when dolt
reports a journal corruption error at startup, each affected database is probed offline, the
corrupt store is preserved under `<city>/.gc/runtime/packs/dolt/corrupt-aside/<db>.<timestamp>`
(never deleted), and the database is restored from its registered local backup — bd's
canonical `default` backup, or the legacy `<db>-backup` remote. The recovery log (and the
`RESTORED` line) names the backup and URL each database came from. When no usable local
backup is registered, recovery fails closed: the corrupt store is left in place, the server
does not start, and the refusal names the backups it checked. That is the moment this
runbook's manual steps apply.

---

## When to Use

Use this runbook when:
- `gc start` fails with a journal-corruption refusal (no usable local backup registered)
- `gc dolt status 2>&1` or the startup log reports a journal or noms error
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
gc stop

# 4a. Manual restore from the registered backup (preferred)
#     (gc start already tried this automatically and refused — see below)
url=$(jq -r '.backups.default.url // .backups["<dbname>-backup"].url // empty' \
  /path/to/city/.beads/dolt/<dbname>/.dolt/repo_state.json)
mv /path/to/city/.beads/dolt/<dbname> /somewhere-safe-not-under-the-data-dir
(cd /path/to/city/.beads/dolt && dolt backup restore "$url" <dbname>)

# 4b. Reconstruct from the core JSONL archive (lossy last resort — see warning)
gc bd import .gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

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
# Attempt to start the city and capture the error (gc start runs recovery itself)
gc start 2>&1 | tee /tmp/gc-start-error.txt

# Look for the recovery refusal or noms/journal errors
grep -i "journal\|noms\|corrupt\|NOT auto-recovering" /tmp/gc-start-error.txt
```

If the error is unrelated to journals (e.g. port in use, missing config), stop here and
diagnose normally. This runbook applies only to journal/noms startup failures. If recovery
ran automatically, its log lines name each corrupt database, the backup it restored from,
and the `corrupt-aside/` path holding the preserved original.

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

---

## Step 3: Stop All Running Processes

Ensure no processes hold the Dolt data directory before recovery:

```bash
gc stop

# Wait for the port to clear
sleep 2
lsof -i :"${GC_DOLT_PORT:-28231}" 2>/dev/null || true
```

---

## Step 4: Manual Restore From the Registered Backup

> **[WARNING]** Restoring from backup permanently replaces the current database with the
> backup snapshot. All writes after the backup timestamp are lost. This is a destructive
> operation. Confirm the backup's freshness first:

```bash
# Freshness of the registered backup (.dolt.backup_url, .dolt.last_sync)
gc bd backup status --json
```

The backup URL is registered in the database's own `repo_state.json`, which stays readable
even when the noms store does not. bd's `bd backup init` registers it under the name
`default`; older cities may still carry the legacy `<dbname>-backup` entry instead:

```bash
# Read the registered backup URL (default first, legacy name as fallback)
url=$(jq -r '.backups.default.url // .backups["<dbname>-backup"].url // empty' \
  /path/to/city/.beads/dolt/<dbname>/.dolt/repo_state.json)

# Move the corrupt store somewhere safe, outside the data dir
mv /path/to/city/.beads/dolt/<dbname> /somewhere-safe-not-under-the-data-dir

# Restore the database from the backup URL
(cd /path/to/city/.beads/dolt && dolt backup restore "$url" <dbname>)
```

`gc start` runs exactly this restore automatically when it can; doing it by hand is for
when the automatic path refused (backup not local, empty, or unreachable) and you have
fixed the cause — for example re-synced the backup directory.

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

If no usable backup exists, proceed to the JSONL fallback — as a last resort only.

---

## Step 4 (fallback): Reconstruct From the Core JSONL Archive

> **[WARNING] Lossy last resort.** JSONL recovery loses all non-issue history (events,
> wisps, comment and audit history). The core `jsonl-export` order writes the archive
> every 15 minutes, so recent issue changes may also be missing, and in-progress beads
> may require manual status correction after import. Prefer the backup restore above
> whenever a usable backup exists.

```bash
# Import the latest JSONL archive snapshot of this database (written by the
# core jsonl-export order in bd export format)
gc bd import .gc/runtime/packs/core/jsonl-archive/<dbname>/issues.jsonl

# Inspect open/in-progress beads and correct state if needed
gc bd list --status in_progress --json | jq '.[] | {id, title, status}'
```

If neither a usable backup nor a JSONL archive exists, **stop and escalate** — do not
initialize an empty store over the incident: an empty beads database silently loses the
audit trail of what the city was doing. Preserve the corrupt store directory and involve
a maintainer.

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
- [ ] Backup is registered and `gc bd backup status --json` shows a recent sync (`.dolt.last_sync`)
- [ ] Open in-progress beads reviewed and state corrected if the JSONL import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
