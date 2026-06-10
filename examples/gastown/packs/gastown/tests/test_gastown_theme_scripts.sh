#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCRIPTS="$ROOT/gastown/assets/scripts"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

write_count_stubs() {
    local bin="$1"
    mkdir -p "$bin"

    cat >"$bin/timeout" <<'SH'
#!/usr/bin/env sh
shift
exec "$@"
SH
    chmod +x "$bin/timeout"

    cat >"$bin/gc" <<'SH'
#!/usr/bin/env sh
printf '%s\t%s\n' "$PWD" "$*" >>"$GC_LOG"
case "$PWD $*" in
    *city-a*" hook alpha") printf '[{"id":"work-a"},{"id":"work-b"}]' ;;
    *city-a*" mail check alpha") printf '1 unread message\n' ;;
    *city-b*" hook alpha") printf '[{"id":"work-a"}]' ;;
    *city-b*" mail check alpha") printf '0 unread messages\n' ;;
    *) printf '[]' ;;
esac
SH
    chmod +x "$bin/gc"
}

test_status_line_counts_with_bounded_gc_commands_and_cache() {
    local tmp city bin cache log output
    tmp=$(mktemp -d)
    city="$tmp/city-a"
    bin="$tmp/bin"
    cache="$tmp/cache"
    log="$tmp/bd.log"
    mkdir -p "$city" "$cache"
    write_count_stubs "$bin"

    if ! output=$(GC_LOG="$log" GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city"); then
        fail "status-line exited non-zero"
    fi

    [[ "$output" == "alpha | 🪝 2 | 📬 1" ]] || fail "unexpected status output: $output"
    grep -F "$city" "$log" >/dev/null || fail "gc was not run from city path"
    grep -F -- $'\thook alpha' "$log" >/dev/null || fail "missing bounded hook query"
    grep -F -- $'\tmail check alpha' "$log" >/dev/null || fail "missing bounded mail check query"

    : >"$log"
    if ! output=$(GC_LOG="$log" GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city"); then
        fail "status-line exited non-zero on cache hit"
    fi

    [[ "$output" == "alpha | 🪝 2 | 📬 1" ]] || fail "unexpected cached status output: $output"
    [[ ! -s "$log" ]] || fail "cache hit still called gc"
}

test_status_line_cache_is_city_scoped() {
    local tmp city_a city_b bin cache log output
    tmp=$(mktemp -d)
    city_a="$tmp/city-a"
    city_b="$tmp/city-b"
    bin="$tmp/bin"
    cache="$tmp/cache"
    log="$tmp/bd.log"
    mkdir -p "$city_a" "$city_b" "$cache"
    write_count_stubs "$bin"

    GC_LOG="$log" GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city_a" >/dev/null

    if ! output=$(GC_LOG="$log" GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city_b"); then
        fail "status-line exited non-zero for second city"
    fi

    [[ "$output" == "alpha | 🪝 1" ]] || fail "cache was not city scoped, got: $output"
    grep -F "$city_b" "$log" >/dev/null || fail "second city did not run its own query"
}

test_status_line_falls_back_to_agent_only_on_query_failure() {
    local tmp bin cache output
    tmp=$(mktemp -d)
    bin="$tmp/bin"
    cache="$tmp/cache"
    mkdir -p "$bin" "$cache"

    cat >"$bin/gc" <<'SH'
#!/usr/bin/env sh
exit 2
SH
    chmod +x "$bin/gc"

    if ! output=$(GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$tmp"); then
        fail "status-line exited non-zero on query failure"
    fi

    [[ "$output" == "alpha" ]] || fail "expected fallback status output, got: $output"
}

test_status_line_counts_ready_work_not_queued_nudges() {
    local tmp city bin cache output
    tmp=$(mktemp -d)
    city="$tmp/city"
    bin="$tmp/bin"
    cache="$tmp/cache"
    mkdir -p "$city" "$bin" "$cache"

    cat >"$bin/timeout" <<'SH'
#!/usr/bin/env sh
shift
exec "$@"
SH
    chmod +x "$bin/timeout"

    cat >"$bin/gc" <<'SH'
#!/usr/bin/env sh
case "$*" in
    "hook alpha") printf '[{"id":"ready-work"}]' ;;
    "mail check alpha") printf '0 unread messages\n' ;;
    "hook beta") printf '[]' ;;
    "mail check beta") printf '0 unread messages\n' ;;
esac
SH
    chmod +x "$bin/gc"

    cat >"$bin/bd" <<'SH'
#!/usr/bin/env sh
printf '[]\n'
SH
    chmod +x "$bin/bd"

    if ! output=$(GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city"); then
        fail "status-line exited non-zero for ready-work semantics"
    fi

    [[ "$output" == "alpha | 🪝 1" ]] || fail "expected ready work count from gc hook, got: $output"

    if ! output=$(GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" beta "$city"); then
        fail "status-line exited non-zero for idle ready-work semantics"
    fi

    [[ "$output" == "beta" ]] || fail "expected empty hook array to be omitted, got: $output"
}

test_status_line_uses_unread_mail_check_semantics() {
    local tmp city bin cache output
    tmp=$(mktemp -d)
    city="$tmp/city"
    bin="$tmp/bin"
    cache="$tmp/cache"
    mkdir -p "$city" "$bin" "$cache"

    cat >"$bin/timeout" <<'SH'
#!/usr/bin/env sh
shift
exec "$@"
SH
    chmod +x "$bin/timeout"

    cat >"$bin/gc" <<'SH'
#!/usr/bin/env sh
case "$*" in
    "hook alpha") printf '[]' ;;
    "mail check alpha") printf '0 unread messages\n' ;;
esac
SH
    chmod +x "$bin/gc"

    cat >"$bin/bd" <<'SH'
#!/usr/bin/env sh
printf '[{"labels":["read"]}]\n'
SH
    chmod +x "$bin/bd"

    if ! output=$(GC_STATUSLINE_CACHE_DIR="$cache" PATH="$bin:$PATH" "$SCRIPTS/status-line.sh" alpha "$city"); then
        fail "status-line exited non-zero for unread mail semantics"
    fi

    [[ "$output" == "alpha" ]] || fail "expected read mail to be omitted, got: $output"
}

test_tmux_theme_passes_city_path_to_status_helper() {
    local tmp bin log city
    tmp=$(mktemp -d)
    bin="$tmp/bin"
    log="$tmp/tmux.log"
    city="$tmp/city with spaces"
    mkdir -p "$bin" "$city"

    cat >"$bin/tmux" <<'SH'
#!/usr/bin/env sh
printf '%s\n' "$*" >>"$TMUX_LOG"
SH
    chmod +x "$bin/tmux"

    GC_CITY="$city" TMUX_LOG="$log" PATH="$bin:$PATH" "$SCRIPTS/tmux-theme.sh" session-alpha alpha "$ROOT/gastown"

    grep -F -- "status-right #('$ROOT/gastown/assets/scripts/status-line.sh' 'alpha' '$city') %H:%M" "$log" >/dev/null ||
        fail "tmux-theme did not quote and pass city path to status-line"
}

test_status_line_counts_with_bounded_gc_commands_and_cache
test_status_line_cache_is_city_scoped
test_status_line_falls_back_to_agent_only_on_query_failure
test_status_line_counts_ready_work_not_queued_nudges
test_status_line_uses_unread_mail_check_semantics
test_tmux_theme_passes_city_path_to_status_helper

echo "gastown theme script tests passed"
