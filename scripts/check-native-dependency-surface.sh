#!/usr/bin/env bash
set -euo pipefail

max_modules="${GC_NATIVE_DEP_MAX_MODULES:-727}"
max_binary_bytes="${GC_NATIVE_DEP_MAX_BINARY_BYTES:-270000000}"
max_aws_modules="${GC_NATIVE_DEP_MAX_AWS_MODULES:-25}"
max_azure_modules="${GC_NATIVE_DEP_MAX_AZURE_MODULES:-9}"
max_dolthub_modules="${GC_NATIVE_DEP_MAX_DOLTHUB_MODULES:-15}"
max_google_api_modules="${GC_NATIVE_DEP_MAX_GOOGLE_API_MODULES:-1}"

modules="$(go list -m all)"
total_modules="$(printf '%s\n' "$modules" | sed '/^$/d' | wc -l | tr -d ' ')"
if [ "$total_modules" -gt "$max_modules" ]; then
	echo "native dependency guard: module graph has $total_modules modules; max is $max_modules" >&2
	exit 1
fi

counts="$(printf '%s\n' "$modules" | awk '
	/^github.com\/aws\/aws-sdk-go-v2( |\/)/ {aws++}
	/^github.com\/Azure\/azure-sdk-for-go( |\/)/ {azure++}
	/^github.com\/dolthub\// {dolthub++}
	/^github.com\/steveyegge\/beads / {beads++}
	/^google\.golang\.org\/api / {googleapi++}
	END {
		printf "aws=%d azure=%d dolthub=%d beads=%d googleapi=%d\n",
			aws, azure, dolthub, beads, googleapi
	}
')"
eval "$counts"

if [ "${beads:-0}" -ne 1 ]; then
	echo "native dependency guard: expected exactly one github.com/steveyegge/beads module, got ${beads:-0}" >&2
	exit 1
fi
if [ "${aws:-0}" -gt "$max_aws_modules" ]; then
	echo "native dependency guard: AWS SDK module count ${aws:-0} exceeds $max_aws_modules" >&2
	exit 1
fi
if [ "${azure:-0}" -gt "$max_azure_modules" ]; then
	echo "native dependency guard: Azure SDK module count ${azure:-0} exceeds $max_azure_modules" >&2
	exit 1
fi
if [ "${dolthub:-0}" -gt "$max_dolthub_modules" ]; then
	echo "native dependency guard: DoltHub module count ${dolthub:-0} exceeds $max_dolthub_modules" >&2
	exit 1
fi
if [ "${googleapi:-0}" -gt "$max_google_api_modules" ]; then
	echo "native dependency guard: google.golang.org/api count ${googleapi:-0} exceeds $max_google_api_modules" >&2
	exit 1
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM HUP
go build -o "$tmpdir/gc" ./cmd/gc
binary_bytes="$(wc -c < "$tmpdir/gc" | tr -d ' ')"
if [ "$binary_bytes" -gt "$max_binary_bytes" ]; then
	echo "native dependency guard: gc binary is $binary_bytes bytes; max is $max_binary_bytes" >&2
	exit 1
fi

echo "native dependency guard: modules=$total_modules aws=${aws:-0} azure=${azure:-0} dolthub=${dolthub:-0} googleapi=${googleapi:-0} binary_bytes=$binary_bytes"
