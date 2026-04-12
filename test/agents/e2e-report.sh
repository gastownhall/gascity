#!/bin/bash
# Dumps startup state to a report file for test verification.
# Used by E2E integration tests to verify the agent build pipeline.
# After reporting once, the agent stays alive but honors runtime drain
# requests so config-drift and restart tests can observe a clean restart.
set -euo pipefail

# Allow any user to delete files created by this script.
# Docker containers run as root, but the test runner is non-root.
umask 000

SAFE_NAME="${GC_AGENT//\//__}"
REPORT_DIR="${GC_CITY}/.gc-reports"
mkdir -p "$REPORT_DIR"
REPORT="${REPORT_DIR}/${SAFE_NAME}.report"

{
    echo "STATUS=started"
    echo "CWD=$(pwd)"
    env | grep "^GC_" | sort || true
    env | grep "^CUSTOM_" | sort || true

    # Overlay files
    for f in .overlay-marker overlay-subdir/nested.txt; do
        [ -f "$f" ] && echo "FILE_PRESENT=$f"
    done

    # Hook files
    for f in .gemini/settings.json .opencode/plugins/gascity.js \
             .github/copilot-instructions.md; do
        [ -f "$f" ] && echo "HOOK_PRESENT=$f"
    done
    [ -f "${GC_CITY}/.gc/settings.json" ] && echo "HOOK_PRESENT=.gc/settings.json"

    # Pre_start marker
    [ -f "prestart-marker" ] && echo "FILE_PRESENT=prestart-marker"

    # Bead store access
    if command -v bd >/dev/null 2>&1 && bd ready 2>/dev/null; then
        echo "BD_READY=true"
    else
        echo "BD_READY=false"
    fi

    echo "STATUS=complete"
} > "$REPORT" 2>&1

while true; do
    if gc runtime drain-check 2>/dev/null; then
        gc runtime drain-ack 2>/dev/null || true
        exit 0
    fi
    sleep 0.2
done
