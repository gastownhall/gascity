#!/bin/sh
# Doctor check: verify bd (beads) binary is available.
# Exit 0 = OK, 1 = Warning, 2 = Error.
# First line of stdout = message; remaining lines = details.

if ! command -v bd >/dev/null 2>&1; then
    echo "bd not found in PATH"
    echo "Install: go install github.com/steveyegge/beads/cmd/bd@latest"
    exit 2
fi

version=$(bd --version 2>/dev/null || echo "unknown")
echo "bd $version"
