#!/usr/bin/env bash
# check-go-format.sh GOFUMPT GOIMPORTS - fail if any Go file under the working
# directory is not gofumpt- and goimports-formatted. The body of the Bazel
# test //:go_format_test, which runs it over the declared source tree (runfiles
# symlinks, followed) with the pinned formatter builds; `make fmt-check` is the
# golangci-lint equivalent outside Bazel.
#
# Same file set as `golangci-lint fmt ./...`: testdata/, vendor/, and dot or
# underscore directories are skipped (the go tool ignores them), and so are
# generated files (golangci-lint's default formatter exclusion).
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 GOFUMPT GOIMPORTS" >&2
	exit 2
fi
gofumpt="$1"
goimports="$2"

jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
files="$(mktemp)"
trap 'rm -f "$files"' EXIT
# grep -L lists the files WITHOUT the generated-code marker (exit 1 when
# every file has one, which the emptiness check below reports).
find -L . \( -type d \( -name testdata -o -name vendor -o -name '.?*' -o -name '_*' \) -prune \) \
	-o \( -type f -name '*.go' -print0 \) |
	xargs -0 grep -LZE '^// Code generated .* DO NOT EDIT\.$' >"$files" || true

if [ ! -s "$files" ]; then
	echo "check-go-format: no Go files found under $(pwd); inputs are not wired" >&2
	exit 1
fi

status=0
for formatter in gofumpt goimports; do
	bin="$gofumpt"
	[ "$formatter" = goimports ] && bin="$goimports"
	# -l prints one whole line per file, so parallel batches do not interleave.
	if ! unformatted="$(xargs -0 -n 64 -P "$jobs" "$bin" -l <"$files")"; then
		status=1
		echo "check-go-format: $formatter failed (see errors above)" >&2
	fi
	if [ -n "$unformatted" ]; then
		status=1
		echo "check-go-format: files $formatter would change:" >&2
		printf '%s\n' "$unformatted" | sed 's|^\./|  |' >&2
		printf '%s\n' "$unformatted" | tr '\n' '\0' | xargs -0 "$bin" -d >&2 || true
	fi
done

if [ "$status" -ne 0 ]; then
	echo "Fix: run 'make fmt' and commit the result." >&2
	exit 1
fi
echo "check-go-format: $(tr -cd '\0' <"$files" | wc -c | tr -d ' ') Go files formatted."
