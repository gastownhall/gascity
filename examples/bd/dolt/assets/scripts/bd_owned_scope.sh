#!/bin/sh
# Shared bd-ownership predicate for the dolt pack.
#
# On a scope bd owns, bd starts, supervises and stops the `dolt sql-server`
# itself. Every managed-Dolt verb in this pack is therefore a typed no-op
# there: probing, restarting or reaping would fight bd for a process gc does
# not own.
#
# Two durable records say so, and they describe different shapes:
#
#   proxied      <scope>/.beads/metadata.json says "dolt_mode":"proxied-server"
#                (bd v1.3.0-rc.2 records a proxied workspace's topology only
#                there — the generic config.yaml template carries no mode).
#                bd runs the server under its own `bd db-proxy-child`.
#   handed off   <scope>/.beads/ownership-handoff.json records a committed
#                transfer to bd. The transport does not change, so the scope
#                still reads dolt_mode "server" — which is exactly why the
#                proxied arm alone answered "gc's" for a city bd owns.
#
# gc's own .gc/scope-ownership.json is deliberately NOT a third arm here. It
# records that gc delegated a scope's initialisation to bd, and the topology
# matrix pins the pack's managed verbs as still running for a journaled
# direct-local city. This mirrors cmd/gc's cityDoltLifecycleOwnedByBd; the two
# must answer alike.
#
# Sourced by runtime.sh, and directly by the few commands that do not need the
# rest of the runtime (health-check reads a report from stdin).

GC_DOLT_PROXIED_NOOP_MESSAGE="dolt lifecycle is owned by bd for proxied scopes; nothing to do"
GC_DOLT_PROXIED_SKIP_REASON="bd-owned-proxied-scope"
GC_DOLT_BD_OWNED_NOOP_MESSAGE="dolt lifecycle is owned by bd for this scope; nothing to do"
GC_DOLT_BD_OWNED_SKIP_REASON="bd-owned-scope"

# bd_scope_json_field <file> <key> prints a top-level string value.
bd_scope_json_field() {
  [ -f "$1" ] || return 0
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$1" 2>/dev/null | head -1
}

# bd_scope_json_number <file> <key> prints a top-level integer value. The string
# reader above cannot: it requires quotes around the value, so every number in
# the document reads as absent, which is the difference between "this journal is
# version 2" and "this journal has no version".
bd_scope_json_number() {
  [ -f "$1" ] || return 0
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$1" 2>/dev/null | head -1
}

# bd_owns_proxied_scope [scope] succeeds when the scope's persisted beads
# metadata says bd owns its Dolt topology through the proxy. Defaults to
# GC_CITY_PATH. Only the persisted binding counts: a scope with no beads
# metadata is not yet bound and stays under the managed-Dolt lens.
#
# This is the sh twin of cmd/gc's scopeBindingIsProviderOwnedProxied, and the
# two must answer alike: dolt_mode proxied-server with a backend Gas City
# treats as Dolt — dolt, bd, or absent — matched case-insensitively.
# examples/bd/dolt/bd_owned_scope_test.go pins the shared cases.
bd_owns_proxied_scope() {
  _proxied_scope="${1:-${GC_CITY_PATH:-}}"
  [ -n "$_proxied_scope" ] || return 1
  _proxied_meta="$_proxied_scope/.beads/metadata.json"
  [ -f "$_proxied_meta" ] || return 1
  _proxied_mode=$(bd_scope_json_field "$_proxied_meta" dolt_mode | tr 'A-Z' 'a-z')
  [ "$_proxied_mode" = "proxied-server" ] || return 1
  case "$(bd_scope_json_field "$_proxied_meta" backend | tr 'A-Z' 'a-z')" in
    '' | dolt | bd) return 0 ;;
    *) return 1 ;;
  esac
}

# bd_scope_was_handed_off [scope] succeeds when the ownership handoff journal
# records a committed transfer of the scope to bd.
#
# The read is deliberately shallow, the same way internal/doctor's is. cmd/gc
# authenticates the same journal in full (cmd/gc/dolt_handoff_projection.go)
# because there the answer gates starting a second sql-server; here it only
# decides whether an order has anything to do, and every unreadable or
# unsettled journal falls back to the managed lens — where every gc verb the
# order would call now refuses on its own.
# Shallow still means versioned. The schema version is read before the phase,
# because the phase names are shared between journal versions and do not mean
# the same thing in them. A journal of any other version is not a signal.
GC_DOLT_HANDOFF_JOURNAL_VERSION=2

bd_scope_was_handed_off() {
  _handoff_scope="${1:-${GC_CITY_PATH:-}}"
  [ -n "$_handoff_scope" ] || return 1
  _handoff_journal="$_handoff_scope/.beads/ownership-handoff.json"
  [ -f "$_handoff_journal" ] || return 1
  [ "$(bd_scope_json_number "$_handoff_journal" schema_version)" = "$GC_DOLT_HANDOFF_JOURNAL_VERSION" ] || return 1
  [ "$(bd_scope_json_field "$_handoff_journal" phase)" = "committed" ] || return 1
  [ "$(bd_scope_json_field "$_handoff_journal" owner)" = "bd" ]
}

# bd_owns_scope [scope] succeeds when bd runs this scope's Dolt server, by
# either record. On success it sets GC_DOLT_BD_SKIP_REASON and
# GC_DOLT_BD_NOOP_MESSAGE to the pair that describes the shape it found, so a
# skip document never tells an operator their direct city is proxied.
bd_owns_scope() {
  if bd_owns_proxied_scope "$@"; then
    GC_DOLT_BD_SKIP_REASON="$GC_DOLT_PROXIED_SKIP_REASON"
    GC_DOLT_BD_NOOP_MESSAGE="$GC_DOLT_PROXIED_NOOP_MESSAGE"
    return 0
  fi
  if bd_scope_was_handed_off "$@"; then
    GC_DOLT_BD_SKIP_REASON="$GC_DOLT_BD_OWNED_SKIP_REASON"
    GC_DOLT_BD_NOOP_MESSAGE="$GC_DOLT_BD_OWNED_NOOP_MESSAGE"
    return 0
  fi
  return 1
}

# bd_owned_skip_document_seen <report> succeeds when a report already carries
# either bd-ownership skip reason.
bd_owned_skip_document_seen() {
  case "$1" in
    *"\"$GC_DOLT_PROXIED_SKIP_REASON\""* | *"\"$GC_DOLT_BD_OWNED_SKIP_REASON\""*) return 0 ;;
  esac
  return 1
}

# exit_zero_if_bd_owns_scope [scope] prints the typed no-op line and exits 0
# when bd owns the scope. Commands with a --json mode emit their own skip
# document instead of calling this.
exit_zero_if_bd_owns_scope() {
  if bd_owns_scope "$@"; then
    printf '%s\n' "$GC_DOLT_BD_NOOP_MESSAGE"
    exit 0
  fi
}

# print_bd_owned_skip_json emits the machine-readable form of the same no-op.
# It reads the reason bd_owns_scope selected, so it must be called after it.
print_bd_owned_skip_json() {
  printf '{\n  "timestamp": "%s",\n  "skipped": {\n    "reason": "%s",\n    "message": "%s"\n  }\n}\n' \
    "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" "${GC_DOLT_BD_SKIP_REASON:-$GC_DOLT_PROXIED_SKIP_REASON}" \
    "${GC_DOLT_BD_NOOP_MESSAGE:-$GC_DOLT_PROXIED_NOOP_MESSAGE}"
}
