#!/bin/sh
# verified-sling.sh — wrap `gc sling` with pre-flight bead existence checks.
#
# Usage: verified-sling.sh [gc-sling-args...]
#
# Scans positional args for anything that looks like a bead ID
# (e.g. sc-xxxxx, hq-xxxx, sc-xxxxx.12) and runs `bd show` on each before
# invoking `gc sling`. Exits non-zero with a clear error if any bead is
# missing, preventing molecules from being poured against fabricated IDs.
#
# This is the defensive companion to the quartermaster prompt's "NEVER
# fabricate IDs" rule — catches hallucinated IDs before they strand workers.
# See hq-7zv for the 2026-04-21 incident that motivated this guard.

set -eu

if [ "$#" -eq 0 ]; then
    echo "verified-sling.sh: usage: verified-sling.sh [gc-sling-args...]" >&2
    exit 2
fi

looks_like_bead_id() {
    # Matches: sc-abc12, hq-x3y7, sc-abcde.12
    # Two or more lowercase letters, a dash, then alphanumerics,
    # optionally a .number step suffix. Flags are excluded.
    case "$1" in
        -*) return 1 ;;
        *.*.*) return 1 ;;
    esac
    printf '%s\n' "$1" | grep -Eq '^[a-z]{2,}-[a-z0-9]{3,}(\.[0-9]+)?$'
}

missing=""
for arg in "$@"; do
    if looks_like_bead_id "$arg"; then
        if ! bd show "$arg" >/dev/null 2>&1; then
            missing="$missing $arg"
        fi
    fi
done

if [ -n "$missing" ]; then
    echo "verified-sling.sh: refusing to sling — bead(s) not found:$missing" >&2
    echo "verified-sling.sh: create the bead with \`gc bd create ... --json | jq -r .id\` and capture its real ID before slinging." >&2
    exit 3
fi

exec gc sling "$@"
