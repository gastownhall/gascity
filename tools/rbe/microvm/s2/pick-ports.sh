#!/usr/bin/env bash
# pick-ports.sh -- find 4 consecutive free loopback ports for the S2 rig and
# print the base port. Checked against `ss -ltn` (constraint from the task:
# use loopback ports outside the reserved ranges in use locally).
#
# cherry is a shared multi-tenant host; something else occasionally grabs a
# port between the check and nativelink's own bind (observed empirically --
# EADDRINUSE on ports `ss` had just reported free). Caller should retry on
# failure; this script just finds one currently-free candidate.
set -euo pipefail

for _ in $(seq 1 40); do
	base=$((56000 + (RANDOM + RANDOM * 977) % 8000))
	free=1
	for off in 0 1 2 3; do
		p=$((base + off))
		if ss -ltn "( sport = :$p )" | grep -q LISTEN; then
			free=0
			break
		fi
	done
	if [ "$free" = 1 ]; then
		echo "$base"
		exit 0
	fi
done
echo "pick-ports: could not find 4 free consecutive ports" >&2
exit 1
