#!/usr/bin/env bash
# Compare a selective Bazel test with the equivalent Go test.
#
# Usage:
#   scripts/bazel-backtest.sh [bazel-target]
#
# The target defaults to the pilot clock package. Set BAZEL_BIN to use a
# pinned bazelisk/bazel executable (for example, BAZEL_BIN=/opt/bazelisk).
# Set GO_PACKAGE when the Bazel label's package differs from the Go package
# path. Test result caching is disabled to compare execution costs directly.
set -euo pipefail

target="${1:-//internal/clock:clock_test}"
if [[ "$target" != //*:* ]]; then
	echo "bazel-backtest: target must be a single //package:name label" >&2
	exit 2
fi
bazel_package="${target#//}"
bazel_package="${bazel_package%%:*}"
go_package="${GO_PACKAGE:-./$bazel_package}"
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

run_bazel_cold() {
	local output_base
	output_base="$(mktemp -d "${TMPDIR:-/tmp}/gascity-bazel.XXXXXX")"
	local status=0
	if "$bazel_bin" --output_base="$output_base" test --noshow_progress --cache_test_results=no "$target"; then
		:
	else
		status=$?
	fi
	"$bazel_bin" --output_base="$output_base" shutdown >/dev/null 2>&1 || true
	# rules_go marks SDK artifacts read-only; make the private temp tree
	# removable before deleting it.
	chmod -R u+w "$output_base" 2>/dev/null || true
	rm -rf "$output_base"
	return "$status"
}

echo "target: $target"
run_timed go-test go test -count=1 "$go_package"
run_timed bazel-cold run_bazel_cold
run_timed bazel-warm "$bazel_bin" test --noshow_progress --cache_test_results=no "$target"
