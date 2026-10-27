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
- A restored database gets back the backups its corrupt store had registered, because
  `dolt backup restore` leaves the copy with none: `gc dolt logs` shows
  `re-registered backup …`, or the command to run when that failed.
- A failed auto-recovery (no usable backup, or a restore that failed) is part of the
  `gc start` error, with the database's registered backups and the per-database manual
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
  `NOT auto-recovering '<dbname>'` (no usable backup) or
  `backup restore failed for '<dbname>'`
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
```

Then recover `<dbname>` with **one** of 4a or 4b, never both. 4b is only for a bead scope
whose database has no usable backup.

```bash
# 4a. Restore one database from its backup (preferred)
# WARNING: all changes after the last backup sync are lost
jq .backups <data_dir>/<dbname>/.dolt/repo_state.json
ls <city>/.dolt-backup/<dbname>
mkdir -p <city>/.gc/runtime/packs/dolt/corrupt-aside
mv <data_dir>/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>
cd <data_dir>
dolt backup restore <backup-url> <dbname>
# register again each backup jq listed (a restored copy has none)
cd <data_dir>/<dbname>
dolt backup add <name> <url>
```

```bash
# 4b. Rebuild one bead scope from the JSONL archive (no usable backup)
# WARNING: reconstructs from the passive 15-minute export; open/in-progress
#          bead state may be partially stale compared to the last committed Dolt state
mkdir -p <city>/.gc/runtime/packs/dolt/corrupt-aside
mv <data_dir>/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>
cd <city>
# for a rig scope, add --rig <rig> to each gc bd command below
gc bd init --reinit-local -p <prefix> --skip-hooks --skip-agents
# the default archive path; Step 4 (fallback) lists the others
gc bd import <city>/.gc/runtime/packs/core/jsonl-archive/<sql_name>/issues.jsonl
gc bd backup status --json | jq -r '.dolt.backup_url // empty'
# if that printed a URL, register it again (a re-created database has none)
gc bd backup init <backup_url>
```

```bash
# 5. Start the city and verify
gc start
gc dolt status
gc doctor
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
mkdir -p <city>/.gc/runtime/packs/dolt/corrupt-aside
mv <data_dir>/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>

# Restore from the backup URL, re-creating the database inside the data dir
cd <data_dir>
dolt backup restore <backup-url> <dbname>

# Register the backups again, once per name jq listed above. A restored copy
# has none, while bd still reports its destination as configured, so the
# backup order would never register it and every later sync would fail.
cd <data_dir>/<dbname>
dolt backup add <name> <url>
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

## Step 4 (fallback): Rebuild One Scope From the JSONL Archive

> **[WARNING]** The JSONL archive snapshot is **passive**: the core `jsonl-export` order
> writes it every 15 minutes. Any bead changes after the last export are not reflected, and
> wisps are not archived. In-progress beads may require manual status correction after
> import. A **non-bead** database has no JSONL archive at all — restore it from its own
> backup instead.

This step rebuilds the database of one bead scope (the city, or one rig) on the city's
managed Dolt server: a scope whose `.beads/metadata.json` has `"dolt_mode": "server"`.
Run it after Step 3, from the city directory, and only for a database with no usable
backup. For a rig scope, add `--rig <rig>` to every `gc bd` command below.

```bash
# Move only this scope's corrupt store aside, outside the data dir
# (never delete it — it is the postmortem evidence)
mkdir -p <city>/.gc/runtime/packs/dolt/corrupt-aside
mv <data_dir>/<dbname> <city>/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>

# Re-create only this scope's database, empty, under the scope's own identity
cd <city>
gc bd init --reinit-local -p <prefix> --skip-hooks --skip-agents

# Import the scope's last JSONL export (bd export format)
gc bd import <archive>/<sql_name>/issues.jsonl

# A re-created database has no backup registered while bd still reports its
# destination: register again the URL this prints (nothing when the scope
# never had one)
gc bd backup status --json | jq -r '.dolt.backup_url // empty'
gc bd backup init <backup_url>

# Inspect open/in-progress beads and correct state if needed
gc bd list --status in_progress --json | jq '.[] | {id, title, status}'
```

- `--reinit-local` is how the pinned bd release (v1.3.1) re-creates the database of a scope
  that is already initialized; a plain `gc bd init` refuses. It acts on this scope's
  database only (`dolt_database` in `.beads/metadata.json`) and keeps the project id that
  file records, which `gc start` checks against `.beads/identity.toml`. Run it only after
  the move above: against a database that still exists it is bd's destructive reinit.
  Newer bd (after v1.3.1) refuses `--reinit-local` for a missing database; pass
  `--recreate-missing` in its place there.
- `<prefix>` is the scope's issue prefix as `gc rig list --json` shows it (the city scope
  is the entry with `"hq": true`). Without `-p`, bd names the prefix after the directory;
  without `--skip-hooks --skip-agents`, it writes hooks and agent instruction files into
  the scope.
- Do not leave the re-creation to `gc start`: the database it creates gets a new project
  id, and the start then fails with `PROJECT IDENTITY MISMATCH`.
- `<archive>` is the core `jsonl-export` order's archive,
  `<city>/.gc/runtime/packs/core/jsonl-archive`. A city migrated from the gastown
  maintenance pack keeps `<city>/.gc/runtime/packs/maintenance/jsonl-archive` (or
  `<city>/.gc/jsonl-archive` from before packs), and `GC_JSONL_ARCHIVE_REPO` overrides
  them all. `<sql_name>` is the database name Dolt serves: `<dbname>` with each `-`
  replaced by `_`. A failed `gc start` names the exact file in its manual steps.

---

## Step 5: Verify

Confirm the database starts cleanly and the city is healthy before resuming work:

```bash
# Start the city; gc start brings up the managed Dolt server first
gc start

# Confirm the managed server is running
gc dolt status

# Run the full doctor scan
gc doctor
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
- [ ] The recovered database has its backups registered again (`jq .backups
      <data_dir>/<dbname>/.dolt/repo_state.json` lists what the corrupt store listed) and
      `<city>/.dolt-backup/<dbname>` holds a recent artifact
- [ ] Preserved corrupt store under `.gc/runtime/packs/dolt/corrupt-aside/` (reported
      by the `dolt-corrupt-aside` doctor check) reviewed and removed once the live
      database is confirmed
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
