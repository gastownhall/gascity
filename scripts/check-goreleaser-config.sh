#!/usr/bin/env bash
# check-goreleaser-config.sh <goreleaser> <.goreleaser.yml>
#
# `goreleaser check` over the release configuration: the config loads,
# validates and uses no deprecated keys. GoReleaser refuses to check outside
# a git repository with a remote (its scm release settings resolve the
# repository from it), so the config is checked from a throwaway repository
# whose origin names this one; nothing is fetched.
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 <goreleaser> <.goreleaser.yml>" >&2
	exit 2
fi
goreleaser="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
config="$2"

work="$(mktemp -d "${TEST_TMPDIR:-${TMPDIR:-/tmp}}/goreleaser-check.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cp "$config" "$work/.goreleaser.yml"
cd "$work"
git init --quiet
git remote add origin https://github.com/gastownhall/gascity.git
HOME="$work" "$goreleaser" check
