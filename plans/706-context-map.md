## Context Map — ga-dez (Fix dolt cleanup external-rig handling)

### Primary Files to Modify

- `examples/dolt/commands/cleanup.sh` — primary target. Lines 41–45: the
  filesystem glob `"$GC_CITY_PATH"/rigs/*/.beads/metadata.json` that misses
  external rigs registered with paths outside `GC_CITY_PATH`. Must be replaced
  with a `metadata_files()` helper mirroring `health.sh` (using `gc rig list
  --json` with filesystem fallback). Lines 98–108: the `rm -rf "$path"` loop
  that needs an allowlist safety check before deletion.

### Key Reference: health.sh Pattern (Applied Twice)

Commit `7f69e81e` ("Fix dolt health orphan detection for external rigs",
2026-03-23) first introduced `metadata_files()` using `gc config show` TOML
parsing. Commit `d8b2b616` ("fix: use gc rig list --json for health metadata
discovery", 2026-03-29) upgraded it to use `gc rig list --json` (which also
added `--json` to `gc rig list`) and fixed several bugs:
- `gc config show` TOML parsing didn't handle external rigs from the global rig
  index
- Used `for meta in $(metadata_files)` which word-splits on paths with spaces;
  fixed by writing to a tmpfile and reading with `while IFS= read -r`
- `metadata_db()` had `jq` calls that could fail under `set -e`; fixed with
  `|| true`
- Fallback `find` had wrong depth (`-maxdepth 2` never matched depth-3 paths)

The `health.sh` `metadata_files()` function (lines 15–36) is the exact
pattern to port to `cleanup.sh`.

### Test Files

**Existing tests to understand / extend:**

- `cmd/gc/cmd_rig_test.go` — unit tests for `doRigList`, including
  `TestDoRigList_WithRigs`, `TestDoRigList_Empty`, `TestDoRigListShowsSuspended`.
  Reference these for how `doRigList` is exercised with external rig paths.
- `cmd/gc/main_test.go` (lines 679–741) — `TestDoRigListConfigLoadFails`,
  `TestDoRigListSuccess`, `TestDoRigListJSON`. Shows the `RigListJSON` struct
  serialization pattern.
- `test/acceptance/rig_test.go` — acceptance tests for `gc rig list --json`
  (lines 68–77). Contains `createGitRig()` helper for making external git rigs.

**New tests needed:**

1. Shell-level tests for `cleanup.sh` exercising:
   - External rig databases are NOT classified as orphans when the rig is
     registered (primary bug scenario)
   - The allowlist safety check refuses to delete databases matching a
     registered rig's prefix
   - Databases with no matching rig ARE classified as orphans
   - Fallback path (when `gc` unavailable) still works for local rigs

   These tests would follow the `TestPackCommandExecution` pattern in
   `cmd/gc/cmd_pack_commands_test.go` (a Go test that runs the shell script
   directly via `runPackCommand` with a controlled filesystem).

2. Alternative: shell test scripts alongside `cleanup.sh`, similar to how
   health.sh's behavior is verified in acceptance tests.

### Related Code (Similar Patterns)

- `examples/dolt/commands/health.sh` — the reference implementation with
  `metadata_files()` helper and tmpfile caching pattern. Lines 15–36 define
  the exact pattern to port. Lines 89–95 show the tmpfile cache setup.
- `examples/dolt/commands/list.sh` — reads existing Dolt databases; shows
  what the database directory looks like from the filesystem side.
- `examples/dolt/scripts/runtime.sh` — defines `DOLT_DATA_DIR`, `GC_CITY_PATH`;
  sourced by all dolt commands.

### Dependencies and Callers

**Who calls cleanup.sh:**
- `examples/dolt/formulas/mol-dog-stale-db.formula.toml` (step "cleanup",
  line 101) — calls `gc dolt cleanup --force --max <count>` when orphan count
  exceeds SQL threshold
- `examples/dolt/formulas/mol-dog-doctor.formula.toml` (step "inspect",
  line 99) — recommends `gc dolt cleanup` to the dog agent
- `examples/gastown/packs/gastown/formulas/mol-deacon-patrol.formula.toml`
  (line 280) — runs `gc dolt cleanup` in patrol step
- `examples/gastown/packs/maintenance/prompts/dog.md.tmpl` (lines 117–118) —
  documents the command to dog agent
- `examples/gastown/packs/gastown/prompts/shared/operational-awareness.md.tmpl`
  (line 38) — instructs agents to use `gc dolt cleanup`

**The `gc rig list` plumbing:**
- `cmd/gc/cmd_rig.go` — `doRigList()` (line 496), `RigListJSON` struct (line
  477), `RigListItem` struct (line 484). The `--city` flag is a persistent
  root flag (set in `cmd/gc/main.go` line 94), so `gc rig list --json --city
  $GC_CITY_PATH` works without changes to the Go code.
- `cmd/gc/pack_commands.go` — `runPackCommand()` (line 153): sets
  `GC_CITY_PATH` env var before running any pack script. So `cleanup.sh` has
  `GC_CITY_PATH` available for the `gc rig list --json --city "$GC_CITY_PATH"` call.

### Key Insights

1. **No Go changes needed.** The `--city` flag is already a persistent root
   flag. `gc rig list --json --city "$GC_CITY_PATH"` works as-is. The fix is
   entirely in `cleanup.sh`.

2. **The exact fix pattern from health.sh (commit d8b2b616):**
   - Extract a `metadata_files()` function that calls `gc rig list --json
     --city "$GC_CITY_PATH"` and pipes through `jq -r '.rigs[].path'` (with
     `grep '"path"'` fallback when jq is absent)
   - Write results to a tmpfile (`mktemp`), trap cleanup on EXIT
   - Replace the glob on line 43 with a `while IFS= read -r meta; do ... done
     < "$_meta_cache"` loop
   - Add a `metadata_db()` helper with `|| true` guards for `set -e` safety
   - Include a filesystem fallback: `find "$GC_CITY_PATH/rigs" -path
     '*/.beads/metadata.json' 2>/dev/null || true`

3. **Defense-in-depth: allowlist check before rm -rf.** The issue requests a
   rig-prefix allowlist check before any `rm -rf`. This means: before deleting
   a database directory, verify its name does not match any registered rig's
   database name (derived from the rig's metadata). If it matches, skip with a
   warning instead of deleting. This is a second layer beyond the orphan
   detection logic.

4. **The "fixed twice with same approach" note** refers to commits `7f69e81e`
   (first fix using `gc config show`) and `d8b2b616` (second fix switching to
   `gc rig list --json`). Both applied to `health.sh`. The lesson: `cleanup.sh`
   needs the d8b2b616 approach from the start, not the intermediate approach.

5. **Word-splitting hazard**: The original `cleanup.sh` loop `for meta in
   "$GC_CITY_PATH"/.beads/metadata.json "$GC_CITY_PATH"/rigs/*/.beads/metadata.json`
   will word-split on paths with spaces. The tmpfile + `while read` pattern
   from `health.sh` fixes this.

### Risk Assessment

- **No merge conflicts expected.** Only `cleanup.sh` is being modified; no
  adjacent PRs touch it (confirmed by bead complexity triage: "low churn, 4
  commits to cleanup.sh").
- **Downstream formula behavior unchanged.** The formulas call `gc dolt cleanup
  --force` unchanged; only the internal database discovery logic changes.
- **Fallback path is critical.** In environments where `gc` is not on PATH
  (unusual but possible), the fallback `find` scan must still catch local-only
  rigs. This is the "acceptable degradation" note in health.sh line 33–35.
- **The `jq` optional dependency pattern.** Both jq and grep-based extraction
  must be maintained for the JSON parsing, as jq is optional. Health.sh lines
  19–24 show the pattern.
- **POSIX shell (`#!/bin/sh`).** cleanup.sh uses `#!/bin/sh` not bash.
  All constructs must be POSIX-compliant. The `while IFS= read -r` pattern
  is POSIX safe.

### Files NOT Needing Changes

- `cmd/gc/cmd_rig.go` — `gc rig list --json` already supports `--city` via
  the persistent root flag; no new Go code needed.
- Formula TOML files — callers don't change.
- `examples/dolt/scripts/runtime.sh` — sets `DOLT_DATA_DIR` and sources
  correctly; no change needed.
