#!/usr/bin/env bash
#
# Drops the wisps planner indexes from the Dolt-backed beads wisps table and
# commits the rollback to Dolt history. Uses the same connection discovery
# documented in migrate.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/common.sh"

resolve_connection
ensure_connection
ensure_database_and_table

info "using Dolt database $DOLT_DB on $DOLT_HOST:$DOLT_PORT as $DOLT_USER"

changed=false
indexes=$(show_wisps_indexes)
rows=$(index_rows "$indexes")
if [ "$rows" -gt 0 ]; then
    verify_index_definition "$indexes"
    dolt_sql -q "
        USE \`$DOLT_DB\`;
        DROP INDEX $INDEX_NAME ON wisps;
    " >/dev/null
    changed=true
else
    info "index $INDEX_NAME is absent"
fi

indexes=$(show_wisps_indexes)
rows=$(status_index_rows "$indexes")
if [ "$rows" -gt 0 ]; then
    verify_status_index_definition "$indexes"
    dolt_sql -q "
        USE \`$DOLT_DB\`;
        DROP INDEX $STATUS_INDEX_NAME ON wisps;
    " >/dev/null
    changed=true
else
    info "index $STATUS_INDEX_NAME is absent"
fi

if [ "$changed" = false ]; then
    info "no rollback changes needed"
    exit 0
fi

indexes=$(show_wisps_indexes)
rows=$(index_rows "$indexes")
if [ "$rows" -ne 0 ]; then
    die "rollback failed; index $INDEX_NAME is still present"
fi

rows=$(status_index_rows "$indexes")
if [ "$rows" -ne 0 ]; then
    die "rollback failed; index $STATUS_INDEX_NAME is still present"
fi

commit_schema_change "schema: drop wisps planner indexes" >/dev/null
info "dropped and committed wisps planner indexes"
