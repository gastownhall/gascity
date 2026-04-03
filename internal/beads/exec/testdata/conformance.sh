#!/usr/bin/env bash
# conformance.sh — minimal file-backed bead store for exec protocol conformance testing.
#
# State lives in $BEADS_DIR (one JSON file per bead, plus a counter file).
# Dependencies: bash, jq.
#
# This script implements the full exec beads protocol so that
# beadstest.RunStoreTests can validate the exec.Store ↔ script contract
# without requiring any external bead provider (br, bd, etc.).
set -euo pipefail

op="${1:?usage: conformance.sh <operation> [args...]}"
shift

: "${BEADS_DIR:?BEADS_DIR must be set}"

# normalize_bead_output applies metadata reconstruction to a bead JSON object.
# Extracts meta:<key>=<value> labels into a .metadata map and removes them
# from .labels. Pipe a single bead JSON object through this filter.
normalize_bead_output() {
  jq "$JQ_NORMALIZE_BEAD"
}

# jq filter for metadata reconstruction — shared across all read paths.
# Extracts meta:<key>=<value> labels into .metadata and strips them from .labels.
# NOTE: "meta:" is a reserved label prefix used for metadata storage.
# Callers should not create labels starting with "meta:" — they will be
# silently consumed into .metadata on read.
JQ_NORMALIZE_BEAD='
  .metadata = ([.labels // [] | .[] | select(startswith("meta:")) | ltrimstr("meta:") | split("=") | {(.[0]): (.[1:] | join("="))}] | add // {})
  | .labels = [.labels // [] | .[] | select(startswith("meta:") | not)]
'

# next_id atomically increments the counter and prints the new ID.
next_id() {
  local counter_file="$BEADS_DIR/.counter"
  local n=0
  if [ -f "$counter_file" ]; then
    n=$(cat "$counter_file")
  fi
  n=$((n + 1))
  echo "$n" > "$counter_file"
  echo "cs-$n"
}

# now prints an RFC3339 timestamp.
now() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

# collect_beads prints all bead file paths sorted by numeric ID, one per line.
# Returns 1 if no beads exist.
collect_beads() {
  local found=()
  for f in "$BEADS_DIR"/cs-*.json; do
    [ -f "$f" ] && found+=("$f")
  done
  if [ ${#found[@]} -eq 0 ]; then
    return 1
  fi
  printf '%s\n' "${found[@]}" | sort -t- -k2 -n
}

case "$op" in
  create)
    input=$(cat)
    id=$(next_id)
    title=$(echo "$input" | jq -r '.title // ""')
    bead_type=$(echo "$input" | jq -r '.type // "task"')
    assignee=$(echo "$input" | jq -r '.assignee // ""')
    from=$(echo "$input" | jq -r '.from // ""')
    parent_id=$(echo "$input" | jq -r '.parent_id // ""')
    ref=$(echo "$input" | jq -r '.ref // ""')
    description=$(echo "$input" | jq -r '.description // ""')
    created_at=$(now)

    # Build labels array from input, including metadata as meta: labels.
    # Dedup: metadata keys take precedence over any caller-supplied meta: labels
    # with the same key prefix, matching the pattern used in update/set-metadata.
    labels=$(echo "$input" | jq -c '
      (.metadata // {}) as $meta |
      ($meta | keys | map("meta:\(.)=")) as $prefixes |
      [(.labels // [])[] | select(. as $l | $prefixes | any(. as $p | $l | startswith($p)) | not)]
      + [$meta | to_entries[] | "meta:\(.key)=\(.value)"]')
    # Build needs array from input.
    needs=$(echo "$input" | jq -c '.needs // []')

    # Write bead file.
    jq -n \
      --arg id "$id" \
      --arg title "$title" \
      --arg status "open" \
      --arg bead_type "$bead_type" \
      --arg created_at "$created_at" \
      --arg assignee "$assignee" \
      --arg from "$from" \
      --arg parent_id "$parent_id" \
      --arg ref "$ref" \
      --argjson needs "$needs" \
      --arg description "$description" \
      --argjson labels "$labels" \
      '{
        id: $id,
        title: $title,
        status: $status,
        type: $bead_type,
        created_at: $created_at,
        assignee: $assignee,
        from: $from,
        parent_id: $parent_id,
        ref: $ref,
        needs: $needs,
        description: $description,
        labels: $labels
      }' > "$BEADS_DIR/$id.json"

    # Output the created bead (normalized: meta: labels → .metadata map).
    normalize_bead_output < "$BEADS_DIR/$id.json"
    ;;

  get)
    id="$1"
    bead_file="$BEADS_DIR/$id.json"
    if [ ! -f "$bead_file" ]; then
      echo "bead $id not found" >&2
      exit 1
    fi
    normalize_bead_output < "$bead_file"
    ;;

  update)
    id="$1"
    bead_file="$BEADS_DIR/$id.json"
    if [ ! -f "$bead_file" ]; then
      echo "bead $id not found" >&2
      exit 1
    fi
    input=$(cat)
    current=$(cat "$bead_file")

    # Apply description if present (non-null).
    has_desc=$(echo "$input" | jq 'has("description") and .description != null')
    if [ "$has_desc" = "true" ]; then
      new_desc=$(echo "$input" | jq -r '.description')
      current=$(echo "$current" | jq --arg d "$new_desc" '.description = $d')
    fi

    # Apply parent_id if present (non-null).
    has_pid=$(echo "$input" | jq 'has("parent_id") and .parent_id != null')
    if [ "$has_pid" = "true" ]; then
      new_pid=$(echo "$input" | jq -r '.parent_id')
      current=$(echo "$current" | jq --arg p "$new_pid" '.parent_id = $p')
    fi

    # Apply assignee if present (non-null).
    has_assignee=$(echo "$input" | jq 'has("assignee") and .assignee != null')
    if [ "$has_assignee" = "true" ]; then
      new_assignee=$(echo "$input" | jq -r '.assignee')
      current=$(echo "$current" | jq --arg a "$new_assignee" '.assignee = $a')
    fi

    # Apply metadata if present: convert to meta:<key>=<value> labels,
    # removing any old labels for the same keys first to prevent duplicates.
    has_meta=$(echo "$input" | jq 'has("metadata") and .metadata != null and (.metadata | length > 0)')
    if [ "$has_meta" = "true" ]; then
      meta_labels=$(echo "$input" | jq -c '[.metadata | to_entries[] | "meta:\(.key)=\(.value)"]')
      meta_keys=$(echo "$input" | jq -c '[.metadata | keys[] | "meta:\(.)="]')
      current=$(echo "$current" | jq --argjson ml "$meta_labels" --argjson mk "$meta_keys" '
        .labels = ([.labels[] | select(. as $l | $mk | any(. as $prefix | $l | startswith($prefix)) | not)] + $ml)')
    fi

    # Append labels if present.
    new_labels=$(echo "$input" | jq -c '.labels // []')
    if [ "$new_labels" != "[]" ]; then
      current=$(echo "$current" | jq --argjson nl "$new_labels" '.labels = (.labels + $nl | unique)')
    fi

    echo "$current" > "$bead_file"
    ;;

  close)
    id="$1"
    bead_file="$BEADS_DIR/$id.json"
    if [ ! -f "$bead_file" ]; then
      echo "bead $id not found" >&2
      exit 1
    fi
    jq '.status = "closed"' "$bead_file" > "$bead_file.tmp" && mv "$bead_file.tmp" "$bead_file"
    ;;

  list)
    bead_files=$(collect_beads) || { echo "[]"; exit 0; }
    # shellcheck disable=SC2086
    jq -s "[.[] | $JQ_NORMALIZE_BEAD]" $bead_files
    ;;

  ready)
    bead_files=$(collect_beads) || { echo "[]"; exit 0; }
    # shellcheck disable=SC2086
    jq -s "[.[] | select(.status == \"open\") | $JQ_NORMALIZE_BEAD]" $bead_files
    ;;

  children)
    parent_id="$1"
    bead_files=$(collect_beads) || { echo "[]"; exit 0; }
    # shellcheck disable=SC2086
    jq -s --arg pid "$parent_id" \
      "[.[] | select(.parent_id == \$pid) | $JQ_NORMALIZE_BEAD]" $bead_files
    ;;

  list-by-label)
    label="$1"
    limit="${2:-0}"
    bead_files=$(collect_beads) || { echo "[]"; exit 0; }
    if [ "$limit" -gt 0 ] 2>/dev/null; then
      # shellcheck disable=SC2086
      jq -s --arg l "$label" --argjson lim "$limit" \
        "[.[] | select(.labels | index(\$l))] | .[:\$lim] | [.[] | $JQ_NORMALIZE_BEAD]" $bead_files
    else
      # shellcheck disable=SC2086
      jq -s --arg l "$label" \
        "[.[] | select(.labels | index(\$l))] | [.[] | $JQ_NORMALIZE_BEAD]" $bead_files
    fi
    ;;

  set-metadata)
    id="$1"
    key="$2"
    value=$(cat)
    bead_file="$BEADS_DIR/$id.json"
    if [ ! -f "$bead_file" ]; then
      echo "bead $id not found" >&2
      exit 1
    fi
    # Store metadata as a label: meta:<key>=<value>
    # Remove any existing label for this key first to prevent duplicates.
    meta_label="meta:${key}=${value}"
    meta_prefix="meta:${key}="
    jq --arg ml "$meta_label" --arg mp "$meta_prefix" '
      .labels = ([.labels // [] | .[] | select(startswith($mp) | not)] + [$ml])
    ' "$bead_file" > "$bead_file.tmp" && mv "$bead_file.tmp" "$bead_file"
    ;;

  mol-cook)
    # Composed in Go — signal unknown operation.
    exit 2
    ;;

  *)
    exit 2
    ;;
esac
