#!/usr/bin/env bash
# Shared helpers for Gas City release scripts.
#
# Source this file in other scripts:
#   SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   source "$SCRIPT_DIR/lib/common.sh"

# shellcheck disable=SC2034  # colors are consumed by sourcing scripts
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

is_darwin_sed() {
    [[ "$OSTYPE" == "darwin"* && "$(command -v sed)" == "/usr/bin/sed" ]]
}

# Cross-platform `sed -i` wrapper (BSD sed on macOS needs an explicit empty backup arg).
sed_i() {
    if is_darwin_sed; then
        sed -i '' "$@"
    else
        sed -i "$@"
    fi
}
