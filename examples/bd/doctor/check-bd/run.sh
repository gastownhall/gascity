#!/bin/sh
# Doctor check: verify bd (beads) binary is available.
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

version=$(bd --version 2>/dev/null || echo "unknown")
echo "bd $version"
