#!/usr/bin/env bash

INDEX_NAME="idx_wisps_type_status_assignee"
INDEX_COLUMNS="issue_type, status, assignee"
STATUS_INDEX_NAME="idx_wisps_status"
STATUS_INDEX_COLUMNS="status"
COMMIT_AUTHOR="gascity-builder <builder@gascity.local>"

die() {
    printf 'wisps composite-index migration: %s\n' "$*" >&2
    exit 1
}

info() {
    printf 'wisps composite-index migration: %s\n' "$*"
}

json_string_field() {
    local file="$1"
    local key="$2"

    [ -f "$file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r --arg key "$key" '.[$key] // empty | strings' "$file" 2>/dev/null || true
        return 0
    fi

    if command -v python3 >/dev/null 2>&1; then
        python3 - "$file" "$key" 2>/dev/null <<'PY' || true
import json
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    value = json.load(f).get(sys.argv[2], "")
if isinstance(value, str):
    print(value)
PY
        return 0
    fi

    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$file" 2>/dev/null | head -1 || true
}

json_number_field() {
    local file="$1"
    local key="$2"

    [ -f "$file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r --arg key "$key" '.[$key] // empty | numbers' "$file" 2>/dev/null || true
        return 0
    fi

    if command -v python3 >/dev/null 2>&1; then
        python3 - "$file" "$key" 2>/dev/null <<'PY' || true
import json
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    value = json.load(f).get(sys.argv[2], "")
if isinstance(value, int):
    print(value)
PY
        return 0
    fi

    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$file" 2>/dev/null | head -1 || true
}

json_bool_field() {
    local file="$1"
    local key="$2"

    [ -f "$file" ] || return 0

    if command -v jq >/dev/null 2>&1; then
        jq -r --arg key "$key" '.[$key] // empty | booleans' "$file" 2>/dev/null || true
        return 0
    fi

    sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\(true\\|false\\).*/\\1/p" "$file" 2>/dev/null | head -1 || true
}

config_value() {
    local file="$1"
    local key="$2"

    [ -f "$file" ] || return 0

    sed -n "s/^[[:space:]]*$key[[:space:]]*:[[:space:]]*//p" "$file" 2>/dev/null \
        | head -1 \
        | sed 's/^"//;s/"$//;s/^'\''//;s/'\''$//' || true
}

candidate_beads_dirs() {
    [ -n "${BEADS_DIR:-}" ] && printf '%s\n' "$BEADS_DIR"
    [ -n "${GC_BEADS_SCOPE_ROOT:-}" ] && printf '%s/.beads\n' "$GC_BEADS_SCOPE_ROOT"
    [ -n "${GC_RIG_ROOT:-}" ] && printf '%s/.beads\n' "$GC_RIG_ROOT"
    [ -n "${GC_CITY_PATH:-}" ] && printf '%s/.beads\n' "$GC_CITY_PATH"
    [ -n "${GC_CITY:-}" ] && printf '%s/.beads\n' "$GC_CITY"
    printf '%s/.beads\n' "$PWD"
}

find_beads_dir() {
    local dir
    while IFS= read -r dir; do
        [ -n "$dir" ] || continue
        if [ -f "$dir/metadata.json" ] || [ -f "$dir/dolt-server.port" ] || [ -f "$dir/config.yaml" ]; then
            printf '%s\n' "$dir"
            return 0
        fi
    done < <(candidate_beads_dirs)
    return 1
}

validate_identifier() {
    local label="$1"
    local value="$2"

    if [[ ! "$value" =~ ^[A-Za-z0-9_][A-Za-z0-9_-]*$ ]]; then
        die "$label $value contains unsupported characters; set GC_DOLT_DATABASE to a simple Dolt database identifier"
    fi
}

resolve_database() {
    local db="${GC_DOLT_DATABASE:-${BEADS_DOLT_DATABASE:-}}"
    local beads_dir metadata

    if [ -z "$db" ]; then
        if beads_dir=$(find_beads_dir); then
            metadata="$beads_dir/metadata.json"
            db=$(json_string_field "$metadata" "dolt_database" | head -1)
        fi
    fi

    [ -n "$db" ] || die "cannot resolve Dolt database; set GC_DOLT_DATABASE or BEADS_DOLT_DATABASE, or provide .beads/metadata.json with dolt_database"
    validate_identifier "database" "$db"
    printf '%s\n' "$db"
}

runtime_state_files() {
    if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
        printf '%s\n' "$GC_DOLT_STATE_FILE"
        return 0
    fi

    [ -n "${GC_CITY_RUNTIME_DIR:-}" ] && {
        printf '%s/packs/dolt/dolt-state.json\n' "$GC_CITY_RUNTIME_DIR"
        printf '%s/packs/dolt/dolt-provider-state.json\n' "$GC_CITY_RUNTIME_DIR"
    }
    [ -n "${GC_CITY_PATH:-}" ] && {
        printf '%s/.gc/runtime/packs/dolt/dolt-state.json\n' "$GC_CITY_PATH"
        printf '%s/.gc/runtime/packs/dolt/dolt-provider-state.json\n' "$GC_CITY_PATH"
    }
    [ -n "${GC_CITY:-}" ] && {
        printf '%s/.gc/runtime/packs/dolt/dolt-state.json\n' "$GC_CITY"
        printf '%s/.gc/runtime/packs/dolt/dolt-provider-state.json\n' "$GC_CITY"
    }
}

resolve_port_from_runtime_state() {
    local state_file running port

    while IFS= read -r state_file; do
        [ -f "$state_file" ] || continue
        running=$(json_bool_field "$state_file" "running" | head -1)
        [ "$running" = "true" ] || continue
        port=$(json_number_field "$state_file" "port" | head -1)
        if [ -n "$port" ]; then
            printf '%s\n' "$port"
            return 0
        fi
    done < <(runtime_state_files)

    return 1
}

resolve_port() {
    local port="${GC_DOLT_PORT:-${BEADS_DOLT_SERVER_PORT:-}}"
    local beads_dir config_port

    if [ -z "$port" ] && beads_dir=$(find_beads_dir); then
        if [ -f "$beads_dir/dolt-server.port" ]; then
            port=$(tr -d '[:space:]' < "$beads_dir/dolt-server.port")
        fi
        if [ -z "$port" ]; then
            config_port=$(config_value "$beads_dir/config.yaml" "dolt.port")
            [ -n "$config_port" ] && port="$config_port"
        fi
    fi

    if [ -z "$port" ]; then
        port=$(resolve_port_from_runtime_state || true)
    fi

    [ -n "$port" ] || die "cannot resolve Dolt SQL port; set GC_DOLT_PORT or BEADS_DOLT_SERVER_PORT, or run from a GC session with runtime state"
    case "$port" in
        *[!0-9]*)
            die "invalid Dolt SQL port: $port"
            ;;
    esac
    printf '%s\n' "$port"
}

resolve_connection() {
    DOLT_BIN="${DOLT_BIN:-dolt}"
    command -v "$DOLT_BIN" >/dev/null 2>&1 || die "dolt CLI not found; set DOLT_BIN to the dolt executable"

    DOLT_DB=$(resolve_database)
    DOLT_HOST="${GC_DOLT_HOST:-${BEADS_DOLT_SERVER_HOST:-127.0.0.1}}"
    DOLT_PORT=$(resolve_port)
    DOLT_USER="${GC_DOLT_USER:-${BEADS_DOLT_SERVER_USER:-root}}"
    DOLT_PASSWORD="${GC_DOLT_PASSWORD:-${BEADS_DOLT_PASSWORD:-}}"
}

dolt_sql() {
    DOLT_CLI_PASSWORD="$DOLT_PASSWORD" "$DOLT_BIN" \
        --host "$DOLT_HOST" \
        --port "$DOLT_PORT" \
        --user "$DOLT_USER" \
        --no-tls \
        sql "$@"
}

dolt_sql_csv() {
    local query="$1"
    dolt_sql --result-format csv -q "$query"
}

ensure_connection() {
    local output

    if ! output=$(dolt_sql -q "SELECT 1;" 2>&1); then
        die "cannot connect to Dolt SQL server at $DOLT_HOST:$DOLT_PORT as $DOLT_USER: $output"
    fi
}

ensure_database_and_table() {
    local output

    if ! output=$(dolt_sql_csv "SHOW DATABASES;" 2>&1); then
        die "cannot list databases on $DOLT_HOST:$DOLT_PORT: $output"
    fi
    if ! printf '%s\n' "$output" | tail -n +2 | tr -d '\r' | grep -Fx "$DOLT_DB" >/dev/null; then
        die "database $DOLT_DB not found on $DOLT_HOST:$DOLT_PORT"
    fi

    if ! output=$(dolt_sql_csv "USE \`$DOLT_DB\`; SHOW TABLES LIKE 'wisps';" 2>&1); then
        die "cannot inspect wisps table in database $DOLT_DB: $output"
    fi
    if ! printf '%s\n' "$output" | tail -n +2 | tr -d '\r' | grep -Fx "wisps" >/dev/null; then
        die "database $DOLT_DB does not contain a wisps table"
    fi
}

show_wisps_indexes() {
    dolt_sql_csv "USE \`$DOLT_DB\`; SHOW INDEX FROM wisps;"
}

index_rows() {
    local output="$1"

    printf '%s\n' "$output" | awk -F, -v idx="$INDEX_NAME" 'NR > 1 && $3 == idx { count++ } END { print count + 0 }'
}

verify_index_definition() {
    local output="$1"
    local count
    local missing=""

    count=$(index_rows "$output")
    if [ "$count" -ne 3 ]; then
        die "index $INDEX_NAME has $count column rows; expected exactly 3 for ($INDEX_COLUMNS)"
    fi

    printf '%s\n' "$output" | awk -F, -v idx="$INDEX_NAME" 'NR > 1 && $3 == idx && $4 == "1" && $5 == "issue_type" { found = 1 } END { exit found ? 0 : 1 }' || missing="$missing issue_type"
    printf '%s\n' "$output" | awk -F, -v idx="$INDEX_NAME" 'NR > 1 && $3 == idx && $4 == "2" && $5 == "status" { found = 1 } END { exit found ? 0 : 1 }' || missing="$missing status"
    printf '%s\n' "$output" | awk -F, -v idx="$INDEX_NAME" 'NR > 1 && $3 == idx && $4 == "3" && $5 == "assignee" { found = 1 } END { exit found ? 0 : 1 }' || missing="$missing assignee"

    if [ -n "$missing" ]; then
        die "index $INDEX_NAME exists but does not match ($INDEX_COLUMNS); missing:$missing"
    fi
}

status_index_rows() {
    local output="$1"

    printf '%s\n' "$output" | awk -F, -v idx="$STATUS_INDEX_NAME" 'NR > 1 && $3 == idx { count++ } END { print count + 0 }'
}

verify_status_index_definition() {
    local output="$1"
    local count

    count=$(status_index_rows "$output")
    if [ "$count" -ne 1 ]; then
        die "index $STATUS_INDEX_NAME has $count column rows; expected exactly 1 for ($STATUS_INDEX_COLUMNS)"
    fi

    printf '%s\n' "$output" | awk -F, -v idx="$STATUS_INDEX_NAME" 'NR > 1 && $3 == idx && $4 == "1" && $5 == "status" { found = 1 } END { exit found ? 0 : 1 }' \
        || die "index $STATUS_INDEX_NAME exists but does not match ($STATUS_INDEX_COLUMNS)"
}

commit_schema_change() {
    local message="$1"
    local output

    if ! output=$(dolt_sql -q "
        USE \`$DOLT_DB\`;
        CALL DOLT_ADD('.');
        CALL DOLT_COMMIT('-m', '$message', '--author', '$COMMIT_AUTHOR');
    " 2>&1); then
        case "$output" in
            *"nothing to commit"*|*"Nothing to commit"*)
                info "no Dolt schema changes to commit"
                return 0
                ;;
            *)
                die "Dolt commit failed for database $DOLT_DB: $output"
                ;;
        esac
    fi

    printf '%s\n' "$output"
}
