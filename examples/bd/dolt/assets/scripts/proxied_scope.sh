#!/bin/sh
# Shared bd-ownership predicate for the dolt pack.
#
# bd v1.3.0-rc.2 records a proxied workspace's topology only in
# <scope>/.beads/metadata.json ("backend":"dolt", "dolt_mode":"proxied-server")
# — the generic config.yaml template carries no mode. On such a scope bd
# starts, supervises and stops the `dolt sql-server` itself, under its own
# `bd db-proxy-child`. Every managed-Dolt verb in this pack is therefore a
# typed no-op there: probing, restarting or reaping would fight bd for a
# process gc does not own.
#
# Sourced by runtime.sh, and directly by the few commands that do not need the
# rest of the runtime (health-check reads a report from stdin).

GC_DOLT_PROXIED_NOOP_MESSAGE="dolt lifecycle is owned by bd for proxied scopes; nothing to do"
GC_DOLT_PROXIED_SKIP_REASON="bd-owned-proxied-scope"

# proxied_metadata_field <metadata.json> <key> prints a top-level string value.
proxied_metadata_field() {
  [ -f "$1" ] || return 0
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$1" 2>/dev/null | head -1
}

# bd_owns_proxied_scope [scope] succeeds when the scope's persisted beads
# metadata says bd owns its Dolt topology. Defaults to GC_CITY_PATH. Only the
# persisted binding counts: a scope with no beads metadata is not yet bound and
# stays under the managed-Dolt lens.
#
# This is the sh twin of cmd/gc's scopeBindingIsProviderOwnedProxied, and the
# two must answer alike: dolt_mode proxied-server with a backend Gas City
# treats as Dolt — dolt, bd, or absent — matched case-insensitively.
# examples/bd/dolt/proxied_scope_test.go pins the shared cases.
bd_owns_proxied_scope() {
  _proxied_scope="${1:-${GC_CITY_PATH:-}}"
  [ -n "$_proxied_scope" ] || return 1
  _proxied_meta="$_proxied_scope/.beads/metadata.json"
  [ -f "$_proxied_meta" ] || return 1
  _proxied_mode=$(proxied_metadata_field "$_proxied_meta" dolt_mode | tr 'A-Z' 'a-z')
  [ "$_proxied_mode" = "proxied-server" ] || return 1
  case "$(proxied_metadata_field "$_proxied_meta" backend | tr 'A-Z' 'a-z')" in
    '' | dolt | bd) return 0 ;;
    *) return 1 ;;
  esac
}

# exit_zero_if_bd_owns_proxied_scope [scope] prints the typed no-op line and
# exits 0 when bd owns the scope. Commands with a --json mode emit their own
# skip document instead of calling this.
exit_zero_if_bd_owns_proxied_scope() {
  if bd_owns_proxied_scope "$@"; then
    printf '%s\n' "$GC_DOLT_PROXIED_NOOP_MESSAGE"
    exit 0
  fi
}

# print_proxied_skip_json emits the machine-readable form of the same no-op.
print_proxied_skip_json() {
  printf '{\n  "timestamp": "%s",\n  "skipped": {\n    "reason": "%s",\n    "message": "%s"\n  }\n}\n' \
    "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" "$GC_DOLT_PROXIED_SKIP_REASON" "$GC_DOLT_PROXIED_NOOP_MESSAGE"
}
