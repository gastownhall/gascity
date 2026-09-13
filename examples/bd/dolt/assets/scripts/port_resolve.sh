#!/bin/sh
# port_resolve.sh — shared GC_DOLT_PORT discovery helper.
#
# Sourced by:
#   .gc/system/packs/dolt/assets/scripts/runtime.sh
#   .gc/system/packs/core/assets/scripts/dolt-target.sh
#   .gc/system/packs/bd/dolt/assets/scripts/runtime.sh
#
# Defines:
#   resolve_dolt_port_or_die  state_file  [provider_state_file]  data_dir  city_path
#
# Preconditions (caller responsibilities):
#   - managed_runtime_port is in scope (either from a sourced runtime.sh
#     above this file in the source chain, or inlined in the caller, as
#     dolt-target.sh currently inlines it).
#   - GC_CITY_PATH is set; the caller enforces this at file head.
#
# POSIX /bin/sh only. No bash-isms.

# resolve_dolt_port_or_die — print the discovered Dolt port to stdout, or
# exit 78 with a structured stderr error if no port can be resolved.
#
# Arguments:
#   $1  state_file           Absolute path to .gc/runtime/packs/dolt/dolt-state.json
#                            (or the equivalent for the caller's pack layout).
#   $2  provider_state_file  Optional provider fallback state file.
#   $3  data_dir             Absolute path the caller expects the running server
#                            to be serving from.
#   $4  city_path            Absolute path of the city root, used only in the
#                            error message so the operator knows which city failed.
#
# The legacy three-argument form remains supported:
#   resolve_dolt_port_or_die  state_file  data_dir  city_path
#
# Behavior:
#   1. It calls managed_runtime_port "$state_file" "$data_dir" and, on a
#      non-empty result, echoes the port and returns 0.
#   2. Otherwise, if provider_state_file was provided and GC_DOLT_STATE_FILE
#      did not force a specific state file, it tries the provider state.
#   3. Otherwise, if GC_DOLT_PORT is non-empty in the caller's environment,
#      the function echoes that value and returns 0 as an operator seed.
#   4. Otherwise, it writes the §3 error template to stderr and exits 78.
#
# Preconditions (caller must arrange):
#   - managed_runtime_port is in scope (sourced from runtime.sh, or inlined
#     in dolt-target.sh per its existing layout).
#   - GC_CITY_PATH is set (the helper does NOT default it; callers already
#     enforce ': "${GC_CITY_PATH:?...}"' at file head).
#
# Output contract:
#   - On success: exactly one line on stdout (the port, e.g. "47823").
#                  Nothing on stderr. Exit 0.
#   - On failure: nothing on stdout. Multi-line structured error on stderr
#                  (§3 verbatim). Exit 78 (whole shell exits, not return).
resolve_dolt_port_or_die() {
    _rdp_state_file="$1"
    _rdp_provider_state_file=""
    if [ "$#" -ge 4 ]; then
        _rdp_provider_state_file="$2"
        _rdp_data_dir="$3"
        _rdp_city_path="$4"
    else
        _rdp_data_dir="$2"
        _rdp_city_path="$3"
    fi

    _rdp_resolved=$(managed_runtime_port "$_rdp_state_file" "$_rdp_data_dir" 2>/dev/null)
    if [ -n "$_rdp_resolved" ]; then
        printf '%s\n' "$_rdp_resolved"
        return 0
    fi

    _rdp_consulted="GC_DOLT_PORT (unset), GC_DOLT_STATE_FILE"
    if [ -n "$_rdp_provider_state_file" ] && [ -z "${GC_DOLT_STATE_FILE:-}" ]; then
        _rdp_consulted="$_rdp_consulted, dolt-provider-state.json"
        _rdp_resolved=$(managed_runtime_port "$_rdp_provider_state_file" "$_rdp_data_dir" 2>/dev/null)
        if [ -n "$_rdp_resolved" ]; then
            printf '%s\n' "$_rdp_resolved"
            return 0
        fi
    fi

    if [ -n "${GC_DOLT_PORT:-}" ]; then
        printf '%s\n' "$GC_DOLT_PORT"
        return 0
    fi

    _rdp_state_status="missing"
    if [ -f "$_rdp_state_file" ]; then
        _rdp_state_status="present but not running"
    fi

    printf 'gc dolt: cannot resolve runtime port\n' >&2
    printf '  state_file: %s (%s)\n' "$_rdp_state_file" "$_rdp_state_status" >&2
    printf '  city_path:  %s\n' "$_rdp_city_path" >&2
    printf '  consulted:  %s\n' "$_rdp_consulted" >&2
    printf '  remediation: run `gc start` to bring up the city, or set\n' >&2
    printf '               GC_DOLT_PORT explicitly to an already-running\n' >&2
    printf '               server.\n' >&2
    exit 78
}
