---
title: Migrating a Gas Town Instance to Gas City
description: Step-by-step guide to adopting existing Gas Town rigs (with live bead history) into a Gas City install, including the Phase 2 cutover from gt-primary to gc-primary supervision.
sidebarTitle: Migrating from Gas Town
---

This guide walks you through migrating a running Gas Town instance into
Gas City supervision, preserving each rig's beads database. It covers
the full lifecycle: pre-flight, adoption, post-adoption fallout, the
Phase 2 cutover from gt-primary to gc-primary supervision, and rollback.

> [!NOTE]
> This is a hands-on migration procedure derived from a real, fully
> completed migration against `gc` v1.0.0. For the conceptual mapping of
> Gas Town concepts to Gas City primitives, see
> [Coming from Gas Town](/getting-started/coming-from-gastown).

> [!IMPORTANT]
> **Two-phase migration.** `gc rig add --adopt` is only Phase 1
> (registration). Phase 2 (cutting over from gt-primary to gc-primary)
> is a separate, larger task with its own gotchas. This guide covers
> both. Skip to [Phase 2 — Cutover to gc-primary supervision](#phase-2--cutover-to-gc-primary-supervision)
> once Phase 1 is stable.

## End-state targeted

This guide targets — and has been verified to achieve — the following
end-state:

- Single Dolt instance hosting all rig DBs and the city DB
- `gascity-supervisor.service` running as the sole supervisor; gt's
  daemon stopped permanently
- All historical bead data preserved (no loss of closed work, convoys,
  mailboxes)
- `gc mayor` alive on Claude (or any provider you choose)
- Polecats spawning autonomously via
  `bd update --set-metadata gc.routed_to=...` and completing routed
  work end-to-end
- Per-role agent overrides functioning (e.g., mayor on Claude, others
  on a cheaper provider for cost savings)
- `gc doctor` passing with zero failures

**Realistic effort:** ~6–10 hours hands-on across two sittings, plus
~1 week of soak before retiring gt artifacts. Most time goes to Phase 2
cutover (data sync + per-role config + verification), not Phase 1
adoption.

## TL;DR — order of operations

```
PHASE 1 — adoption (registration only)
  1. Pre-flight checks (Dolt version, gc CLI, rig metadata, prefixes)
  2. Upgrade Dolt if <1.86.1
  3. Set up fresh city (cp -r ~/gascity/examples/gastown ~/my-city)
  4. Discover existing rig prefixes (CRITICAL — write them down)
  5. Stop Gas Town (gt down --all) — detach script if running from Mayor
  6. Adopt rigs (gc rig add <path> --adopt --prefix <existing> --city <city>)
  7. Post-adoption fallout check: project_id mismatch, routes.jsonl
     format, formula shadowing (Issues A/B/C below)

PHASE 2 — cutover to gc-primary
  P2.1 Map dual-supervisor + Dolt state
  P2.2 gc beads city use-managed (sets canonical bd endpoint)
  P2.3 Reconcile project_id mismatches (revert metadata.json edits to
       match gc-Dolt's stored UUIDs)
  P2.4 Watch supervisor reconcile to outcome=success
  P2.5 Address data divergence (in-place sync gt→gc; preserve city DB)
  P2.5.5 Bump session.startup_timeout to 240s (vital for opencode-family)
  P2.5.6 Apply local source patch for opencode no-escape-before-enter
         (or fall back to claude-only routing)
  P2.6.0 Define any custom providers ([providers.<name>] base = "opencode")
  P2.6.5 Add rig-pack imports + rig-scoped patches (per rig)
  P2.6  Recreate per-role provider overrides (city-scoped + per-rig)
  P2.7  Stop gt (gt down --all), validate gc-only operation
  P2.8  Cleanup (deregister orphan cities by editing ~/.gc/cities.toml)
```

The single most impactful settings change in the whole migration is
adding `[session] startup_timeout = "240s"` to `city.toml`. Without it,
spawning anything backed by a slow-cold-start provider silently fails
in a way that has nothing obvious to do with timeouts.

## What `gc` actually is (read this first)

Gas City is **not** a rebranded Gas Town. It is the SDK extracted from
gt. Critical mental shifts:

- **`gc` has no native role model.** "mayor", "witness", "refinery",
  "polecat", "deacon" are not types in gc's Go code. They are declared
  as `[[named_session]]` entries in the **gastown pack** at
  `~/gascity/examples/gastown/packs/gastown/`. The pack IS the role
  model.
- **A "city" is the unit of supervision.** A
  `gascity-supervisor.service` systemd unit drives one or more cities;
  each city composes packs that define its agents and behavior.
- **Work dispatch is bead-metadata-driven, not file-hook-driven.**
  Setting `metadata.gc.routed_to = <rig>/polecat` on a bead causes
  gc's pool reconciler to spawn a polecat. The polecat completes by
  running `gc runtime drain-ack` and exiting — no merge-queue daemon.
- **`gc` has its own Dolt** (typically port 36910) separate from gt's
  (3307). Each city's Dolt holds the city's own beads and may hold
  mirrored copies of the rigs' databases. Data on the two Dolts
  diverges quickly under dual-supervision; expect to reconcile.
- **Per-role agent overrides** are config patches, not a `role_agents`
  map:
  ```toml
  [[patches.agent]]
  name = "gastown.mayor"      # NOTE: gastown. prefix for pack-imported agents
  provider = "claude"
  ```
  For rig-scoped agents (polecat/witness/refinery), use
  `[[rigs.patches]]` per-rig — there is no city-wide "all polecats use
  X" override exposed.

The canonical translation guide lives in
[Coming from Gas Town](/getting-started/coming-from-gastown), with the
full gt→gc command map in that document.

## When to use this guide

You have all of:

- A running Gas Town (`gt` daemon + Dolt + at least one rig)
- The `gc` CLI installed and a checkout of the `gascity` repo
- A desire to move supervision from `gt` to `gc` without losing bead
  history

If you can simply `bd init` a new city, skip this guide — it is for
migrations, not greenfield installs.

## Decisions to make first

Resolve these four questions before you touch anything. Wrong answers
waste an hour or wedge a Dolt server.

### 1. Fresh city vs. in-place repair

**Recommend: fresh city** (`cp -r ~/gascity/examples/gastown ~/my-city`).

Reasons to prefer fresh:

- The example tracks current `gc` schema (`provider = "claude"`,
  current pack layout) — no legacy rewrites needed.
- System-managed packs in stale cities use a legacy
  `formulas/orders/<x>/order.toml` layout that produces `deprecated
  order path` warnings on every `gc` invocation.
- Old cities' Dolt packs may have been silently wedged for weeks
  (max-connections exhaustion is a common failure mode).

Only repair an existing city if you have already invested in
customizing it.

### 2. Single-Dolt vs. dual-Dolt

Gas City's Dolt pack auto-spawns its own server on port 33464, distinct
from Town's 3307. **Run them side-by-side during the transition.**
Pointing Gas City at Town's live Dolt creates dual-supervisor races,
schema-drift risk, and fights between `gc`'s system-managed packs and
Town's data.

Migrate beads via `gc rig add --adopt`, not by repointing.

### 3. Where the rigs live

You can either:

- **Keep rigs in place** at their existing `~/gt/<rig>/` paths and
  adopt them — recommended, less churn.
- Relocate to `~/projects/<rig>/` first, then adopt.

This guide assumes "keep in place."

### 4. Stop Town entirely vs. suspend per-rig

`gc rig add --adopt` modifies files inside the live rig directory
(installs agent hooks, generates `routes.jsonl`). If Town's daemon is
reacting to those same files, you can race. **Stop Gas Town entirely
before adopting.**

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

`gc doctor` requires Dolt ≥ 1.86.1. Skip this step if you already have
a compatible version.

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
> `bd init` derives the bead prefix from the directory name. If you
> name the dir `my-city`, your city-level beads will be prefixed
> `my-city-*`. Pick a directory name you can live with, or rename
> before running `bd init`.

Two non-obvious things `gc register` does:

- Installs `~/.local/share/systemd/user/gascity-supervisor.service`
- Auto-starts the supervisor for the new city

## Step 3 — Find each rig's existing bead prefix

This is the step everyone skips and everyone regrets. **Do not let
`gc rig add` auto-derive a prefix from the directory name** — Town's
rigs often have prefixes that don't match their dirnames (e.g., a rig
at `~/gt/karasearch/` may use prefix `ks`).

```bash
for rig in <rig1> <rig2> <rig3>; do
  prefix=$(grep -oP '"prefix"\s*:\s*"\K[^"]+' ~/gt/$rig/.beads/metadata.json 2>/dev/null)
  echo "$rig -> ${prefix:-MISSING}"
done
```

Write each prefix down. You will pass it as `--prefix` in Step 5.

## Step 4 — Stop Gas Town

Once you start this, Town is offline until you `gt start` or the
migration finishes.

> [!WARNING]
> If you are running this from inside a Mayor agent session, **stop
> here** and switch to the [detached-script pattern](#detached-script-variant).
> `gt down --all` will kill your session.

```bash
gt down --all                   # Stops refineries, witnesses, Mayor, daemon, Dolt
sleep 5
pgrep -af 'dolt sql-server.*\.dolt-data' || echo "  no dolt"
pgrep -af 'gt-daemon|gt daemon' || echo "  no daemon"
```

The `gt down --all` output may include warnings like `skipping
~/gt/.beads/dolt — may contain unmigrated data` — these are
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
sequence to a file and `nohup` it before invoking `gt down`. Log output
so you can read results in the next session.

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

## Post-adoption fallout

`gc rig add --adopt` silently rewrites or augments three things in each
adopted rig's `.beads/`. None of these are errors at adoption time, but
each can break Town-side tools (`bd`, `gt mail`, `gt sling`) until
repaired. **Always run these checks before declaring Phase 1
successful** — these failures don't surface until you actually try to
use Mayor mail / sling / cross-rig beads.

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
takes the "both missing" branch, mints a fresh random
`gc-local-<32hex>`, writes that into `metadata.json`, and never touches
the DB. Now metadata says one thing, the DB still has its original
UUID, and `bd` refuses to connect.

This typically only happens on the rig backed by the database that
already had a UUID written by Town's `bd init` (often the town-root /
`hq` database). Fresh rigs that never had a Dolt-stored project_id are
unaffected.

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
# that DB with the UUID returned above. Multiple rigs may share one DB
# (e.g., town root and another rig both using 'hq').
```

After the edit, `gc`'s reconciler (next time it runs) will see
"metadata and DB match" → no-op. The fix is permanent.

If a DB has no `_project_id` row at all (returns `(0 rows)`) and
`metadata.json` has a `gc-local-*` value, leave it alone — `bd`
tolerates "DB-empty + metadata-set" and `gc` will eventually seed the
DB to match on next operation.

> [!WARNING]
> **Do not just delete the `project_id` field.** `gc-beads-bd.sh`
> re-runs `ensure-project-id` on every bd-bridge initialization; if
> both sides are empty and the DB gets reseeded by `bd init`, you'll
> regenerate a fresh mismatch. The only stable fix is to make
> `metadata.json` match what the DB actually holds.

### Issue B — Cross-rig bead lookup fails

**Symptom:** `gt sling <bead-id> <rig>` says `bead 'XX-yyy' not found`,
or `Warning: no route found for prefix "ks-"`.

**Cause:** `gc rig add --adopt` rewrites `~/gt/.beads/routes.jsonl`
from gt-style (trailing-dash prefixes, paths relative to `.beads/`) to
gc-style (no-dash prefixes, `../path` notation). Town's `bd` resolver
expects the gt-style format and silently fails to match.

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
{"prefix":"hq-cv-","path":"."}
{"prefix":"ro-","path":"rosie"}
{"prefix":"hod-","path":"hod"}
{"prefix":"ks-","path":"karasearch"}
```

Trailing dashes and bare relative paths = gt format. If `gc` later
rewrites it again, repeat.

### Issue C — Formula resolution failure

**Symptom:**

```
Error: resolving formula: extends mol-polecat-base: formula "mol-polecat-base"
not found in search paths
```

**Cause:** `gc` adoption drops `.toml` files (gc-style) into
`<rig>/.beads/formulas/` alongside Town's `.formula.toml` files. The
gc-style `mol-polecat-work.toml` extends `mol-polecat-base`, which
lives only in `<gas-city>/.gc/system/packs/core/formulas/` and is not
on Town's resolution path. The town-root copy at `~/gt/.beads/formulas/`
and each adopted rig's per-rig copy both have this problem — and the
per-rig `.formula.toml` was *also* rewritten to the gc-extends-base
form.

**Check:**

```bash
ls ~/gt/.beads/formulas/mol-polecat-work.toml 2>/dev/null \
  && echo "WARN: gc-added .toml may shadow gt formula" \
  || echo "OK: formulas"
```

**Quick workaround — bypass the formula entirely with `--hook-raw-bead`:**

```bash
gt sling <bead-id> <rig> --create --hook-raw-bead --agent <agent>
```

The polecat receives the bead and starts work without molecule
scaffolding. Sufficient for most one-shot tasks.

**Proper fix — restore Town's formulas.** Three approaches in order of
preference:

1. If your formulas dir is git-tracked:
   ```bash
   git -C ~/gt checkout HEAD -- .beads/formulas/
   git -C ~/gt clean -f .beads/formulas/
   ```
2. Move gc-added `.toml` files aside (anything without the
   `.formula.toml` suffix that wasn't there pre-migration):
   ```bash
   mkdir /tmp/gc-adopted-formulas.bak
   mv ~/gt/.beads/formulas/mol-*.toml /tmp/gc-adopted-formulas.bak/
   ```
3. Install `mol-polecat-base` into each rig's formulas dir by copying
   from the city's system pack:
   ```bash
   cp ~/my-city/.gc/system/packs/core/formulas/mol-polecat-base.toml \
      ~/gt/<rig>/.beads/formulas/
   ```
   This makes the gc-style formulas resolvable. Cleanest if you want
   to keep gc's molecule definitions.

## Common failures and recovery

| Symptom | Cause | Fix |
|---|---|---|
| `.beads already exists; use --adopt` | Forgot `--adopt` flag | Add `--adopt` |
| `rig "X" already has bead prefix "AAA" (requested "BBB")` | Auto-derived prefix mismatch | Re-run with `--prefix AAA` (the existing one) |
| `.beads/metadata.json` missing on a rig | Rig was never initialized under Town | Skip it, or `bd init` first (starts with empty history) |
| `chmod 700` required | `gc` rejects 0755 on `.beads/` | `chmod 700 .beads` |
| `max connections reached` on old city Dolt | Wedged supervisor with zombie probe clients | See [Wedged Dolt recovery](#wedged-dolt-recovery) — never `rm -rf` Dolt data |
| `gc doctor --fix` fails on `custom-types:city` | Stale system pack layout | Pivot to a fresh city |
| `sudo: a terminal is required` | Running installer from agent shell with no TTY | Run from user's shell directly |
| Mayor session died mid-migration | Ran inline instead of detached | Start fresh session, `cat /tmp/migrate.log`, retry failed `gc rig add` commands |

### Wedged Dolt recovery

```bash
# 1. Goroutine dump (preserves evidence)
kill -QUIT $(cat ~/<oldcity>/.gc/runtime/packs/dolt/dolt.pid)

# 2. Identify zombie probes (often `gc doctor` subprocesses)
ps aux | grep -E "dolt|gc " | grep -v grep

# 3. Kill zombies, then Dolt, then clear stale lock
kill <zombie-pids>
kill <dolt-pid>
rm -f ~/<oldcity>/.gc/runtime/packs/dolt/dolt.pid \
      ~/<oldcity>/.gc/runtime/packs/dolt/dolt.lock
chmod 700 ~/<oldcity>/.beads
```

## Phase 1 verification

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

If `bd list` works in each rig, the adoption preserved history
correctly.

### Post-adoption fallout check (REQUIRED)

```bash
# Issue A — project_id mismatch (will hard-fail on the offending rig)
gt mail inbox 2>&1 | grep -i "PROJECT IDENTITY MISMATCH" && echo "FIX: see Issue A"

# Issue B — routes.jsonl format (look for trailing-dash prefixes)
grep -E '"prefix":"[a-z]+-' ~/gt/.beads/routes.jsonl > /dev/null \
  && echo "routes OK (gt format)" \
  || echo "FIX: routes.jsonl in gc format — see Issue B"

# Issue C — formula resolution
ls ~/gt/.beads/formulas/mol-polecat-work.toml 2>/dev/null \
  && echo "WARN: gc-added mol-polecat-work.toml present — may shadow gt formula. See Issue C"
```

## Rollback

This migration is largely reversible because nothing destroys Town
state:

- `gc rig remove --city ~/my-city <rig>` — unregisters a rig (Town's
  `.beads/` is untouched).
- `gt start` — brings Town back up against the same data.
- The fresh city dir can be removed after `gc rig remove` for each rig
  and stopping the supervisor:
  ```bash
  systemctl --user stop gascity-supervisor.service
  ```

> [!IMPORTANT]
> Do **not** `rm -rf` any `.dolt-data/` directory. Dolt state lives
> there. Use `gt dolt cleanup` for orphan test DBs, never `rm -rf`.

## Phase 2 — Cutover to gc-primary supervision

Phase 1 (`gc rig add --adopt`) only registers rigs in the city. Until
you do Phase 2, both `gt daemon` and `gascity-supervisor` race for the
same rig file trees. Symptoms: polecats lifecycled mysteriously,
witnesses going stopped without explanation, opaque Dolt errors.

> [!WARNING]
> Do not ship work through either supervisor in dual-mode beyond brief
> validation. The dual-supervisor window is for verification only.

### Step P2.1 — Inventory the parallel state

Before changing anything:

```bash
# Both supervisors alive?
pgrep -af "gt daemon"
systemctl --user status gascity-supervisor.service

# All Dolts running and which port each uses
ss -tlnp | grep dolt
pgrep -af "dolt sql-server"

# Each rig's metadata.json project_id vs gc-Dolt's stored _project_id:
for rig in <rig1> <rig2>; do
  echo "=== $rig (metadata) ==="
  grep -E "dolt_database|project_id" ~/gt/$rig/.beads/metadata.json
done
echo "=== gc Dolt's _project_id per database ==="
env -i HOME=$HOME PATH=$PATH bash -c 'cd ~/my-city &&
  for db in <db1> <db2> <db_city>; do
    echo "--- $db ---"
    bd sql "SELECT value FROM \`$db\`.metadata WHERE \`key\` = '\''_project_id'\''"
  done'
```

You're looking for **mismatches between `metadata.json` and gc-Dolt's
`_project_id`** — those will block gc's supervisor from initializing
the rig.

> [!IMPORTANT]
> The most common mismatch is on the rig that shares the town root's
> database (typically `hq`). During Phase 1, `gc` stamped a
> `gc-local-*` UUID into `metadata.json`. If you later "rescued" `gt`
> by reverting metadata to match gt's Dolt UUID (per
> [Issue A](#issue-a--project-identity-mismatch)), you've now broken
> the gc-side match. **Phase 2 reverses that rescue.**

### Step P2.2 — Wire the city's bd to its own Dolt

Verify the city's `.beads/metadata.json` has a `dolt_server_host` /
`dolt_server_port` pointing at the city's actual Dolt port. If missing,
`bd` will dial `127.0.0.1:0` (placeholder) and silently fail.

The proper way to set this is `gc beads city use-managed` (run from
the city directory). It writes the canonical "city is its own bd
endpoint" state.

> [!NOTE]
> `use-managed` is whole-city — it also rewrites rig endpoints to
> "inherited" (rigs inherit the city's endpoint). If you need rigs
> pointed elsewhere (e.g., still at gt's 3307 during a hybrid period),
> there is no exposed per-rig endpoint override; you'd need
> `use-external` city-wide instead, which points the entire city at an
> external Dolt.

```bash
cd ~/my-city

# Dry-run first to see what'll change:
gc beads city use-managed --dry-run

# Apply:
gc beads city use-managed
```

After this, restart the supervisor (it caches resolution):

```bash
systemctl --user restart gascity-supervisor.service
tail -F ~/.gc/supervisor.log
```

### Step P2.3 — Reconcile project_id mismatches

For each rig where `metadata.project_id != <gc-Dolt>._project_id`:

```bash
# Read what the gc-side Dolt actually stores:
env -i HOME=$HOME PATH=$PATH bash -c \
  'cd ~/my-city && bd sql "SELECT value FROM \`<db>\`.metadata WHERE \`key\` = '\''_project_id'\''"'

# Edit the rig's metadata.json to use that exact value
```

For example: gt's `hq` DB might have stored `f9a5e55e-...`, but gc's
`hq` DB (mirrored at adoption time) has `gc-local-fc2a5...`. Setting
`metadata.json` to `gc-local-fc2a5...` makes gc happy but breaks gt;
setting it to `f9a5e55e-...` does the opposite. Phase 2 commits to gc.

### Step P2.4 — Watch the supervisor reconcile

After fixing project_ids and restarting, the supervisor log should
show:

```
session lifecycle: op=start wave=0 session=mayor template=mayor outcome=...
session lifecycle: op=start wave=0 candidates=N
City started.
```

Mayor session start can take >60s (Claude bootstrap + first model
load). `outcome=deadline_exceeded` on the first attempt is normal;
it'll retry. Watch for these specific failure signatures:

| Signature | Meaning |
|---|---|
| `Dolt server unreachable at 127.0.0.1:0` | City `metadata.json` lacks `dolt_server_host`/`dolt_server_port`. Step P2.2 didn't take. |
| `PROJECT IDENTITY MISMATCH` | Step P2.3 incomplete on this rig. |
| `dolt circuit breaker is open` | `gc` backs off after repeated Dolt failures (5s cooldown). Symptomatic of an upstream issue, not the cause. |

### Step P2.5 — Address data divergence (BEFORE retiring gt)

> [!WARNING]
> **This is the migration's biggest landmine.** While both supervisors
> were running, work happened on both Dolts and they've diverged.

Common divergences:

- **Closed beads** — work that landed on one Dolt isn't visible to the
  other. Polecats may try to redo done work.
- **Convoy state** — open convoys may exist on one side and not the
  other.
- **New beads** — any beads created post-adoption only exist on the
  side where they were created.

#### P2.5.0 — Audit divergence first

Query both Dolts directly (gt's `bd` may be broken from earlier
project_id rescues):

```bash
# gt 3307 (direct dolt CLI bypasses bd):
for db in <db1> <db2> <db3>; do
  echo "=== $db (gt 3307) ==="
  dolt --data-dir ~/gt/.dolt-data sql -q \
    "USE $db; SELECT COUNT(*) AS n, MAX(updated_at) FROM issues"
done

# gc 36910 (use bd from a stripped env to bypass any caller's overrides):
env -i HOME=$HOME PATH=$PATH bash -c '
  cd ~/my-city
  for db in <db1> <db2> <db3> my_city; do
    echo "=== $db (gc 36910) ==="
    bd sql "SELECT COUNT(*) AS n, MAX(updated_at) FROM \`$db\`.issues"
  done'
```

Compare. If the gap is small and reproducible, option A is fine. If
either side has hundreds of historical beads the other doesn't,
options B or C are required.

#### P2.5.1 — Three options

**A. Cut and accept divergence** (fastest, but lossy). Pick one Dolt
as canonical (gt's 3307 if you've been working through gt; gc's 36910
if you've been working through gc). Throw away the other side's recent
state. Acceptable if recent work was minimal or all reproducible.

**B. Sync gt → gc in place** (RECOMMENDED for most migrations).
Replace gc's stale rig-DB snapshots with gt's full history while
preserving gc's own city DB. Procedure below.

**C. Use-external instead of use-managed.** Point gc at gt's 3307 —
gc becomes the sole supervisor but uses gt's existing Dolt as its data
store. gc's own 36910 Dolt becomes vestigial (can be stopped). Loses
any city-level beads that only exist on gc's 36910 — typically
supervisor scaffolding, but `gc supervisor run` may have been writing
real city-level state for hours/days. Verify before choosing.

#### P2.5.2 — Procedure for option B (in-place sync)

```bash
# 1. Clean stale gt-side state first (so it doesn't pollute the sync).
#    With gt's bd potentially broken, use direct dolt SQL:
dolt --data-dir ~/gt/.dolt-data sql -q "
  USE hq;
  UPDATE issues SET status='closed', closed_at=NOW(), updated_at=NOW()
  WHERE id IN ('<stale-id-1>','<stale-id-2>');"
# Identify staleness via:
#   SELECT id,title,status,updated_at FROM issues
#   WHERE status='open' ORDER BY updated_at DESC

# 2. Quiesce ALL writers — gc supervisor + every Dolt instance:
systemctl --user stop gascity-supervisor.service
kill $(cat ~/my-city/.gc/runtime/packs/dolt/dolt.pid)
# Also stop gt's Dolt + daemon:
gt down --all  ||  (gt dolt stop && pkill -f "gt daemon")
# Stop any orphan Dolts (e.g., a half-built old city's Dolt):
ss -tlnp | grep dolt    # find any remaining
# Kill PIDs as needed; never `rm -rf` Dolt data dirs.

# 3. Backup gc's data dir (cheap insurance):
cp -r ~/my-city/.beads/dolt /tmp/my-city-dolt.bak.$(date +%s)

# 4. Replace gc's rig databases with gt's, leaving gc's own city DB
#    and `__gc_probe` UNTOUCHED:
for db in <db1> <db2> <db3>; do      # adjust to your rigs
  rm -rf ~/my-city/.beads/dolt/$db
  cp -r  ~/gt/.dolt-data/$db ~/my-city/.beads/dolt/$db
done

# 5. Realign metadata.json files. After the sync, gc will see each
#    DB's stored _project_id (gt's UUID), not gc's previous gc-local-*
#    stamp. Revert metadata in EVERY file that points at that DB —
#    multiple rigs may share a DB (e.g., town root + a deacon rig +
#    deacon's worktrees all point at hq). Set project_id back to the
#    UUID stored in the DB itself.

# 6. Restart gc supervisor:
systemctl --user start gascity-supervisor.service

# 7. Watch reconciliation:
tail -F ~/.gc/supervisor.log
```

> [!IMPORTANT]
> **Key invariant:** copy specific subdirectories (`<db1>`, `<db2>`,
> etc.), NOT the entire `dolt/` directory. Overwriting the whole dir
> kills your city DB (gc's own city beads — often thousands) and
> `__gc_probe` (gc's health-check DB).

#### P2.5.3 — What "good" looks like after sync

After supervisor restart, you should see in `~/.gc/supervisor.log`:

- `Launching city '<name>' (...)` then `City started.`
- `session lifecycle: op=start ... outcome=success` for mayor, boot,
  deacon, witness, refinery, control-dispatchers
- NO `MISMATCH`, NO `Dolt server unreachable`, NO `circuit breaker`
  lines

`gc session list` should show all named sessions transitioning from
`creating` → `active`. `tmux -L <city-name> list-sessions` should show
the corresponding tmux sessions (they live on the city's tmux socket,
**not** the default tmux socket).

A `bd list` from any rig should show the full historical bead set you
copied from gt, including closed historical work.

If you see a new error class `unknown provider: "<name>"` — that's a
provider definition gap, not a data sync problem. See
[Step P2.6.0](#step-p260--define-any-non-built-in-providers-first).

### Step P2.5.5 — Bump `session.startup_timeout` BEFORE spawning anything

> [!IMPORTANT]
> **This is the most impactful single setting** in a `gc` city using
> opencode-family or any other slow-cold-start provider, and the
> default will quietly destroy your migration if you skip it.

Add to `city.toml`:

```toml
[session]
# Default 60s. Too short for opencode + vLLM cold-loads, which
# routinely take >60s for first-token. The runtime adapter wraps
# doStartSession in context.WithTimeout(ctx, startupTimeout) and walks
# pre_start → waitForReady → runSessionSetup → step6:nudge →
# runSessionLive. When the deadline fires inside waitForReady, the
# flow returns early at one of the ctx.Err() guards and NEVER REACHES
# THE NUDGE STEP.
startup_timeout = "240s"
```

**Why this matters more than it looks:** opencode (and other providers
with `PromptMode = "none"`) deliver their first-turn prompt **via the
post-readiness nudge step**. If the timeout fires before the nudge
runs, the agent loads its TUI but never receives a prompt — it sits
idle forever at "Ask anything..." and eventually gets reaped without
doing work.

Symptoms of skipping this step:

- `outcome=deadline_exceeded duration=1m4s` for nearly every named
  session in the supervisor log
- Polecats spawn (you see them in `tmux -L <city> list-sessions`)
- Opencode TUI loads with the right model
- BUT polecats never run `gc hook`, never claim routed beads, never
  produce work

After the change, you should see `outcome=success` with longer
durations (1m13s, 1m15s, 2m48s in our migration). Restart the
supervisor to apply: `systemctl --user restart
gascity-supervisor.service`.

### Step P2.5.6 — Patch: opencode nudge submission (gc v1.0.0 source fix)

After Step P2.5.5, sessions reach `outcome=success` but you may hit a
second-stage symptom: the nudge text appears in opencode's input field
but never gets submitted. The polecat sits at the prompt with the
nudge typed but unentered.

**Root cause** (verified via source dive): in
`internal/runtime/tmux/tmux.go`, `shouldSendEscapeBeforeEnter`
recognizes `claude`, `codex`, `gemini` as no-escape providers but
**not opencode**. For opencode polecats, `gc` sends an `Escape`
between text and Enter. Opencode treats `Escape` as "exit insert
mode" — the typed text gets discarded, so the subsequent `Enter` does
nothing.

**Fix is two source edits**, requires a local rebuild of `gc`:

```go
// internal/runtime/tmux/tmux.go (~line 1480)
func (t *Tmux) shouldSendEscapeBeforeEnter(target string) bool {
    provider, err := t.GetEnvironment(target, "GC_PROVIDER")
    if err == nil {
        p := strings.TrimSpace(provider)
        switch p {
        case "claude", "codex", "gemini", "opencode":      // ← add "opencode"
            return false
        default:
            // Custom alias inheriting from opencode (e.g., "opencode-qwen3")
            // — opencode treats Escape as "exit insert mode", which clears
            // the typed nudge text before Enter fires.
            if strings.HasPrefix(p, "opencode") {           // ← new fall-through
                return false
            }
        }
    }
    if t.targetLooksLikeNoEscapeProvider(target) {
        return false
    }
    return true
}

// internal/runtime/tmux/tmux.go (~line 1495)
func (t *Tmux) targetLooksLikeNoEscapeProvider(target string) bool {
    noEscapeProviders := []string{"claude", "codex", "gemini", "opencode"}  // ← add "opencode"
    return t.targetLooksLikeAnyProvider(target, noEscapeProviders...)
}
```

**Build + install:**

```bash
cd ~/gascity && make build      # produces bin/gc
cp bin/gc ~/.local/bin/gc       # ~/.local/bin must precede /usr/local/bin in PATH
```

**Wire systemd to use the patched binary** (the supervisor unit
hard-codes `/usr/local/bin/gc`):

```bash
mkdir -p ~/.config/systemd/user/gascity-supervisor.service.d
cat > ~/.config/systemd/user/gascity-supervisor.service.d/override.conf <<'EOF'
[Service]
ExecStart=
ExecStart=%h/.local/bin/gc supervisor run
EOF
systemctl --user daemon-reload
systemctl --user restart gascity-supervisor.service
```

**Verify** by routing a test bead via your opencode-family provider
and watching it reach `status=closed` autonomously:

```bash
ID=$(bd create "nudge fix verification" -t task -p P3 --json | jq -r '.[0].id')
bd update "$ID" --set-metadata gc.routed_to=<rig>/gastown.polecat
# Wait ~2-5 minutes; bd show "$ID" should show CLOSED with polecat-written notes
```

> [!NOTE]
> **Maintenance burden:** the fix lives in your local gc tree. On
> every gc upgrade you'll need to re-apply (or carry the patch on a
> fork branch). File this as an issue upstream so the fork window is
> short.

**Fallback if you don't want to maintain a local build:** route work
to `<rig>/claude` instead of `<rig>/gastown.polecat`. Claude has
`PromptMode = "arg"` so its prompt rides on argv at exec time and
isn't dependent on the nudge step. You lose the cost benefit of
cheaper opencode-family providers but regain autonomy without a code
patch.

### Step P2.6.0 — Define any non-built-in providers FIRST

> [!IMPORTANT]
> **This is the missing step that bites everyone.** `gc`'s built-in
> providers are bare names: `claude`, `codex`, `opencode`, `gemini`,
> etc. If you reference a custom provider name like `opencode-qwen3`
> in any agent patch and DON'T define it, `gc` silently skips that
> agent at reconcile time with a `unknown provider` warning, and the
> corresponding named session never spawns. The skip is silent if you
> only watch `tmux list-sessions` — you have to read the supervisor
> log to see it.

Define custom providers via `[providers.<name>]` blocks in
`city.toml`. The simplest form inherits from a built-in:

```toml
# city.toml — extend the built-in 'opencode' provider
[providers.opencode-qwen3]
base = "opencode"
display_name = "OpenCode (qwen3-coder-next via vLLM)"
```

That's it — model selection happens via the underlying provider's own
config (e.g., `~/.config/opencode/opencode.json`'s top-level `model`
key for opencode). If you need explicit args (e.g., env vars, custom
flags), add them:

```toml
[providers.opencode-qwen3]
base = "opencode"
args_append = ["--variant", "high"]
```

`base` resolution semantics:

| Form | Looks up |
|---|---|
| `"<name>"` | Custom first (excluding self), then built-in |
| `"builtin:<name>"` | Force built-in |
| `"provider:<name>"` | Force custom |

Verify with `gc config explain` after editing — entries with unknown
providers will appear as `agent "<name>": unknown provider: "<X>"`
warnings in the trace log at
`~/my-city/.gc/runtime/session-reconciler-trace/`.

### Step P2.6.5 — Two non-obvious facts about rig-scoped agent patches

These tripped us up; encode them so future migrations don't
re-discover.

1. **Each rig must declare `[rigs.imports.<pack>]`** for rig-scoped
   pack agents (witness, refinery, polecat) to be instantiated. The
   workspace-level `[imports.<pack>]` only expands city-scoped agents
   (mayor, boot, deacon). Without per-rig imports, `gc config explain`
   shows no polecat/witness/refinery for any rig.
2. **Rig patches use the bare agent name** (`agent = "polecat"`), NOT
   the prefixed form (`agent = "gastown.polecat"`). `gc` resolves it
   relative to the rig's imported pack scope. **City-scoped patches
   use the prefixed form** (`name = "gastown.mayor"`). Inconsistent
   but firm.

After applying both, `gc config explain` lists the agents as
`<rig>/gastown.polecat`, etc. — i.e., the prefix IS preserved in the
final identity, just not in the patch's `agent` field.

### Step P2.6 — Add per-role provider patches

If your gt setup had a `role_agents` map (e.g., mayor on Claude,
others on a cheaper provider), recreate it in `city.toml`. There is no
`role_agents` key in `gc` — use `[[patches.agent]]` for city-scoped
agents and `[[rigs.patches]]` per rig for rig-scoped (see
[Step P2.6.5](#step-p265--two-non-obvious-facts-about-rig-scoped-agent-patches)
for the naming gotchas):

```toml
# city.toml — patch the city-scoped mayor
[[patches.agent]]
name = "gastown.mayor"      # PREFIXED: city-scoped patches use full name
provider = "claude"

[[patches.agent]]
name = "gastown.boot"
provider = "opencode-qwen3"

[[patches.agent]]
name = "gastown.deacon"
provider = "opencode-qwen3"

# Per-rig — must include [rigs.imports.<pack>] AND patches use BARE name
[[rigs]]
name = "<rig>"

[rigs.imports.gastown]      # REQUIRED to expand rig-scoped pack agents
source = "packs/gastown"

[[rigs.patches]]
agent = "polecat"           # BARE: no `gastown.` prefix at rig scope
provider = "opencode-qwen3"

[[rigs.patches]]
agent = "witness"
provider = "opencode-qwen3"

[[rigs.patches]]
agent = "refinery"
provider = "opencode-qwen3"
```

Repeat the entire per-rig block for each rig. There is no city-wide
"all rig agents use X" override.

Run `gc config explain | awk '/^Agent: /{n=$2} /provider/{print n,$0}'`
to verify resolution. You should see entries like
`<rig>/gastown.polecat   provider = opencode-qwen3`.

### Step P2.7 — Stop gt daemon, validate gc-only operation

```bash
gt down --all
```

Watch `gc`'s supervisor for ~10 minutes. Verify:

- All expected named sessions are alive (`gc session list`)
- A test bead routed via `bd update --set-metadata
  gc.routed_to=<rig>/polecat` spawns a polecat that completes
  (`gc runtime drain-ack`) and exits cleanly
- mayor mailbox is reachable (`gc mail inbox`)

### Step P2.8 — Cleanup

After ~1 week of stable gc-only operation:

- Disable gt's daemon at the systemd/init level (don't auto-start)
- Run `gc doctor --fix` against the city to address any remaining
  custom-types or other warnings.

> [!NOTE]
> `--fix` may reformat `city.toml` (sorts blocks, normalizes
> indentation). Content is preserved.

#### Deregistering an orphan city

If `gc cities` shows a half-built earlier city (e.g., `~/gas-city/`)
alongside your real one, the supervisor will keep its Dolt alive at
every reconcile cycle — burning resources and cluttering logs.

> [!IMPORTANT]
> There is no `gc cities remove` command as of v1.0.0. The procedure:

1. **Edit `~/.gc/cities.toml`** and delete the entire `[[cities]]`
   block for the orphan. Leave only the city you actually use.
2. **Restart the supervisor:**
   ```bash
   systemctl --user restart gascity-supervisor.service
   ```
   On startup it re-reads `cities.toml`, stops the orphan's Dolt
   automatically, and only supervises the surviving city.
3. **Verify:** `ss -tlnp | grep dolt` should show only your live
   city's Dolt port. `gc cities` should list only the survivor.

> [!WARNING]
> **Do not just `kill` the orphan Dolt** without deregistering — the
> supervisor will respawn it within seconds on its next reconcile
> cycle. The deregister-then-restart sequence is the only stable way.

If you also want to delete the orphan city's data on disk: `rm -rf
~/gas-city/` (the directory) is safe AFTER deregistration. The Dolt
data inside `~/gas-city/.beads/dolt/` was never the canonical store
for any of your real rigs (those lived on gt's 3307 / now on gc's
36910), so deleting it loses no production data — just supervisor
scaffolding. Always verify by inspecting `bd list` from the orphan
city's `.beads/` first if there's any doubt.

## Final verification checklist

Run these AFTER Phase 2 is complete. All should pass:

```bash
# 1. Single Dolt instance
ss -tlnp | grep dolt | wc -l    # → 1

# 2. gt daemon stopped
pgrep -af "gt daemon"           # → empty

# 3. gc supervisor active
systemctl --user is-active gascity-supervisor.service   # → active

# 4. gc doctor clean
cd ~/my-city && gc doctor 2>&1 | grep -E "^[0-9]+ failed"
                                # → "0 failed" or similar

# 5. mayor session live on the city's tmux socket
tmux -L <city-name> list-sessions | grep -i mayor       # → 1 session

# 6. Recent spawns successful (no deadline_exceeded)
tail -100 ~/.gc/supervisor.log | grep -oE "outcome=[a-z_]+" | sort | uniq -c
                                # → mostly outcome=success, no deadline_exceeded

# 7. Autonomous polecat dispatch (the real test)
ID=$(bd create "post-migration smoke test" -t task -p P3 --json | jq -r '.[0].id')
bd update "$ID" --set-metadata gc.routed_to=<rig>/<target>
# wait 2-5 min, then:
bd show "$ID"                   # → should be CLOSED with notes from the polecat
```

If all 7 pass, the migration is fully operational. You can retire
gt's daemon for good and operate gc-only going forward.

## Quick reference

| Task | Command |
|---|---|
| Find rig prefix | `grep prefix ~/gt/<rig>/.beads/metadata.json` |
| Stop Town | `gt down --all` |
| Adopt rig | `gc rig add <path> --adopt --prefix <existing> --city <city>` |
| List rigs | `gc --city <city> rig list` |
| Bring Town back | `gt start` |
| Migration log (detached) | `cat /tmp/migrate.log` |
| Read DB project_id (Issue A) | `bd sql "SELECT value FROM \`<db>\`.metadata WHERE \`key\` = '_project_id'"` |
| Restore Town routes (Issue B) | `git -C ~/gt checkout HEAD -- .beads/routes.jsonl` |
| Bypass missing formulas (Issue C) | add `--hook-raw-bead` to `gt sling` |
| Set city as its own bd endpoint | `cd <city> && gc beads city use-managed` |
| Set city to external Dolt (e.g., gt's) | `gc beads city use-external --host <h> --port <p>` |
| Read gc-Dolt's stored project_id | `cd <city> && bd sql "SELECT value FROM \`<db>\`.metadata WHERE \`key\` = '_project_id'"` |
| List supervisor-spawned sessions | `gc session list` (must be from city dir, or pass `--city`) |
| Tail supervisor log | `tail -F ~/.gc/supervisor.log` |
| Route a bead to a polecat (gc) | `bd update <bead> --set-metadata gc.routed_to=<rig>/polecat` |
| Polecat self-cleanup (gc) | `gc runtime drain-ack && exit` |
| Define a custom provider | add `[providers.<name>] base = "opencode"` to `city.toml` |
| List tmux sessions on a city's socket | `tmux -L <city-name> list-sessions` (NOT default socket) |
| Direct dolt query when bd is broken | `dolt --data-dir <data-dir> sql -q "USE <db>; SELECT ..."` |
| Bypass env BEADS_DOLT_PORT override | `env -i HOME=$HOME PATH=$PATH bash -c 'cd <city> && bd ...'` |
