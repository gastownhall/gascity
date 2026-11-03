# Journal Corruption Recovery Runbook

> **Incident context:** ga-pqfk8t documents the production journal corruption event that
> motivated this runbook. Read it before executing these steps if time permits.

This runbook covers fail-closed journal corruption: the Dolt server refuses to start because
a write-ahead journal file is corrupt. The `gc doctor` check `dolt-journal-size` warns when
journals approach the compaction threshold (default 4 GB warn / 6 GB error). If the server
starts successfully, prefer `gc dolt compact` before a corruption event rather than this
recovery path.

**Automatic recovery comes first.** For gc-managed databases, starting the city already
recovers from journal corruption on its own: when the server fails with a journal/noms
error, it probes each database offline and restores every confirmed-corrupt one with
`dolt backup restore` from the database's registered local backup — bd's `default`
backup, which the `mol-dog-backup` order registers with `bd backup init` under
`<city>/.dolt-backup/<dbname>` (the legacy `<dbname>-backup` entry only when `default` is
not registered). The restore runs in `/path/to/city/.gc/runtime/packs/dolt/restore-staging/`,
and the backup is registered on the restored copy again, which `dolt backup restore` leaves
with no backup registered. The finished copy then moves to
`/path/to/city/.gc/runtime/packs/dolt/restore-ready/`, the corrupt store moves to
`/path/to/city/.gc/runtime/packs/dolt/corrupt-aside/<dbname>.<timestamp>` (never deleted),
and the copy moves into its place. Until then the corrupt store stays where it is. Each
move is a rename, so recovery refuses when `restore-staging/`, `restore-ready/` or
`corrupt-aside/` is on a different filesystem from the Dolt data directory.

The recovery runs when the city starts: `gc start`, or the supervisor starting the city
(for example after a reboot). Health patrol's restart of an unhealthy Dolt server does not
run it, so a corruption found while the city is running needs `gc stop`, then `gc start`.

gc gives the Dolt server's start 120 seconds in all, so an automatic restore may take 45
of them (`GC_DOLT_JOURNAL_RESTORE_TIMEOUT_SECONDS`). A restore that needs longer is stopped
and its partial copy removed, the corrupt store stays in place, and `gc start` fails with a
line that says the restore `did not finish within 45s`. Run `gc dolt start`, which has no
time limit and lets the restore run to completion, then `gc start`. A start killed in the
middle of a restore leaves its partial copy in `restore-staging/`, which the next start
removes. A start killed between moving the corrupt store aside and moving the copy in
leaves the database missing from the data directory. The next start moves the copy in from
`restore-ready/` before it starts the server, and when it cannot, it fails with a
`CRITICAL` line that names both copies.

A successful recovery does not stop the start, so look for it. Every step is recorded,
timestamped, as a `gc-beads-bd:` line in the managed Dolt log (`gc dolt logs`; by default
`/path/to/city/.gc/runtime/packs/dolt/dolt.log`):

```bash
gc dolt logs -n 2000 | grep 'gc-beads-bd:'
ls /path/to/city/.gc/runtime/packs/dolt/corrupt-aside/
```

A `RESTORED '<dbname>'` line means that database lost every write made since its last
backup sync; the line names the backup and the preserved store. `gc doctor` (the
`dolt-corrupt-aside` check) keeps reporting each store preserved under `corrupt-aside/`
until you remove it after confirming the live database.

If the selected backup is not a non-empty local `file://` directory, another database
registers the same directory, or one of recovery's directories is on a different filesystem,
recovery refuses: the store is left in place, `gc start`
fails with the original error, and the error and the log list the backups it checked, the
manual `dolt backup restore` command and the steps that recover that database alone. A
restore that fails leaves the store in place too, and prints every registered backup and
the same steps. This runbook is for those cases.

**The data dir is shared.** `<data_dir>` holds every database in the city, not just the
beads stores. The backup and JSONL-export orders cover only scopes bound to a rig, so an
unbound database may have no recovery source at all. Every instruction below touches one
database: never move, delete or re-create `<data_dir>` itself, and never rebuild it from
a single database's export — that is how live databases vanish (#1979).

In the steps below, `<data_dir>` is the managed Dolt data directory:
`/path/to/city/.beads/dolt` unless `GC_DOLT_DATA_DIR` overrides it. `<somewhere-safe>` is
a path outside it that does not exist yet, where the corrupt store is kept. Under
`/path/to/city/.gc/runtime/packs/dolt/corrupt-aside/` (create it with `mkdir -p` if it does
not exist yet), as `<dbname>.<timestamp>`, the store sits with the ones automatic recovery
preserves, and the `dolt-corrupt-aside` doctor check reports it until you remove it.

---

## When to Use

Use this runbook when:
- `gc start` fails with a journal or noms error and its log says
  `NOT auto-recovering '<dbname>'` (no usable backup) or `backup restore failed`
- `gc doctor` reports `dolt-journal-size` at error level AND the server will not start
- You need to recover a managed Dolt database that fails to open

Do **not** use this runbook for:
- An automatic restore that ran out of time (`did not finish within`): run `gc dolt start`,
  then `gc start`
- A running-but-slow server (use `gc dolt compact` instead)
- Backup staleness warnings without a startup failure (monitor and schedule compaction)
- Non-managed Dolt stores (external endpoint configured; recovery is out of scope here)

---

## Quick Reference

```bash
# 1. Confirm server is down and identify the corrupt database
gc dolt status 2>&1 || true
gc dolt logs 2>&1 | tail -20
ls -lh <data_dir>

# 2. Locate the corrupt journal files
ls -lh <data_dir>/<dbname>/.dolt/noms/*.journal

# 3. Stop everything (gc stop also stops the dolt pack's server)
gc stop 2>/dev/null || true
```

Then recover `<dbname>` with **one** of 4a or 4b, never both. 4b is only for a bead scope
whose database has no usable backup.

```bash
# 4a. Restore from the registered backup (preferred; works with the server down):
#     run the Step 4 block, which restores a usable registered backup in a
#     staging directory, registers it on the restored copy again, and only
#     then swaps the copy in for the corrupt store
# WARNING: all changes after the backup snapshot are lost
# SPLIT-STORE CITY: restore only a backup taken AFTER the split. One taken
# before it holds that time's infrastructure rows, which land in the work
# store, and the next boot refuses. With only an older backup, stop and
# escalate: 4b stops on a split city too.
```

```bash
# 4b. Rebuild one bead scope from the core JSONL archive (lossy last resort)
# WARNING: JSONL recovery loses all non-issue history (events, wisps, comment and
#          audit history); issue rows are as of the last 15-minute export
# SPLIT-STORE CITY: stop here, before the move, and escalate: see Step 4
#          (fallback) for why
# <archive> and <sql_name>: see Step 4 (fallback)
ls -l <archive>/<sql_name>/issues.jsonl   # stop and escalate if it is missing
mv <data_dir>/<dbname> <somewhere-safe>
cd /path/to/city
# for a rig scope, add --rig <rig> to each gc bd command below
gc bd init --reinit-local -p <prefix> --skip-hooks --skip-agents
gc bd import <archive>/<sql_name>/issues.jsonl
# if the import fails, stop before the backup lines and Step 5: move the
#          re-created <data_dir>/<dbname> aside too, move <somewhere-safe>
#          back to <data_dir>/<dbname>, and escalate
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

## Step 4: Restore One Database From Backup

> **[WARNING]** Restoring from backup permanently replaces the current database with the
> backup snapshot. All writes after the backup timestamp are lost. Confirm the backup's
> freshness with `gc bd backup status --json` (`.dolt.backup_url`, `.dolt.last_sync`)
> before proceeding. This is a destructive, **per-database** operation: only
> `<data_dir>/<dbname>` is touched, and every sibling database stays in place.

**gc-managed databases.** Restore from the backup registered in the database's
`repo_state.json`, which stays readable when the noms store does not. Prefer bd's
`default` backup; use the legacy `<dbname>-backup` entry only when `default` is not
registered. The legacy entry's URL is either an object's `url` field or a plain string.
The restore runs offline, with the server down. Like the automatic recovery, this block
restores only a non-empty local directory that no other database registers, since a
directory two databases sync to holds whichever synced to it last. It restores into a
staging directory next to `<data_dir>`, so the corrupt store stays in place until the
restore has succeeded:

```bash
# The registered backup: bd's default backup, else the legacy entry
state=<data_dir>/<dbname>/.dolt/repo_state.json
name=$(jq -r 'if (.backups // {} | has("default")) then "default" else "<dbname>-backup" end' "$state")
url=$(jq -r --arg name "$name" '.backups[$name] | (.url? // .) | strings' "$state")
echo "$name $url"   # expect: default file:///path/to/city/.dolt-backup/<dbname>

# Refuse anything but a non-empty local directory no other database registers
refusal=
if [ "${url#file://}" = "$url" ] || [ -z "$(ls -A "${url#file://}" 2>/dev/null)" ]; then
  refusal="it is not a non-empty local file:// directory"
else
  for dir in <data_dir>/*/; do
    dir=${dir%/}
    [ "${dir##*/}" != <dbname> ] && [ -f "$dir/.dolt/repo_state.json" ] || continue
    if jq -r '(.backups // {}) | objects | .[] | (.url? // .) | strings' "$dir/.dolt/repo_state.json" |
      grep -qxF -- "$url"; then
      refusal="database ${dir##*/} registers the same directory"
    fi
  done
fi

# Restore into staging and register the backup on the restored copy (dolt
# backup restore leaves it with none), then swap the copy in and keep the
# corrupt store (never delete it), moving the store back if the swap fails
if [ -n "$refusal" ]; then
  echo "not restoring <dbname> from backup '$name' ($url): $refusal; the store is untouched" >&2
elif [ -e <somewhere-safe> ]; then
  echo "<somewhere-safe> already exists: pick a new path to keep the corrupt store at; the store is untouched" >&2
elif ! staging=$(mktemp -d <data_dir>.restore.XXXXXX); then
  echo "could not create a staging directory next to <data_dir>; the store is untouched" >&2
elif ! (cd "$staging" && dolt backup restore "$url" <dbname>); then
  rm -rf "$staging"
  echo "the restore failed; the store is untouched" >&2
else
  (cd "$staging/<dbname>" && dolt backup add "$name" "$url") ||
    echo "WARNING: the restored <dbname> has no backup registered: with the server down, run \"dolt backup add $name '$url'\" in <data_dir>/<dbname>" >&2
  if ! mv <data_dir>/<dbname> <somewhere-safe>; then
    rm -rf "$staging"
    echo "could not move <data_dir>/<dbname> to <somewhere-safe>; the store is untouched" >&2
  elif mv "$staging/<dbname>" <data_dir>/<dbname>; then
    rmdir "$staging"
  elif [ ! -e <data_dir>/<dbname> ] && mv <somewhere-safe> <data_dir>/<dbname>; then
    rm -rf "$staging"
    echo "could not move the restored copy into <data_dir>; the store is back in place, untouched" >&2
  else
    echo "CRITICAL: the restored copy could not be moved into <data_dir> AND the corrupt store could not be moved back; original data is at <somewhere-safe>, the restored copy at $staging/<dbname>" >&2
  fi
fi
```

When the block refuses, the restore fails or the swap fails, the store is untouched. A
refusal because another database registers the same directory means that directory may
hold the other database's data: pick a backup that holds `<dbname>`, set `name` and `url`
to it, and run the block again from `refusal=`. When only `dolt backup add` fails, the
restored database is in place but is not backed up until you run the command the warning
prints. A `CRITICAL` line means `<data_dir>/<dbname>` is missing: move the restored copy
or the corrupt store it names to `<data_dir>/<dbname>` before you start the city.

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

On a **split-store city** (one that serves its infrastructure classes from a
separate binding), `gc bd backup restore` is refused, with no override. gc
cannot verify a backup's age against the split, and a backup taken before the
split would bring that time's infrastructure rows back into the work store all
at once. Restore the backup into a scratch single-store city instead (a
throwaway `gc init` city with no `[storage]` split, where `gc bd backup
restore` is forwarded unchanged), `gc bd export` it there, and import the
export into the split city with `gc bd import <file> --skip-infra` (Step 4b),
which keeps only the rows the work store holds. The next `gc start` checks the
work store for stranded rows.

No maintenance order ever runs a restore.

If no backup is registered or the backup itself is corrupt, proceed to the JSONL fallback.

---

## Step 4 (fallback): Rebuild One Scope From the JSONL Archive

> **[WARNING]** This is a lossy last resort. JSONL recovery loses all non-issue history
> (events, wisps, comment and audit history). The JSONL archive snapshot is **passive**: the
> core `jsonl-export` order writes it every 15 minutes, so bead changes after the last
> export are not reflected. In-progress beads may require manual status correction after
> import. Use the core jsonl-archive below: gc turns bd's own JSONL auto-export off, so the
> scope's export file is not a current recovery source. A **non-bead** database has no
> JSONL archive at all — restore it from its own backup instead.

When neither a usable backup nor a jsonl-archive snapshot for `<dbname>` exists, stop and
escalate. Never `bd init` an empty store in place of the corrupt one.

This step rebuilds the database of one bead scope (the city, or one rig) on the city's
managed Dolt server: a scope whose `.beads/metadata.json` has `"dolt_mode": "server"`.
Run it after Step 3, from the city directory, and only for a database with no usable
backup. For a rig scope, add `--rig <rig>` to every `gc bd` command below.

On a **split-store city**, stop here and escalate, before you move the store: `gc bd`
refuses `--reinit-local` there, and the one re-create flag it admits, `--recreate-missing`,
is not in the pinned bd. This runbook has no tested split-city rebuild for a bd that has it
(see "gc bd on split cities" in `docs/runbooks/split-storage-classes.md`).

```bash
# SPLIT-STORE CITY: stop here, before the move (see above)
# Confirm the archive snapshot exists before touching the store
ls -l <archive>/<sql_name>/issues.jsonl

# Keep the corrupt store; never delete it
mv <data_dir>/<dbname> <somewhere-safe>

# Re-create only this scope's database, empty, under the scope's own identity
cd /path/to/city
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

- `--reinit-local` is how the pinned bd release (v1.3.2) re-creates the database of a scope
  that is already initialized; a plain `gc bd init` refuses. It acts on this scope's
  database only (`dolt_database` in `.beads/metadata.json`) and keeps the project id that
  file records, which `gc start` checks against `.beads/identity.toml`. Run it only after
  the move above: against a database that still exists it is bd's destructive reinit.
  A bd whose `bd init --help` lists `--recreate-missing` refuses `--reinit-local` for a
  missing database; pass `--recreate-missing` in its place there. No release through the
  pinned one has that flag, but a build from newer beads source can, whatever version it
  reports, so check the help before the move.
- `<prefix>` is the scope's issue prefix as `gc rig list --json` shows it (the city scope
  is the entry with `"hq": true`). Without `-p`, bd names the prefix after the directory;
  without `--skip-hooks --skip-agents`, it writes hooks and agent instruction files into
  the scope.
- Do not leave the re-creation to `gc start`: the database it creates gets a new project
  id, and the start then fails with `PROJECT IDENTITY MISMATCH`.
- If the import fails, stop before you register the backup or start the city: the
  re-created database holds part of the archive or none of it, and a backup sync would
  replace the backup with it. Move it aside to another path outside `<data_dir>`, move
  `<somewhere-safe>` back to `<data_dir>/<dbname>`, and escalate.
- `<archive>` is the core `jsonl-export` order's archive,
  `/path/to/city/.gc/runtime/packs/core/jsonl-archive`. A city migrated from the gastown
  maintenance pack keeps `/path/to/city/.gc/runtime/packs/maintenance/jsonl-archive` (or
  `/path/to/city/.gc/jsonl-archive` from before packs), and `GC_JSONL_ARCHIVE_REPO`
  overrides them all. `<sql_name>` is the database name Dolt serves: `<dbname>` with each
  `-` replaced by `_`. A failed `gc start` names the exact file in its manual steps.

> **Split-store cities.** On a city that serves its infrastructure classes
> (graph, sessions, messaging, orders, nudges) from a separate binding, the
> work store holds work beads only. An archive taken before the split carries
> infrastructure rows, and `gc bd import` refuses such a file rather than
> strand them in the work store. It names the rows by class. Re-run it with
> `--skip-infra` to import the work rows only. The binding already holds the
> live infrastructure state, so those rows are left out, not restored over it.

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
- [ ] The restored database registers the backup it came from (`dolt backup`, run in
      `<data_dir>/<dbname>`, lists it), and `gc bd backup status --json` shows a recent
      `.dolt.last_sync` after the next backup run
- [ ] The corrupt store under `corrupt-aside/` (or `<somewhere-safe>`) is kept until the
      restored database is confirmed, then removed (the `dolt-corrupt-aside` doctor check
      reports it until then)
- [ ] Open in-progress beads reviewed and state corrected if import was used
- [ ] Root cause of journal growth identified (missing compaction schedule? high write volume?)
- [ ] `gc dolt compact` scheduled or an order configured to prevent recurrence
