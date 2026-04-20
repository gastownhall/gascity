#!/bin/sh
# gc dolt cleanup — Find and remove orphaned Dolt databases.
#
# Discovers databases from the authoritative rig registry (all registered rigs,
# including external rigs outside GC_CITY_PATH). By default, lists orphaned
# databases (dry-run). Use --force to remove them.
# Use --max to set a safety limit (refuses if more orphans than --max).
#
# Environment: GC_CITY_PATH
set -e

force=false
max_orphans=50
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --force) force=true; shift ;;
    --max)   max_orphans="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt cleanup [--force] [--max N]"
      echo ""
      echo "Find Dolt databases not referenced by any registered rig."
      echo ""
      echo "Flags:"
      echo "  --force    Actually remove orphaned databases"
      echo "  --max N    Refuse if more than N orphans (default: 50)"
      exit 0
      ;;
    *) echo "gc dolt cleanup: unknown flag: $1" >&2; exit 1 ;;
  esac
done

if [ ! -d "$data_dir" ]; then
  echo "No orphaned databases found."
  exit 0
fi

# metadata_files() — discover databases from authoritative rig registry.
# Uses gc rig list --json when available (all rigs, including external).
# Falls back to filesystem glob when gc is unavailable (local rigs only).
# Outputs: pathnames of .beads/metadata.json files (space-safe).
metadata_files() {
  printf '%s\n' "$GC_CITY_PATH/.beads/metadata.json"

  if command -v gc >/dev/null 2>&1; then
    rig_paths=$(gc rig list --json 2>/dev/null \
      | if command -v jq >/dev/null 2>&1; then
          jq -r '.rigs[].path' 2>/dev/null
        else
          grep '"path"' | sed 's/.*"path": *"//;s/".*//'
        fi) || true
    if [ -n "$rig_paths" ]; then
      printf '%s\n' "$rig_paths" | while IFS= read -r p; do
        [ -n "$p" ] && printf '%s\n' "$p/.beads/metadata.json"
      done
      return
    fi
  fi

  # Fallback: scan local rigs/ directory only. Cannot discover external rigs
  # when gc is unavailable — acceptable degradation.
  find "$GC_CITY_PATH/rigs" -path '*/.beads/metadata.json' 2>/dev/null || true
}

# Collect referenced database names from metadata.json files.
referenced=""
while IFS= read -r meta; do
  [ -z "$meta" ] && continue
  [ -f "$meta" ] || continue
  db=$(grep -o '"dolt_database"[[:space:]]*:[[:space:]]*"[^"]*"' "$meta" 2>/dev/null | sed 's/.*"dolt_database"[[:space:]]*:[[:space:]]*"//;s/"//' || true)
  [ -n "$db" ] && referenced="$referenced $db "
done <<EOF
$(metadata_files)
EOF

# Find orphans.
orphans=""
orphan_count=0
for d in "$data_dir"/*/; do
  [ ! -d "$d/.dolt" ] && continue
  name="$(basename "$d")"
  case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|__gc_probe) continue ;; esac
  case "$referenced" in
    *" $name "*) continue ;; # referenced, not orphan
  esac
  # Calculate size.
  size_bytes=$(du -sb "$d" 2>/dev/null | cut -f1 || echo 0)
  if [ "$size_bytes" -ge 1073741824 ]; then
    size=$(awk "BEGIN {printf \"%.1f GB\", $size_bytes/1073741824}")
  elif [ "$size_bytes" -ge 1048576 ]; then
    size=$(awk "BEGIN {printf \"%.1f MB\", $size_bytes/1048576}")
  elif [ "$size_bytes" -ge 1024 ]; then
    size=$(awk "BEGIN {printf \"%.1f KB\", $size_bytes/1024}")
  else
    size="${size_bytes} B"
  fi
  orphans="$orphans$name|$size|$d
"
  orphan_count=$((orphan_count + 1))
done

if [ "$orphan_count" -eq 0 ]; then
  echo "No orphaned databases found."
  exit 0
fi

# Precompute the non-HQ allowlist once from `gc rig list --json`. This lets us
# fail closed if the registry query or jq parse fails at runtime (not just if
# the binaries are missing), and avoids spawning N subprocess pairs for N
# orphans. The allowlist file is empty iff no non-HQ rigs are registered —
# distinguished from a *failed* query, which exits before any delete runs.
#
# compute_allowlist_file — write one non-HQ rig path per line to $1, or fail
# with exit 1 if the pipeline can't be completed.
compute_allowlist_file() {
  _out=$1
  if ! command -v gc >/dev/null 2>&1; then
    echo "gc dolt cleanup: gc not found on PATH; cannot evaluate rig overlap allowlist" >&2
    return 1
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "gc dolt cleanup: jq not found on PATH; cannot evaluate rig overlap allowlist" >&2
    echo "install jq or remove orphans manually" >&2
    return 1
  fi
  _list=$(gc rig list --json 2>/dev/null) || {
    echo "gc dolt cleanup: gc rig list --json failed; refusing to run overlap allowlist unverified" >&2
    return 1
  }
  if ! printf '%s\n' "$_list" | jq -e '.rigs' >/dev/null 2>&1; then
    echo "gc dolt cleanup: gc rig list --json produced unparseable output; refusing to run overlap allowlist unverified" >&2
    return 1
  fi
  printf '%s\n' "$_list" | jq -r '.rigs[] | select(.hq != true) | .path' > "$_out" || return 1
}

# overlapping_rig_path — emit the non-HQ rig path from $allowlist_file that
# overlaps $1, or nothing if no overlap. Strips trailing slashes so
# `$data_dir/*/` glob output (always ending in `/`) matches against registry
# paths (no trailing slash).
overlapping_rig_path() {
  _db_path=${1%/}
  while IFS= read -r rig_path; do
    [ -z "$rig_path" ] && continue
    rig_path=${rig_path%/}
    # Exact equality, db under rig, or rig under db.
    if [ "$_db_path" = "$rig_path" ] \
      || case "$_db_path" in "$rig_path/"*) true ;; *) false ;; esac \
      || case "$rig_path" in "$_db_path/"*) true ;; *) false ;; esac
    then
      printf '%s\n' "$rig_path"
      return
    fi
  done < "$allowlist_file"
}

# Build the allowlist. Under --force, failure aborts before any rm -rf.
# Under dry-run, failure degrades to "no annotations" — we still print the
# table so operators can see what exists.
allowlist_file=$(mktemp)
trap 'rm -f "$allowlist_file" "${refused_tmp:-}"' EXIT
allowlist_ready=true
if ! compute_allowlist_file "$allowlist_file"; then
  allowlist_ready=false
  if [ "$force" = true ]; then
    exit 1
  fi
  : > "$allowlist_file"  # empty → no overlap annotations in dry-run
fi

# Print orphan table. Under dry-run, annotate entries that --force would refuse
# so users can preview refusals without running the destructive command.
printf "%-30s  %-12s  %s\n" "NAME" "SIZE" "STATUS"
echo "$orphans" | while IFS='|' read -r name size path; do
  [ -z "$name" ] && continue
  status=""
  if [ "$force" != true ] && [ "$allowlist_ready" = true ]; then
    overlap=$(overlapping_rig_path "$path")
    [ -n "$overlap" ] && status="refused: overlaps rig at $overlap"
  fi
  printf "%-30s  %-12s  %s\n" "$name" "$size" "$status"
done

# Safety limit.
if [ "$orphan_count" -gt "$max_orphans" ]; then
  echo "" >&2
  echo "gc dolt cleanup: $orphan_count orphans exceeds --max $max_orphans; remove manually or increase --max" >&2
  exit 1
fi

if [ "$force" != true ]; then
  echo ""
  echo "$orphan_count orphaned database(s). Use --force to remove."
  exit 0
fi

# Remove each orphan. Track refusals and successful removals via tmpfiles so
# the subshell's counters survive (the pipe creates a subshell).
refused_tmp=$(mktemp)
removed_tmp=$(mktemp)
trap 'rm -f "$allowlist_file" "$refused_tmp" "$removed_tmp"' EXIT
echo "$orphans" | while IFS='|' read -r db_name size path; do
  [ -z "$db_name" ] && continue

  # Allowlist safety check: refuse if path overlaps any registered rig.
  # Exclude HQ: HQ's path is the city root; the managed data-dir (.beads/dolt/) is
  # always a subdirectory of it. Including HQ would refuse every orphan at the default
  # data-dir location. Only non-HQ rig paths need the overlap guard.
  overlap=$(overlapping_rig_path "$path")
  if [ -n "$overlap" ]; then
    echo "refusing to remove '$db_name': path overlaps registered rig at '$overlap'" >&2
    echo "refused" >> "$refused_tmp"
    continue
  fi

  if rm -rf "$path"; then
    echo "removed" >> "$removed_tmp"
    echo "  Removed $db_name"
  else
    echo "  Failed to remove $db_name" >&2
  fi
done

# Count removed and refused (the removal loop runs in a subshell, so the
# parent shell reads back through the tmpfiles).
removed=$(wc -l < "$removed_tmp" | tr -d ' ')
refused_count=$(wc -l < "$refused_tmp" | tr -d ' ')
echo ""
echo "Removed $removed of $orphan_count orphaned database(s)."

# Exit non-zero if any orphan was refused or failed to remove.
if [ "$removed" -lt "$((orphan_count - refused_count))" ] \
  || { [ "$refused_count" -gt 0 ] && [ "$removed" -eq 0 ]; }; then
  exit 1
fi
