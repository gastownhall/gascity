#!/usr/bin/env bash
# Fail when contrib/k8s/Dockerfile.mail's apt-get --only-upgrade list differs from
# the packages its digest-pinned base image has pending upgrades for.
#
# The base image is pinned by digest, so an OS-package fix reaches the built image
# only if the Dockerfile names the package. A package missing from the list turns
# Container Scan red the day a CVE is scored against it; a pending upgrade exists
# earlier than that, so this check reports the gap before any scanner does.
#
# Usage: mcp-mail-os-upgrade-drift.sh [Dockerfile]   (default contrib/k8s/Dockerfile.mail)
#
# Needs network and a container runtime, so it is not part of the hermetic make or
# bazel suites; .github/workflows/container-scan.yml runs it on its schedule.
# MCP_MAIL_PENDING_UPGRADES_FILE (one package per line) stands in for the container
# run, for offline use. DOCKER selects the runtime (default docker).
#
# Exit 0: the list and the pending set agree. Exit 1: they differ; both directions
# are printed. Any other failure (no stanza, no digest, runtime error) also fails.
set -euo pipefail

dockerfile="${1:-contrib/k8s/Dockerfile.mail}"

# The --only-upgrade stanza runs from the flag to the next "&&" line.
listed=$(awk '
	on && /^[[:space:]]*&&/ { exit }
	on { gsub(/[[:space:]\\]/, ""); if ($0 != "") print }
	/apt-get install .*--only-upgrade/ { on = 1 }
' "$dockerfile" | sort -u)
if [ -z "$listed" ]; then
	echo "error: no --only-upgrade package list found in $dockerfile" >&2
	exit 1
fi

if [ -n "${MCP_MAIL_PENDING_UPGRADES_FILE:-}" ]; then
	pending=$(sort -u "$MCP_MAIL_PENDING_UPGRADES_FILE")
else
	image=$(awk '$1 == "FROM" { print $2; exit }' "$dockerfile")
	case "$image" in
	*@sha256:*) ;;
	*)
		echo "error: $dockerfile FROM line is not pinned by digest: ${image:-<none>}" >&2
		exit 1
		;;
	esac
	# "apt-get -s upgrade" prints "Inst <package> [<old>] (<new> ...)" for each
	# package it would upgrade without installing anything.
	pending=$("${DOCKER:-docker}" run --rm --entrypoint sh "$image" \
		-c 'apt-get update -qq >/dev/null && apt-get -s upgrade' |
		awk '$1 == "Inst" { print $2 }' | sort -u)
fi

missing=$(comm -13 <(printf '%s\n' "$listed") <(printf '%s\n' "$pending") | grep . || true)
stale=$(comm -23 <(printf '%s\n' "$listed") <(printf '%s\n' "$pending") | grep . || true)
[ -z "$missing$stale" ] && exit 0

if [ -n "$missing" ]; then
	printf 'pending upgrade, not in the --only-upgrade list:\n'
	printf '  %s\n' $missing
	printf '\n'
fi
if [ -n "$stale" ]; then
	printf 'in the --only-upgrade list, no pending upgrade:\n'
	printf '  %s\n' $stale
	printf '\n'
fi
printf 'drift: update the --only-upgrade list in %s to match the pinned base.\n' "$dockerfile"
exit 1
