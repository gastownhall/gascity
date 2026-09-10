#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Conflicts: a bd store shared between cities (pushed and pulled through a
# hub) has the same automatic sweep run by every city — bd's defer wake flips
# an expired deferred `issues` row to open on every store that reads the
# ready front, and each store writes its own fresh row_lock and updated_at.
# The change is the same on both sides; only those two columns differ, and
# dolt reports the row as a conflict on the next pull (hw-ynz1w, 2026-09-10).
# The pull resolves exactly that class and nothing else: a conflicted
# `issues` row whose EVERY other column is equal on both sides takes the
# remote's row_lock and updated_at (so the other city's next pull of this
# merge is a fast-forward), inside one transaction dolt refuses to commit
# while any conflict remains. A row that differs in any other column, a
# conflict in any other table, or a schema change under the merge leaves the
# store exactly as it was and the pull fails with the rows listed for manual
# resolution. See resolve_benign_conflicts.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD,
# GC_DOLT_REMOTE_<DB> (select among multiple remotes), GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 (permit a non-file:// pull)
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"

db_filter=""
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --db) db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt pull [--db NAME]"
      echo ""
      echo "Pull Dolt databases from their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --db NAME   Pull only the named database"
      echo ""
      echo "Environment:"
      echo "  GC_DOLT_REMOTE_<DB>                Select which remote to pull from when a database has several"
      echo "  GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1   Permit pulling from a non-file:// remote"
      echo ""
      echo "Conflicts:"
      echo "  A conflicted bd issues row that differs from the remote ONLY in"
      echo "  row_lock and updated_at (the same automatic change written by two"
      echo "  cities, e.g. bd's defer wake) takes the remote's values and the pull"
      echo "  completes. Any other conflict leaves the database exactly as it was,"
      echo "  prints the conflicted rows, and fails the pull for manual resolution."
      exit 0
      ;;
    *) echo "gc dolt pull: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt pull: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

remote_env_value() {
  key=$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')
  case "$key" in *[!A-Z0-9_]*) return 0 ;; esac
  eval "printf '%s' \"\${GC_DOLT_REMOTE_$key:-}\""
}

remote_allow_env_value() {
  key=$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')
  case "$key" in *[!A-Z0-9_]*) return 0 ;; esac
  eval "printf '%s' \"\${GC_DOLT_PULL_ALLOW_REMOTE_$key:-}\""
}

# Pull's own remote-selection policy: refuse an ambiguous multi-remote db
# unless GC_DOLT_REMOTE_<DB> names one explicitly, and require
# GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 before honoring any selection — override
# or sole-remote default alike — that resolves to a non-local (non-file://)
# remote. The locality check applies regardless of how many candidates there
# are, including exactly one (ga-nht26j; mirrors the equivalent sync-side
# fix, ga-2w96wd, not yet on main — consolidating the two into a shared
# helper remains a future intent, not current behavior).
select_remote() {
  sel_db="$1"; sel_candidates="$2"
  [ -z "$sel_candidates" ] && return 0
  sel_count=$(printf '%s\n' "$sel_candidates" | grep -c '.')
  if [ "$sel_count" -le 1 ]; then
    sel_solo_name=${sel_candidates%%,*}
    sel_solo_url=${sel_candidates#*,}
    case "$sel_solo_url" in
      file://*) ;;
      *)
        sel_solo_allowed=$(remote_allow_env_value "$sel_db") || return 1
        if [ "$sel_solo_allowed" != "1" ]; then
          echo "  $sel_db: ERROR: sole configured remote '$sel_solo_name' is a non-local remote ($sel_solo_url) — set GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 to allow pulling from it" >&2
          return 1
        fi ;;
    esac
    printf '%s\n' "$sel_candidates"; return 0
  fi
  sel_names=$(printf '%s\n' "$sel_candidates" | awk -F, '{ if (o=="") o=$1; else o=o","$1 } END{print o}')
  sel_override=$(remote_env_value "$sel_db") || return 1
  if [ -z "$sel_override" ]; then
    echo "  $sel_db: ERROR: multiple remotes configured ($sel_names) — set GC_DOLT_REMOTE_<DB> to disambiguate" >&2
    return 1
  fi
  if ! valid_remote_name "$sel_override"; then
    echo "  $sel_db: ERROR: invalid GC_DOLT_REMOTE override: $sel_override" >&2
    return 1
  fi
  sel_match=$(printf '%s\n' "$sel_candidates" | awk -F, -v want="$sel_override" '$1 == want {print; exit}')
  if [ -z "$sel_match" ]; then
    echo "  $sel_db: ERROR: GC_DOLT_REMOTE names unknown remote '$sel_override' (available: $sel_names)" >&2
    return 1
  fi
  sel_url=${sel_match#*,}
  case "$sel_url" in
    file://*) ;;
    *)
      sel_allowed=$(remote_allow_env_value "$sel_db") || return 1
      if [ "$sel_allowed" != "1" ]; then
        echo "  $sel_db: ERROR: GC_DOLT_REMOTE names non-local remote '$sel_override' ($sel_url) — set GC_DOLT_PULL_ALLOW_REMOTE_<DB>=1 to allow pulling from it" >&2
        return 1
      fi ;;
  esac
  printf '%s\n' "$sel_match"; return 0
}

dolt_sql() {
  query="$1"
  host="${GC_DOLT_HOST:-127.0.0.1}"
  export DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}"
  run_bounded 120 dolt --host "$host" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls \
    sql --result-format csv -q "$query"
}

# --- Conflict resolution -----------------------------------------------------
#
# run_db_sql NAME DIR QUERY — run QUERY against database NAME (SQL mode, on
# the live server) or the database checkout at DIR (CLI mode), CSV on stdout.
run_db_sql() {
  if [ "$server_running" = true ]; then
    dolt_sql "USE \`$1\`; $3"
  else
    (cd "$2" && run_bounded 120 dolt sql --result-format csv -q "$3")
  fi
}

# valid_column_name — a bd column name is a plain identifier; anything else
# is never spliced into SQL.
valid_column_name() {
  case "$1" in
    ''|*[!A-Za-z0-9_]*) return 1 ;;
    *) return 0 ;;
  esac
}

# benign_conflict_sql NAME DIR — set CONFLICT_PREDICATE and CONFLICT_DIFFERING
# from the `issues` schema of the database. Returns 1 (and sets neither) when
# the schema cannot be read, has a column name that is not a plain
# identifier, or is not a bd issues table (no row_lock or no updated_at):
# then nothing is ever auto-resolved for this database.
#
# CONFLICT_PREDICATE selects a row of dolt_conflicts_issues that is a benign
# conflict: both sides modified the row, every column other than row_lock
# and updated_at is equal on both sides — compared as bytes (BINARY), so a
# case-insensitive collation cannot make "Fix API" and "fix api" equal, and
# NULL-safe (<=>) — and the `issues` schema the merge produced is still the
# schema this predicate was built from (a schema change under the merge
# would have added a column the predicate does not compare, so the
# fingerprint makes the predicate match nothing and the transaction fails
# closed).
#
# CONFLICT_DIFFERING is the comma-separated list of columns that differ on a
# conflicted row, for the report.
benign_conflict_sql() {
  CONFLICT_PREDICATE=""
  CONFLICT_DIFFERING=""
  cols_csv=$(run_db_sql "$1" "$2" "SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' ORDER BY ordinal_position" 2>/dev/null) || return 1
  cols=$(printf '%s\n' "$cols_csv" | awk 'NR > 1 && $0 != "" {print}' | tr -d '\r"')
  [ -n "$cols" ] || return 1
  fingerprint=""
  predicate="our_diff_type = 'modified' AND their_diff_type = 'modified'"
  differing=""
  has_row_lock=false
  has_updated_at=false
  for col in $cols; do
    valid_column_name "$col" || return 1
    fingerprint="${fingerprint:+$fingerprint,}$col"
    differing="${differing:+$differing, }IF(BINARY \`our_$col\` <=> BINARY \`their_$col\`, NULL, '$col')"
    case "$col" in
      row_lock) has_row_lock=true; continue ;;
      updated_at) has_updated_at=true; continue ;;
    esac
    predicate="$predicate AND BINARY \`our_$col\` <=> BINARY \`their_$col\`"
  done
  [ "$has_row_lock" = true ] && [ "$has_updated_at" = true ] || return 1
  predicate="$predicate AND (SELECT GROUP_CONCAT(column_name ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues') = '$fingerprint'"
  CONFLICT_PREDICATE="$predicate"
  CONFLICT_DIFFERING="CONCAT_WS(',', $differing)"
  return 0
}

# abort_cli_merge NAME DIR — CLI mode holds a refused merge in the working
# set on disk; put the database back at its pre-pull head. SQL mode never
# wrote anything to abort. An abort that fails is reported, not hidden.
abort_cli_merge() {
  [ "$server_running" = true ] && return 0
  if ! (cd "$2" && dolt merge --abort >/dev/null 2>&1); then
    echo "  $1: WARNING: dolt merge --abort failed; the conflicted merge is still in the working set (dolt conflicts cat issues)" >&2
  fi
}

# resolve_benign_conflicts NAME DIR REMOTE URL — the conflicted pull, retried
# inside one transaction that resolves the benign rows and commits only when
# no conflict is left. Prints the pull result line and returns 0 when the
# merge landed; otherwise prints the conflicted rows and the error and
# returns 1 with the database exactly as it was before the pull (in CLI
# mode the on-disk merge is aborted).
#
# SQL mode pulls again inside the transaction (the failed autocommit pull
# rolled its merge back); CLI mode already holds the merge in the working
# set. Either way the statements are the same: report every conflicted
# table and every conflicted `issues` row with its differing columns; give
# each benign row the remote's row_lock and updated_at and clear its
# conflict marker; report what is left; stage and commit. DOLT_COMMIT
# refuses a working set with any conflict left, and a transaction that never
# reaches COMMIT is discarded — the fail-closed operand is dolt's own, not a
# check of ours. "nothing to commit" on the retry means the pull found no
# conflict this time (a fast-forward or a clean merge, both durable before
# COMMIT), so it is reported as a plain pull.
resolve_benign_conflicts() {
  name="$1"; dir="$2"; remote_name="$3"; remote_url="$4"
  if ! benign_conflict_sql "$name" "$dir"; then
    abort_cli_merge "$name" "$dir"
    echo "  $name: ERROR: pull failed: merge conflict, and the issues table is not a bd store (no row_lock/updated_at) — resolve manually" >&2
    return 1
  fi
  p="$CONFLICT_PREDICATE"
  sql="SET @@autocommit = 0;"
  if [ "$server_running" = true ]; then
    sql="$sql CALL DOLT_PULL('$remote_name', 'main');"
  fi
  # Report rows put the count BEFORE the table name and fold a row's id and
  # differing columns into one field, so a table name with a comma, a quote
  # or a space cannot shift the parsed count (CSV quoting is undone on the
  # displayed name only).
  sql="$sql SELECT 'conflict' AS k, num_conflicts AS n, \`table\` AS t FROM dolt_conflicts;"
  sql="$sql SELECT 'schema' AS k, COUNT(*) AS n FROM dolt_schema_conflicts;"
  # A delete/modify conflict has no our_id (our side removed the row); the
  # row is still named from whichever side has it.
  sql="$sql SELECT 'row' AS k, CONCAT(COALESCE(our_id, their_id, base_id), ': ', $CONFLICT_DIFFERING) AS detail FROM dolt_conflicts_issues;"
  sql="$sql UPDATE issues SET row_lock = (SELECT c.their_row_lock FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND $p), updated_at = (SELECT c.their_updated_at FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND $p) WHERE id IN (SELECT c.our_id FROM dolt_conflicts_issues c WHERE $p);"
  sql="$sql DELETE FROM dolt_conflicts_issues WHERE $p;"
  sql="$sql SELECT 'remaining' AS k, num_conflicts AS n, \`table\` AS t FROM dolt_conflicts;"
  sql="$sql CALL DOLT_ADD('-A');"
  sql="$sql CALL DOLT_COMMIT('-m', 'gc dolt pull: merge $remote_name/main (row_lock/updated_at-only conflicts in issues resolved to the remote)', '--author', 'gc dolt pull <gc-dolt-pull@gascity.local>');"
  sql="$sql COMMIT;"
  resolve_rc=0
  out=$(run_db_sql "$name" "$dir" "$sql" 2>&1) || resolve_rc=$?
  rows=$(printf '%s\n' "$out" | grep '^row,' | sed 's/^row,//; s/^"//; s/"$//; s/""/"/g' || true)
  row_count=$(printf '%s\n' "$rows" | grep -c '.' || true)
  if [ "$resolve_rc" -eq 0 ]; then
    ids=$(printf '%s\n' "$rows" | sed 's/:.*//' | tr '\n' ' ' | sed 's/ $//')
    echo "  $name: pulled from $remote_url (resolved $row_count row_lock/updated_at-only conflict(s) in issues: $ids)"
    return 0
  fi
  reported=$(printf '%s\n' "$out" | grep -c '^conflict,' || true)
  schema_conflicts=$(printf '%s\n' "$out" | awk -F, '$1 == "schema" {n += $2} END {print n + 0}')
  # The retried pull found no conflict (a fast-forward or a clean merge,
  # both durable before COMMIT) exactly when the session reported no
  # conflicted table, no schema conflict, and then DOLT_COMMIT itself had
  # nothing to commit. The error line is matched by its own shape — result
  # rows (a table could be named anything) never count.
  if [ "$reported" -eq 0 ] && [ "$schema_conflicts" -eq 0 ] && printf '%s\n' "$out" | grep -q '^error on line [0-9]* for query CALL DOLT_COMMIT(.*nothing to commit'; then
    echo "  $name: pulled from $remote_url"
    return 0
  fi
  remaining=$(printf '%s\n' "$out" | awk -F, '$1 == "remaining" {n += $2} END {print n + 0}')
  if [ "$remaining" -eq 0 ]; then
    remaining=$(printf '%s\n' "$out" | awk -F, '$1 == "conflict" {n += $2} END {print n + 0}')
  fi
  tables=$(printf '%s\n' "$out" | grep '^conflict,' | sed 's/^conflict,[0-9]*,//; s/^"//; s/"$//; s/""/"/g' | sort -u | tr '\n' ' ' | sed 's/ $//')
  if [ -n "$rows" ]; then
    printf '%s\n' "$rows" | sed "s/^/  $name: conflict /" >&2
  fi
  abort_cli_merge "$name" "$dir"
  if [ "$schema_conflicts" -gt 0 ]; then
    echo "  $name: ERROR: pull failed: $schema_conflicts schema conflict(s) and $remaining row conflict(s) in ${tables:-no table} need manual resolution; nothing was written" >&2
  else
    echo "  $name: ERROR: pull failed: $remaining conflict(s) in ${tables:-unknown table(s)} need manual resolution; nothing was written" >&2
  fi
  case "$out" in
    *"nothing to commit"*|*"in conflict"*|*"unresolved conflicts"*) ;;
    *) printf '%s\n' "$out" | grep -i 'error' | head -3 | sed "s/^/  $name: /" >&2 || true ;;
  esac
  return 1
}

find_remote_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes ORDER BY name") || return 1
  candidates=$(printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "," $2}')
  # 2 = select_remote already printed a specific policy refusal; 1 = lookup failed.
  chosen=$(select_remote "$db" "$candidates") || return 2
  [ -z "$chosen" ] && return 0
  printf '%s\n' "$chosen" | awk -F, '{print $1 "|" $2}'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    find_rc=$?
    [ "$find_rc" -eq 2 ] || echo "  $name: ERROR: failed to query remotes" >&2
    return 1
  }
  if [ -z "$remote_pair" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  pull_out=""
  if pull_out=$(dolt_sql "USE \`$name\`; CALL DOLT_PULL('$remote_name', 'main')" 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  # A failed autocommit pull with conflicts rolled its merge back and wrote
  # nothing; retry it inside the resolving transaction.
  if printf '%s\n' "$pull_out" | grep -qi 'conflict'; then
    resolve_benign_conflicts "$name" "$data_dir/$name" "$remote_name" "$remote_url"
    return $?
  fi

  echo "  $name: ERROR: pull failed" >&2
  return 1
}

pull_database_cli() {
  d="$1"
  name="$2"

  remote_name=""
  remote_url=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    candidates=$(grep -o '"name":"[^"]*","url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null \
      | sed 's/"name":"//;s/","url":"/,/;s/"$//' \
      | sort)
    if [ -n "$candidates" ]; then
      chosen=$(select_remote "$name" "$candidates") || return 1
      remote_name=${chosen%%,*}
      remote_url=${chosen#*,}
    fi
  fi

  if [ -z "$remote_url" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  # A merge already in progress belongs to whoever started it (possibly
  # with resolutions half done); the pull must neither build on it nor
  # abort it. Read the state first — a failed read is a failure, never "no
  # merge" — and touch nothing when a merge is found.
  if ! pre_status=$(run_db_sql "$name" "$d" "SELECT is_merging FROM dolt_merge_status" 2>&1); then
    echo "  $name: ERROR: the merge state could not be read before pulling ($(printf '%s\n' "$pre_status" | head -1)); nothing was done" >&2
    return 1
  fi
  case "$(printf '%s\n' "$pre_status" | awk 'NR == 2 {print $1}')" in
    true|1)
      echo "  $name: ERROR: a merge is already in progress; resolve it or abort it (dolt merge --abort) before pulling; nothing was done" >&2
      return 1
      ;;
  esac

  if (cd "$d" && dolt pull "$remote_name" main 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  # CLI mode leaves a failed merge in the working set. Read the merge state
  # first — a failed read is a failure, never "no merge" — and put the
  # database back on every path that does not complete the merge.
  if ! merge_status=$(run_db_sql "$name" "$d" "SELECT is_merging FROM dolt_merge_status" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the merge state could not be read ($(printf '%s\n' "$merge_status" | head -1)); any merge in progress was aborted" >&2
    return 1
  fi
  case "$(printf '%s\n' "$merge_status" | awk 'NR == 2 {print $1}')" in
    true|1) ;;
    *)
      echo "  $name: ERROR: pull failed" >&2
      return 1
      ;;
  esac
  if ! schema_out=$(run_db_sql "$name" "$d" "SELECT COUNT(*) FROM dolt_schema_conflicts" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the schema conflicts could not be read ($(printf '%s\n' "$schema_out" | head -1)); the merge was aborted" >&2
    return 1
  fi
  schema_conflicts=$(printf '%s\n' "$schema_out" | awk 'NR == 2 {print $1 + 0}')
  if [ "${schema_conflicts:-0}" -gt 0 ]; then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed: $schema_conflicts schema conflict(s) need manual resolution; the merge was aborted, nothing was written" >&2
    return 1
  fi
  if ! conflicts_out=$(run_db_sql "$name" "$d" "SELECT COUNT(*) FROM dolt_conflicts" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the conflicts could not be read ($(printf '%s\n' "$conflicts_out" | head -1)); the merge was aborted" >&2
    return 1
  fi
  conflicts=$(printf '%s\n' "$conflicts_out" | awk 'NR == 2 {print $1 + 0}')
  if [ "${conflicts:-0}" -gt 0 ]; then
    resolve_benign_conflicts "$name" "$d" "$remote_name" "$remote_url"
    return $?
  fi

  abort_cli_merge "$name" "$d"
  echo "  $name: ERROR: pull failed: the merge did not complete and reported no conflict; the merge was aborted" >&2
  return 1
}

exit_code=0
server_running=false
is_running && server_running=true
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    if [ "$server_running" = true ]; then
      pull_database_sql "$name" || exit_code=1
    else
      pull_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
