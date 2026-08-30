#!/usr/bin/env bash
# Backtest selective Go/Bazel timings against historical revisions.
#
# The pilot BUILD graph is copied into each revision, so this measures source
# changes under a constant Bazel graph. It is evidence for target selection,
# not a claim that those revisions were Bazel-buildable in their original form.
#
# Usage:
#   scripts/bazel-pr-backtest.sh [git-ref ...]
#
# By default this uses the commits that closed PRs #5246 and #5252. Set
# BAZEL_BIN to select a pinned bazelisk/bazel executable.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
bazel_bin="${BAZEL_BIN:-}"
if [[ -z "$bazel_bin" ]]; then
	if command -v bazelisk >/dev/null 2>&1; then
		bazel_bin="$(command -v bazelisk)"
	elif command -v bazel >/dev/null 2>&1; then
		bazel_bin="$(command -v bazel)"
	else
		echo "bazel-pr-backtest: bazelisk or bazel is required (set BAZEL_BIN)" >&2
		exit 127
	fi
fi

refs=("$@")
if ((${#refs[@]} == 0)); then
	refs=("7c33f3f7f1" "128bd64033")
fi

now_ns() {
	local stamp
	stamp="$(date +%s%N)"
	if [[ "$stamp" =~ ^[0-9]+$ ]]; then
		printf '%s\n' "$stamp"
	else
		printf '%s000000000\n' "$(date +%s)"
	fi
}

measure() {
	local output="$1"
	shift
	local start end status=0
	start="$(now_ns)"
	if "$@" >"$output" 2>&1; then
		:
	else
		status=$?
	fi
	end="$(now_ns)"
	MEASURE_SECONDS="$(awk -v elapsed="$((end - start))" 'BEGIN { printf "%.3f", elapsed / 1000000000 }')"
	return "$status"
}

copy_pilot_graph() {
	local destination="$1"
	local rel
	for rel in \
		MODULE.bazel MODULE.bazel.lock .bazelrc BUILD.bazel \
		internal/beads/contract/BUILD.bazel \
		internal/beadmeta/BUILD.bazel \
		internal/fsys/BUILD.bazel \
		internal/pidutil/BUILD.bazel \
		internal/testenv/BUILD.bazel; do
		if [[ -f "$repo_root/$rel" ]]; then
			mkdir -p "$destination/$(dirname "$rel")"
			cp -f "$repo_root/$rel" "$destination/$rel"
		fi
	done
}

printf 'ref\tgo_test_s\tbazel_cold_s\tbazel_forced_s\tbazel_forced_actions\n'
current_work=""
current_bazel_base=""
cleanup_exit() {
	if [[ -n "$current_work" && -n "$current_bazel_base" ]]; then
		"$bazel_bin" --output_base="$current_bazel_base" shutdown >/dev/null 2>&1 || true
	fi
	if [[ -n "$current_work" ]]; then
		git -C "$repo_root" worktree remove --force "$current_work" >/dev/null 2>&1 || true
	fi
}
trap cleanup_exit EXIT
trap 'exit 130' INT TERM

for ref in "${refs[@]}"; do
	resolved="$(git -C "$repo_root" rev-parse --verify "$ref^{commit}")"
	work="$(mktemp -d "${TMPDIR:-/tmp}/gascity-pr-backtest.XXXXXX")"
	current_work="$work"
	git -C "$repo_root" worktree add --detach --quiet "$work" "$resolved"
	actual_ref="$(git -C "$work" rev-parse HEAD)"
	if [[ "$actual_ref" != "$resolved" ]]; then
		echo "backtest: worktree resolved to $actual_ref, expected $resolved" >&2
		exit 1
	fi
	copy_pilot_graph "$work"
	go_directive="$(awk '$1 == "go" { print $2; exit }' "$work/go.mod")"
	go_runtime="$(go -C "$work" version | awk '{print $3}' | sed 's/^go//')"
	if [[ -n "$go_directive" && "$go_runtime" != "$go_directive" ]]; then
		echo "backtest: warning: $ref declares go $go_directive; host is go $go_runtime" >&2
	fi

	go_log="$work/go-test.log"
	if ! measure "$go_log" go -C "$work" test -count=1 ./internal/beads/contract; then
		echo "backtest: go test failed for $ref (see $go_log)" >&2
		exit 1
	fi
	go_seconds="$MEASURE_SECONDS"

	profile="$work/bazel.profile.gz"
	bep="$work/bazel.bep.json"
	bazel_log="$work/bazel.log"
	bazel_base="$work/bazel-output"
	current_bazel_base="$bazel_base"
	if ! measure "$bazel_log" "$bazel_bin" --output_base="$bazel_base" test \
		--noshow_progress --cache_test_results=no --profile="$profile" \
		--build_event_json_file="$bep" \
		//internal/beads/contract:contract_test; then
		echo "backtest: Bazel cold run failed for $ref (see $bazel_log)" >&2
		exit 1
	fi
	cold_seconds="$MEASURE_SECONDS"

	if ! measure "$bazel_log" "$bazel_bin" --output_base="$bazel_base" test --noshow_progress \
		--cache_test_results=no --profile="$profile" --build_event_json_file="$bep" \
		//internal/beads/contract:contract_test; then
		echo "backtest: Bazel forced run failed for $ref (see $bazel_log)" >&2
		exit 1
	fi
	forced_seconds="$MEASURE_SECONDS"
	actions="$(grep -Eo '[0-9]+ processes:' "$bazel_log" | tail -1 | awk '{print $1}')"
	actions="${actions:-unknown}"
	bep_actions="$(grep -c '"actionCompleted"' "$bep" 2>/dev/null || true)"

	printf '%s\t%s\t%s\t%s\t%s (%s BEP)\n' "${resolved:0:12}" "$go_seconds" "$cold_seconds" "$forced_seconds" "$actions" "${bep_actions:-0}"
	"$bazel_bin" --output_base="$bazel_base" shutdown >/dev/null 2>&1 || true
	current_bazel_base=""
	git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true
	current_work=""
done
