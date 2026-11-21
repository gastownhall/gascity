#!/bin/sh
# Doctor check: verify bd (beads) binary is available and meets the minimum
# supported version.
# Exit 0 = OK, 1 = Warning, 2 = Error.
# First line of stdout = message; remaining lines = details.

if ! command -v bd >/dev/null 2>&1; then
    echo "bd not found in PATH"
    # The Go module path is steveyegge/beads even though releases live under
    # gastownhall/beads, and @latest resolves past prereleases -- so it would
    # install something older than the pin when the pin is an RC. Install the
    # version deps.env pins, which is what CI and the devcontainer get.
    echo "Install: .github/scripts/install-bd-archive.sh \"\$BD_VERSION\"  # BD_VERSION from deps.env"
    echo "     or: go install github.com/steveyegge/beads/cmd/bd@\$BD_VERSION"
    exit 2
fi

# The minimum supported bd: cmd/gc bdMinVersion and deps.env BD_PREV_VERSION
# (scripts/bd_version_pin_test.go keeps them in step). bd 1.3.0 is the first
# tested release with the --if-assignee/--if-status conditional-release flags
# gc requires; bd 1.2.2 re-released the 1.1.2 code without them.
required="1.3.0"

version=$(bd --version 2>/dev/null || echo "unknown")

# "bd version 1.3.1 (abc123)" -> "1.3.1"; a prerelease suffix compares as its
# release, matching gc's own version gate.
ver=$(printf '%s\n' "$version" | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
if [ -z "$ver" ]; then
    echo "bd $version (could not read a version; need >= $required)"
    exit 1
fi

version_lt() {
    a_major=${1%%.*}; a_rest=${1#*.}; a_minor=${a_rest%%.*}; a_patch=${a_rest#*.}
    b_major=${2%%.*}; b_rest=${2#*.}; b_minor=${b_rest%%.*}; b_patch=${b_rest#*.}
    [ "$a_major" -ne "$b_major" ] && { [ "$a_major" -lt "$b_major" ]; return; }
    [ "$a_minor" -ne "$b_minor" ] && { [ "$a_minor" -lt "$b_minor" ]; return; }
    [ "$a_patch" -lt "$b_patch" ]
}

if version_lt "$ver" "$required"; then
    echo "bd $ver is too old (need >= $required) — upgrade required"
    echo "Reason: Gas City requires bd >= $required for the --if-assignee/--if-status conditional release; older bd is unsupported."
    echo "Install: https://github.com/gastownhall/beads/releases"
    exit 2
fi

echo "bd $version"
