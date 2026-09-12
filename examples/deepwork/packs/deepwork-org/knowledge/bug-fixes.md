# Bug Fixes & Patterns — Learned from Usage

These are bugs discovered and fixed through the self-improving loop.
Every fix here prevents the same bug from hitting another node.

## Dolt CSV Output

**Bug:** `dolt sql -q "SELECT ..." -r csv | tail -1` returns the column header when query returns 0 rows.
**Fix:** Always use `tail -n +2 | head -1` to skip the CSV header row.
**Applies to:** Every script that queries DoltHub via `dolt sql -r csv`.

## Dolt Diff Grep Pattern

**Bug:** `dolt diff --staged --stat` outputs "Row Modified" (capitalized), but grep pattern was `grep -q "rows"` (lowercase plural).
**Fix:** Use `grep -qi "row"` (case-insensitive, singular).
**Applies to:** mesh-sync.sh and any script checking for staged changes.

## set -e With Dolt Commands

**Bug:** `set -e` causes the entire script to exit when a dolt command returns non-zero for benign reasons (e.g., `dolt pull` with nothing to pull, `grep` finding no matches in a pipe).
**Fix:** Remove `set -e` from mesh scripts. Handle errors explicitly with `|| true` or `if` checks.
**Applies to:** All mesh scripts that call dolt.

## Dirty State Before Pull

**Bug:** `dolt pull` fails with "cannot merge with uncommitted changes" when the previous sync left staged changes (e.g., heartbeat update) without committing.
**Fix:** Always commit staged changes BEFORE pulling:
```bash
dolt add . 2>/dev/null
if dolt diff --staged --stat 2>/dev/null | grep -qi "row"; then
  dolt commit -m "mesh: pre-sync commit" --allow-empty 2>/dev/null || true
fi
dolt pull
```

## Knowledge Deduplication

**Bug:** `mesh-learnings.md` gets duplicate entries when both the graduate command and the sync pull write the same knowledge entry.
**Fix:** Before appending, check if the title already exists in the file:
```bash
if grep -qF "$title" "$LEARNINGS" 2>/dev/null; then
  continue  # already present
fi
```

## CSV Parsing With Commas in Content

**Bug:** `IFS=',' read` breaks when a field contains commas (e.g., skill descriptions).
**Fix:** Use `CONCAT(fields, '|')` with `IFS='|'` for list queries. For detail queries, fetch each field individually.

## Shell Function Override for gt

**Bug:** `gt` is a compiled Go binary that doesn't support plugin subcommands. Symlinks and PATH tricks don't work.
**Fix:** Add a shell function to `.bashrc` that intercepts `gt mesh` and routes to `gt-mesh`:
```bash
gt() {
  if [ "$1" = "mesh" ]; then shift; command gt-mesh "$@"
  else command gt "$@"; fi
}
```
