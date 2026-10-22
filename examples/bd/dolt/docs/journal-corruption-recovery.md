# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

## What `gc start` already did

Before this runbook is needed, `gc start` probes every database in the data dir offline
and restores each confirmed-corrupt one from its local backup remote — bd's `default`
destination (what mol-dog-backup registers and syncs) or the legacy `<dbname>-backup`
name:

- `gc dolt logs` shows `RESTORED '<dbname>' from backup …` or
  `NOT auto-recovering '<dbname>'` for each database it touched. The same lines are in the
  managed log, because a successful start discards the provider's stderr.
- A failed auto-recovery is part of the `gc start` error, with the per-database manual
  steps to run.
- The pre-corruption store is preserved under
  `<city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>/` — never deleted.
  `gc doctor` (`dolt-corrupt-aside` check) keeps reporting it until you remove the copy
  after confirming the live database.
- A restored copy is as of the **last backup sync** — up to one 6h backup interval of
  writes is missing. The JSONL archive (Step 4b; 15m cooldown) may hold newer issue rows.

**The data dir is shared.** `<data_dir>` holds every database in the city, not just the
beads stores. The backup and JSONL-export orders cover only scopes bound to a rig, so an
unbound database may have no recovery source at all. Every instruction below touches one
database: never move, delete or re-create `<data_dir>` itself, and never rebuild it from
a single database's export — that is how live databases vanish (#1979).

---

## When to Use

Use this runbook when:
- `gc start` fails with a journal/noms error and `gc dolt logs` shows
  `NOT auto-recovering '<dbname>'` (auto-recovery found no usable backup)
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
gc dolt logs 2>&1 | tail -20
ls -lh "$(gc config get dolt.data_dir 2>/dev/null || echo .dolt-data)"

# 2. Locate the corrupt journal files
ls -lh <data_dir>/<dbname>/.dolt/noms/*.journal

# 3. Stop everything (gc stop also stops the dolt pack's server)
gc stop

# 4a. Restore one database from its backup (preferred)
# WARNING: all changes after the last backup sync are lost
jq .backups <data_dir>/<dbname>/.dolt/repo_state.json
ls <city>/.dolt-backup/<dbname>
mv <data_dir>/<dbname> <city>/.beads/<dbname>.broken-<timestamp>
cd <data_dir>
dolt backup restore <backup-url> <dbname>

# 4b. Reconstruct one bead scope from the JSONL archive (fallback)
# WARNING: reconstructs from the passive 15-minute export; open/in-progress
#          bead state may be partially stale compared to the last committed Dolt state
mv <data_dir>/<dbname> <city>/.beads/<dbname>.broken-<timestamp>
gc bd init --recreate-missing
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

Also capture the affected scope's `.beads/issues.jsonl` as a fallback snapshot before any
mutation:

```bash
cp .beads/issues.jsonl /tmp/issues-snapshot-$(date +%Y%m%d-%H%M%S).jsonl
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

## Step 4: Restore One Database From Backup

> **[WARNING]** Restoring replaces that database with the backup snapshot — as of the
> last backup sync (mol-dog-backup's interval is 6h). All writes after it are lost. This
> is a destructive, **per-database** operation: only `<data_dir>/<dbname>` is touched,
> and every sibling database stays in place.

```bash
# Identify the backup remotes registered for this database (bd names its
# destination "default"; mol-dog-backup syncs it on its interval)
jq .backups <data_dir>/<dbname>/.dolt/repo_state.json

# Confirm the artifact dir mol-dog-backup syncs for this scope
ls <city>/.dolt-backup/<dbname>

# Move only this database's corrupt store aside, outside the data dir
# (never delete it — it is the postmortem evidence)
mv <data_dir>/<dbname> <city>/.beads/<dbname>.broken-<timestamp>

# Restore from the backup URL, re-creating the database inside the data dir
cd <data_dir>
dolt backup restore <backup-url> <dbname>
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

## Step 4 (fallback): Reconstruct One Scope From JSONL Export

> **[WARNING]** The JSONL archive snapshot is **passive**: the core `jsonl-export` order
> writes it every 15 minutes. Any bead changes after the last export are not reflected, and
> wisps are not archived. In-progress beads may require manual status correction after
> import. A **non-bead** database has no JSONL archive at all — restore it from its own
> backup instead.

```bash
# Move only this scope's corrupt store aside, outside the data dir
# (never delete it — it is the postmortem evidence)
mv <data_dir>/<dbname> <city>/.beads/<dbname>.broken-<timestamp>

# Re-create only this scope's database: run from the scope root (the city,
# or add --rig <rig>); --recreate-missing authorizes re-creating a missing
# server-mode database
gc bd init --recreate-missing

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
- [ ] Backup remote is registered (`jq .backups <data_dir>/<dbname>/.dolt/repo_state.json`)
      and `<city>/.dolt-backup/<dbname>` holds a recent artifact
- [ ] Preserved corrupt store (the `.broken-<timestamp>` copy, and anything under
      `.gc/runtime/packs/dolt/corrupt-aside/`) reviewed and removed once the live
      database is confirmed
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence