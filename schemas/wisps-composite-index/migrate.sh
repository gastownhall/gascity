#!/usr/bin/env bash
#
# Adds the verified-needed wisps indexes to the Dolt-backed beads wisps table:
#   - idx_wisps_type_status_assignee for mail-check lookups
#   - idx_wisps_status for PrimeWisps status=open reconciliation
#   - idx_wisps_defer_until for the control-dispatcher readiness probe
#
# Connection discovery order:
#   database: GC_DOLT_DATABASE, BEADS_DOLT_DATABASE, then .beads/metadata.json dolt_database
#   host:     GC_DOLT_HOST, BEADS_DOLT_SERVER_HOST, then 127.0.0.1
#   port:     GC_DOLT_PORT, BEADS_DOLT_SERVER_PORT, .beads/dolt-server.port,
#             .beads/config.yaml dolt.port, then GC runtime Dolt state JSON
#   user:     GC_DOLT_USER, BEADS_DOLT_SERVER_USER, then root
#   password: GC_DOLT_PASSWORD, BEADS_DOLT_PASSWORD, then empty
#
# Rollback path: run ./rollback.sh from this directory against the same
# connection settings.
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
    info "index $INDEX_NAME already exists on wisps($INDEX_COLUMNS)"
else
    dolt_sql -q "
        USE \`$DOLT_DB\`;
        CREATE INDEX $INDEX_NAME ON wisps(issue_type, status, assignee);
    " >/dev/null
    changed=true
fi

indexes=$(show_wisps_indexes)
verify_index_definition "$indexes"

rows=$(status_index_rows "$indexes")
if [ "$rows" -gt 0 ]; then
    verify_status_index_definition "$indexes"
    info "index $STATUS_INDEX_NAME already exists on wisps($STATUS_INDEX_COLUMNS)"
else
    dolt_sql -q "
        USE \`$DOLT_DB\`;
        CREATE INDEX $STATUS_INDEX_NAME ON wisps(status);
    " >/dev/null
    changed=true
fi

indexes=$(show_wisps_indexes)
verify_status_index_definition "$indexes"

rows=$(defer_until_index_rows "$indexes")
if [ "$rows" -gt 0 ]; then
    verify_defer_until_index_definition "$indexes"
    info "index $DEFER_UNTIL_INDEX_NAME already exists on wisps($DEFER_UNTIL_INDEX_COLUMNS)"
else
    dolt_sql -q "
        USE \`$DOLT_DB\`;
        CREATE INDEX $DEFER_UNTIL_INDEX_NAME ON wisps(defer_until);
    " >/dev/null
    changed=true
fi

indexes=$(show_wisps_indexes)
verify_defer_until_index_definition "$indexes"

if [ "$changed" = false ]; then
    info "all wisps indexes already exist; no changes needed"
    exit 0
fi

commit_schema_change "schema: add wisps planner indexes" >/dev/null
info "created and committed missing wisps indexes"
