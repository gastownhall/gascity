#!/usr/bin/env bash
# Shared bead-ID matcher for the bash agent scripts in test/agents.
# This file is sourced by those scripts; it is not an agent itself.
#
# IDs are matched by SHAPE, never by a hardcoded prefix. Since #5416, a bead
# store's minted prefix is store-configured or derived from the city name,
# and the integration bd shim can mint gc-N into the same store where gc
# mints <prefix>-N — so a filter anchored to a literal gc, bd, or mc prefix
# silently drops real rows. test/integration/bead_id_test.go pins BEAD_ID_ERE
# below to the Go matcher (beadIDERE) so the two never drift apart.
#
# Source this file in other scripts:
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/bead-id.sh"

# A whole-token bead ID: a letter-led alphanumeric prefix, one or more dash
# segments (configured prefixes may contain interior dashes; wisp IDs are
# gm-wisp-x), and optional .N child segments (e.g. ga-t832q4.2).
BEAD_ID_ERE='[A-Za-z][A-Za-z0-9]*(-[A-Za-z0-9]+)+([.][A-Za-z0-9]+)*'

# bead_id_rows reads stdin and prints only the lines whose first field is a
# bead ID. Always returns 0, even with no matches, since callers run under
# set -euo pipefail. Portable across GNU grep, BSD grep and ugrep: -E and
# POSIX classes only, no \b and no \s.
bead_id_rows() {
    grep -E "^${BEAD_ID_ERE}([[:space:]]|\$)" || true
}
