---
title: Migrating a Gas Town Instance to Gas City
description: Step-by-step guide to adopting existing Gas Town rigs (with live bead history) into a Gas City install.
sidebarTitle: Migrating from Gas Town
---

This guide walks you through migrating a running Gas Town instance into
Gas City supervision, preserving each rig's beads database. It covers
the full lifecycle: pre-flight, adoption, post-adoption fallout, and
rollback.

> [!NOTE]
> This is a hands-on migration procedure. For the conceptual mapping of
> Gas Town concepts to Gas City primitives, see
> [Coming from Gas Town](/getting-started/coming-from-gastown).

## When to use this guide

You have all of:

- A running Gas Town (`gt` daemon + Dolt + at least one rig)
- The `gc` CLI installed and a checkout of the `gascity` repo
- A desire to move supervision from `gt` to `gc` without losing bead history

If you can simply `bd init` a new city, skip this guide — it is for
migrations, not greenfield installs.

## Decisions to make first

Resolve these four questions before you touch anything. Wrong answers waste
an hour or wedge a Dolt server.

### 1. Fresh city vs. in-place repair

**Recommend: fresh city** (`cp -r ~/gascity/examples/gastown ~/my-city`).

Reasons to prefer fresh:

- The example tracks current `gc` schema (`provider = "claude"`, current
  pack layout) — no legacy rewrites needed.
- System-managed packs in stale cities use a legacy
  `formulas/orders/<x>/order.toml` layout that produces `deprecated order
  path` warnings on every `gc` invocation.
- Old cities' Dolt packs may have been silently wedged for weeks
  (max-connections exhaustion is a common failure mode).

Only repair an existing city if you have already invested in customizing it.

### 2. Single-Dolt vs. dual-Dolt

Gas City's Dolt pack auto-spawns its own server on port 33464, distinct
from Town's 3307. **Run them side-by-side during the transition.** Pointing
Gas City at Town's live Dolt creates dual-supervisor races, schema-drift
risk, and fights between `gc`'s system-managed packs and Town's data.

Migrate beads via `gc rig add --adopt`, not by repointing.

### 3. Where the rigs live

You can either:

- **Keep rigs in place** at their existing `~/gt/<rig>/` paths and adopt
  them — recommended, less churn.
- Relocate to `~/projects/<rig>/` first, then adopt.

This guide assumes "keep in place."

### 4. Stop Town entirely vs. suspend per-rig

`gc rig add --adopt` modifies files inside the live rig directory (installs
agent hooks, generates `routes.jsonl`). If Town's daemon is reacting to
those same files, you can race. **Stop Gas Town entirely before adopting.**

> [!WARNING]
> `gt down --all` will kill the Mayor session that runs it. If you are
> operating from inside a Mayor agent, use the
> [detached-script pattern](#detached-script-variant) so the migration
> survives.

## Pre-flight checks

Run these before starting. Each maps to a failure mode someone has
actually hit.

```bash
# Tool versions
which gc && gc --version
dolt version            # Must be >= 1.86.1; gc doctor will fail otherwise

# Gas City state
gc cities                                # What's already registered?
ls ~/gascity/examples/gastown            # Confirm fresh-city template exists

# Gas Town state
gt dolt status                           # Note PID, port, DBs, latency
gt agents status                         # Note any in-flight polecat work

# Per-rig state (substitute your rig names)
for rig in <rig1> <rig2> <rig3>; do
  echo "=== $rig ==="
  ls ~/gt/$rig/.beads/ 2>&1
  test -f ~/gt/$rig/.beads/metadata.json \
    && grep -i prefix ~/gt/$rig/.beads/metadata.json
done
```

What you are looking for:

| Check | Good | Fix first |
|---|---|---|
| `dolt version` | ≥ 1.86.1 | Upgrade before stopping Town (Step 1) |
| `.beads/metadata.json` exists per rig | yes | If missing, that rig was never properly initialized — investigate before adopting |
| Existing bead prefix per rig | matches what you'll pass to `--prefix` | Note it now; mismatches are the #1 failure mode |
| In-flight polecat work | none / acceptable to lose | `gt mol roster` and let work drain, or accept loss |

Heterogeneous `.beads/` contents are normal. Some rigs have just
`metadata.json` + `redirect`; others have full `dolt/`, `audit.log`,
`backup/`. `gc rig add --adopt` handles both shapes.

## Step 1 — Upgrade Dolt (if needed)

`gc doctor` requires Dolt ≥ 1.86.1. Skip this step if you already have a
compatible version.

```bash
gt dolt stop                                                 # Clean shutdown
```

Run the installer from the user's shell (sudo needs a TTY):

```bash
curl -L https://github.com/dolthub/dolt/releases/latest/download/install.sh | sudo bash
```

Then bring Town's Dolt back:

```bash
dolt version                                                 # Verify
gt dolt start
gt dolt status                                               # Confirm DBs + latency healthy
```

## Step 2 — Set up the fresh Gas City

```bash
cp -r ~/gascity/examples/gastown ~/my-city
cd ~/my-city

# Strip example-only files (optional but cleaner)
rm -f FUTURE.md gastown_test.go maintenance_scripts_test.go \
      SDK-ROADMAP.md testenv_import_test.go

gc doctor                       # Expected: some warns/fails — city not registered yet
bd init                         # Initialize beads; auto-derives prefix from dirname
chmod 700 .beads                # Required: gc rejects 0755
gc register                     # Registers city, installs systemd user unit
gc cities                       # Verify both old + new appear
```

> [!IMPORTANT]
> `bd init` derives the bead prefix from the directory name. If you name
> the dir `my-city`, your city-level beads will be prefixed `my-city-*`.
> Pick a directory name you can live with, or rename before running
> `bd init`.

Two non-obvious things `gc register` does:

- Installs `~/.local/share/systemd/user/gascity-supervisor.service`
- Auto-starts the supervisor for the new city

## Step 3 — Find each rig's existing bead prefix

This is the step everyone skips and everyone regrets. **Do not let
`gc rig add` auto-derive a prefix from the directory name** — Town's rigs
often have prefixes that don't match their dirnames (e.g., a rig at
`~/gt/karasearch/` may use prefix `ks`).

```bash
for rig in <rig1> <rig2> <rig3>; do
  prefix=$(grep -oP '"prefix"\s*:\s*"\K[^"]+' ~/gt/$rig/.beads/metadata.json 2>/dev/null)
  echo "$rig -> ${prefix:-MISSING}"
done
```

Write each prefix down. You will pass it as `--prefix` in Step 5.

## Step 4 — Stop Gas Town

Once you start this, Town is offline until you `gt start` or the migration
finishes.

> [!WARNING]
> If you are running this from inside a Mayor agent session, **stop here**
> and switch to the [detached-script pattern](#detached-script-variant).
> `gt down --all` will kill your session.

```bash
gt down --all                   # Stops refineries, witnesses, Mayor, daemon, Dolt
sleep 5
pgrep -af 'dolt sql-server.*\.dolt-data' || echo "  no dolt"
pgrep -af 'gt-daemon|gt daemon' || echo "  no daemon"
```

The `gt down --all` output may include warnings like
`skipping ~/gt/.beads/dolt — may contain unmigrated data` — these are
informational; the dirs are preserved.

## Step 5 — Adopt each rig

**Always pass `--prefix` explicitly:**

```bash
CITY=~/my-city

gc rig add ~/gt/<rig1> --adopt --prefix <prefix1> --city "$CITY"
gc rig add ~/gt/<rig2> --adopt --prefix <prefix2> --city "$CITY"
gc rig add ~/gt/<rig3> --adopt --prefix <prefix3> --city "$CITY"

gc --city "$CITY" rig list
```

Each successful invocation prints:

```
Adding rig '<name>'...
  Prefix: <XX>
  Adopted existing beads database
  Generated routes.jsonl for cross-rig routing
Rig added.
```

### Detached-script variant

Because `gt down --all` will kill the Mayor session, write the whole
sequence to a file and `nohup` it before invoking `gt down`. Log output so
you can read results in the next session.

```bash
cat > /tmp/migrate.sh <<'EOF'
#!/bin/bash
set -u
LOG=/tmp/migrate.log
exec >>"$LOG" 2>&1
echo "=== started $(date -Iseconds) ==="

CITY=~/my-city

gt down --all
sleep 5
pgrep -af 'dolt sql-server.*\.dolt-data' || echo "no dolt"
pgrep -af 'gt-daemon|gt daemon' || echo "no daemon"

# Substitute your rigs and prefixes from Step 3:
gc rig add ~/gt/<rig1> --adopt --prefix <prefix1> --city "$CITY"; echo "<rig1> exit: $?"
gc rig add ~/gt/<rig2> --adopt --prefix <prefix2> --city "$CITY"; echo "<rig2> exit: $?"

gc --city "$CITY" rig list
echo "=== finished $(date -Iseconds) ==="
EOF
chmod +x /tmp/migrate.sh
nohup /tmp/migrate.sh </dev/null >/dev/null 2>&1 &
disown
echo "PID $!"
```

After the next session starts, read the log with `cat /tmp/migrate.log`.

## Step 6 — Verification

```bash
gc cities                                                    # New city listed
gc --city ~/my-city rig list                                 # All rigs adopted
ls ~/my-city/.gc/runtime/packs/dolt/                         # City's Dolt pack present
systemctl --user status gascity-supervisor.service           # Supervisor up
```

Per-rig sanity:

```bash
for rig in <rig1> <rig2> <rig3>; do
  echo "=== $rig ==="
  cd ~/gt/$rig
  bd list --status open --limit 3                            # Beads still readable
done
```

If `bd list` works in each rig, the adoption preserved history correctly.

## Step 7 — Post-adoption fallout check

`gc rig add --adopt` silently rewrites or augments three things in each
adopted rig's `.beads/`. None of these are errors at adoption time, but
each can break Town-side tools (`bd`, `gt mail`, `gt sling`) until
repaired. **Always run these checks.**

### Issue A — Project identity mismatch

**Symptom:**

```
Error: failed to open database: PROJECT IDENTITY MISMATCH — refusing to connect
  Local project ID (metadata.json):  gc-local-fc2a5de36335bba9dd5767010717e0dd
  Database project ID:               f9a5e55e-d4be-4993-b2a4-2e4dc54bddd9
```

**Cause:** `gc rig add --adopt` runs `gc dolt-state ensure-project-id`
which should read the existing Dolt-stored `_project_id` and copy it
into `metadata.json`. When there is a race between `bd migrate
--update-repo-id` re-seeding the Dolt value and `gc` reading it, `gc`
takes the "both missing" branch, mints a fresh random `gc-local-<32hex>`,
writes that into `metadata.json`, and never touches the DB.

**Check:**

```bash
gt mail inbox 2>&1 | grep -q "PROJECT IDENTITY MISMATCH" \
  && echo "FIX NEEDED: see fix below" \
  || echo "OK: project_id"
```

**Fix — read the DB's actual UUID and write it into `metadata.json`:**

```bash
# From a rig whose bd works (any rig that didn't fail with MISMATCH):
cd ~/gt/<working-rig>
bd sql "SELECT value FROM \`<affected-db>\`.metadata WHERE \`key\` = '_project_id'"
# Returns e.g. f9a5e55e-d4be-4993-b2a4-2e4dc54bddd9

# Replace the gc-local-* value in each metadata.json that references
# that DB with the UUID returned above.
```

> [!WARNING]
> **Do not just delete the `project_id` field.** `gc-beads-bd.sh` re-runs
> `ensure-project-id` on every bd-bridge initialization; if both sides are
> empty and the DB gets reseeded by `bd init`, you'll regenerate a fresh
> mismatch. The only stable fix is to make `metadata.json` match what the
> DB actually holds.

### Issue B — Cross-rig bead lookup fails

**Symptom:** `gt sling <bead-id> <rig>` says `bead 'XX-yyy' not found`,
or `Warning: no route found for prefix "ks-"`.

**Cause:** `gc rig add --adopt` rewrites `~/gt/.beads/routes.jsonl` from
gt-style (trailing-dash prefixes, paths relative to `.beads/`) to gc-style
(no-dash prefixes, `../path` notation). Town's `bd` resolver expects the
gt-style format.

**Check:**

```bash
grep -E '"prefix":"[a-z]+-' ~/gt/.beads/routes.jsonl > /dev/null \
  && echo "OK: routes.jsonl in gt format" \
  || echo "FIX NEEDED: routes.jsonl in gc format"
```

**Fix — revert `routes.jsonl`** (it is git-tracked at the town root):

```bash
git -C ~/gt checkout HEAD -- .beads/routes.jsonl
```

Correct gt-format example:

```json
{"prefix":"hq-","path":"."}
{"prefix":"ro-","path":"rosie"}
{"prefix":"hod-","path":"hod"}
{"prefix":"ks-","path":"karasearch"}
```

Trailing dashes and bare relative paths = gt format.

### Issue C — Formula resolution failure

**Symptom:**

```
Error: resolving formula: extends mol-polecat-base: formula "mol-polecat-base"
not found in search paths
```

**Cause:** `gc` adoption drops `.toml` files (gc-style) into
`<rig>/.beads/formulas/` alongside Town's `.formula.toml` files. The
gc-style formulas extend `mol-polecat-base`, which lives only in the Gas
City system packs and is not on Town's resolution path.

**Check:**

```bash
ls ~/gt/.beads/formulas/mol-polecat-work.toml 2>/dev/null \
  && echo "WARN: gc-added .toml may shadow gt formula" \
  || echo "OK: formulas"
```

**Quick workaround — bypass the formula entirely:**

```bash
gt sling <bead-id> <rig> --create --hook-raw-bead --agent <agent>
```

**Proper fix — restore Town's formulas.** Three approaches:

1. If your formulas dir is git-tracked:
   ```bash
   git -C ~/gt checkout HEAD -- .beads/formulas/
   git -C ~/gt clean -f .beads/formulas/
   ```
2. Move gc-added `.toml` files aside:
   ```bash
   mkdir /tmp/gc-adopted-formulas.bak
   mv ~/gt/.beads/formulas/mol-*.toml /tmp/gc-adopted-formulas.bak/
   ```
3. Copy the base formula from the city's system pack:
   ```bash
   cp ~/my-city/.gc/system/packs/core/formulas/mol-polecat-base.toml \
      ~/gt/<rig>/.beads/formulas/
   ```

## Common failures and recovery

| Symptom | Cause | Fix |
|---|---|---|
| `.beads already exists; use --adopt` | Forgot `--adopt` flag | Add `--adopt` |
| `rig "X" already has bead prefix "AAA" (requested "BBB")` | Auto-derived prefix mismatch | Re-run with `--prefix AAA` (the existing one) |
| `.beads/metadata.json` missing on a rig | Rig was never initialized under Town | Skip it, or `bd init` first (starts with empty history) |
| `chmod 700` required | `gc` rejects 0755 on `.beads/` | `chmod 700 .beads` |
| `max connections reached` on old city Dolt | Wedged supervisor with zombie probe clients | Goroutine dump → kill zombies → kill Dolt → clear `dolt.pid`/`dolt.lock`. Never `rm -rf` Dolt data. |
| `gc doctor --fix` fails on `custom-types:city` | Stale system pack layout | Pivot to a fresh city |
| `sudo: a terminal is required` | Running installer from agent shell with no TTY | Run from user's shell directly |
| Mayor session died mid-migration | Ran inline instead of detached | Start fresh session, `cat /tmp/migrate.log`, retry failed `gc rig add` commands |

### Wedged Dolt recovery

```bash
# 1. Goroutine dump (preserves evidence)
kill -QUIT $(cat ~/my-city/.gc/runtime/packs/dolt/dolt.pid)

# 2. Identify zombie probes
ps aux | grep -E "dolt|gc " | grep -v grep

# 3. Kill zombies, then Dolt, then clear stale lock
kill <zombie-pids>
kill <dolt-pid>
rm -f ~/my-city/.gc/runtime/packs/dolt/dolt.pid \
      ~/my-city/.gc/runtime/packs/dolt/dolt.lock
chmod 700 ~/my-city/.beads
```

## Rollback

This migration is largely reversible because nothing destroys Town state:

- `gc rig remove --city ~/my-city <rig>` — unregisters a rig (Town's
  `.beads/` is untouched).
- `gt start` — brings Town back up against the same data.
- The fresh city dir can be removed after `gc rig remove` for each rig
  and stopping the supervisor:
  ```bash
  systemctl --user stop gascity-supervisor.service
  ```

> [!IMPORTANT]
> Do **not** `rm -rf` any `.dolt-data/` directory. Dolt state lives there.
> Use `gt dolt cleanup` for orphan test DBs, never `rm -rf`.

## Quick reference

| Task | Command |
|---|---|
| Find rig prefix | `grep prefix ~/gt/<rig>/.beads/metadata.json` |
| Stop Town | `gt down --all` |
| Adopt rig | `gc rig add <path> --adopt --prefix <existing> --city <city>` |
| List rigs | `gc --city <city> rig list` |
| Bring Town back | `gt start` |
| Migration log (detached) | `cat /tmp/migrate.log` |
| Read DB project_id | `bd sql "SELECT value FROM \`<db>\`.metadata WHERE \`key\` = '_project_id'"` |
| Restore Town routes | `git -C ~/gt checkout HEAD -- .beads/routes.jsonl` |
| Bypass missing formulas | add `--hook-raw-bead` to `gt sling` |
