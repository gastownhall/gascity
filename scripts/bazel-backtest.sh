#!/usr/bin/env bash
# Compare a selective Bazel test with the equivalent Go test.
#
# Usage:
#   scripts/bazel-backtest.sh [bazel-target]
#
# The target defaults to the pilot clock package. Set BAZEL_BIN to use a
# pinned bazelisk/bazel executable (for example, BAZEL_BIN=/opt/bazelisk).
set -euo pipefail

target="${1:-//internal/clock:clock_test}"
bazel_package="${target#//}"
bazel_package="${bazel_package%%:*}"
bazel_bin="${BAZEL_BIN:-}"
if [[ -z "$bazel_bin" ]]; then
	if command -v bazelisk >/dev/null 2>&1; then
		bazel_bin="$(command -v bazelisk)"
	elif command -v bazel >/dev/null 2>&1; then
		bazel_bin="$(command -v bazel)"
	else
		echo "bazel-backtest: bazelisk or bazel is required (set BAZEL_BIN)" >&2
		exit 127
	fi
fi

now_ns() {
	date +%s%N
}

run_timed() {
	local label="$1"
	shift
	local start end
	start="$(now_ns)"
	"$@"
	end="$(now_ns)"
	awk -v elapsed="$((end - start))" 'BEGIN { printf "%s\t%.3f s\n", "'"$label"'", elapsed / 1000000000 }'
}

echo "target: $target"
run_timed go-test go test -count=1 "./$bazel_package"
run_timed bazel-cold "$bazel_bin" test --noshow_progress "$target"
run_timed bazel-warm "$bazel_bin" test --noshow_progress "$target"
