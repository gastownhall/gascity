#!/usr/bin/env bash
# Verify .githooks is this clone's active core.hooksPath.
#
# The gates in .githooks cannot report their own absence: when another installer
# claims core.hooksPath (beads points it at .beads/hooks), git stops invoking
# them entirely and every commit looks clean while formatting, lint-changed, the
# codegen+stage steps and make vet are all skipped. That is how spec-derived
# drift reached the mainline. This check is the external detector; run it
# with `make check-hooks`.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
configured="$(git config --get core.hooksPath || true)"

expected="$repo_root/.githooks"

if [ ! -d "$expected" ]; then
	echo "ERROR: $expected does not exist — this does not look like a gascity clone." >&2
	exit 1
fi

if [ -z "$configured" ]; then
	actual=""
else
	# core.hooksPath may be relative to the repo root or absolute. Resolve both
	# spellings to a canonical path before comparing.
	case "$configured" in
	/*) candidate="$configured" ;;
	*) candidate="$repo_root/$configured" ;;
	esac
	actual="$(cd "$candidate" 2>/dev/null && pwd -P || true)"
fi

if [ "$actual" = "$(cd "$expected" && pwd -P)" ]; then
	exit 0
fi

{
	echo "ERROR: .githooks is not this clone's git hooks directory."
	echo "  core.hooksPath: ${configured:-<unset — git defaults to .git/hooks>}"
	echo "  expected:       $expected"
	echo
	echo "Every gate in .githooks (staged-Go formatting, lint-changed, the OpenAPI"
	echo "spec/client/schema codegen+stage steps, make vet, the push-time suite) is"
	echo "silently skipped while another directory owns core.hooksPath."
	echo
	echo "Fix it with:  make setup"
	echo
	echo "Beads is not lost by doing this: .githooks chains every beads-managed hook"
	echo "through .githooks/lib/beads-chain.sh. If beads reclaims core.hooksPath"
	echo "later (its installer does), re-run 'make setup'."
} >&2
exit 1
