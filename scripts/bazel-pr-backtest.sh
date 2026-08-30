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
# By default this uses commits associated with PRs #5246, #5252, #5193, and
# #5215. Set BAZEL_BIN to select a pinned bazelisk/bazel executable.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
bazel_bin="${BAZEL_BIN:-}"
# Cold output bases may need to compile the pinned Go SDK and rules_go tools.
# Keep that expensive step bounded, while allowing callers to tune it.
backtest_timeout="${BACKTEST_TIMEOUT:-300}"
keep_artifacts="${BACKTEST_KEEP_ARTIFACTS:-0}"
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
	refs=("7c33f3f7f1" "128bd64033" "a784438ce0" "58a47d6bdc")
fi

now_ns() {
	if command -v perl >/dev/null 2>&1; then
		perl -MTime::HiRes=time -e 'printf "%.0f\n", time() * 1000000000'
		return
	fi
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

run_bounded() {
	if command -v timeout >/dev/null 2>&1; then
		timeout --signal=TERM --kill-after=15 "$backtest_timeout" "$@"
		return $?
	fi
	if command -v gtimeout >/dev/null 2>&1; then
		gtimeout --signal=TERM --kill-after=15 "$backtest_timeout" "$@"
		return $?
	fi
	# macOS does not ship GNU timeout. Callers there should install coreutils
	# or set a suitably small test scope; this fallback preserves portability.
	"$@"
}

shutdown_bazel() {
	local base="$1"
	if [[ -z "$base" || ! -d "$base" ]]; then
		return 0
	fi
	if command -v timeout >/dev/null 2>&1; then
		timeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown >/dev/null 2>&1 || true
	elif command -v gtimeout >/dev/null 2>&1; then
		gtimeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown >/dev/null 2>&1 || true
	else
		"$bazel_bin" --output_base="$base" shutdown >/dev/null 2>&1 || true
	fi
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

printf 'ref\tgo_test_s\tbazel_cold_s\tbazel_forced_s\tbazel_noop_s\tbazel_incremental_s\tprocesses(cold/forced/noop/incremental)\tcache_hits(cold/forced/noop/incremental)\n'
current_work=""
current_bazel_base=""
cleanup_exit() {
	if [[ -n "$current_work" && -n "$current_bazel_base" ]]; then
		shutdown_bazel "$current_bazel_base"
	fi
	if [[ -n "$current_work" ]]; then
		chmod -R u+w "$current_work" 2>/dev/null || true
		git -C "$repo_root" worktree remove --force "$current_work" >/dev/null 2>&1 || true
		chmod -R u+w "$current_work" 2>/dev/null || true
		if [[ "$keep_artifacts" != "1" ]]; then
			rm -rf -- "$current_work"
		fi
	fi
}

cleanup_worktree() {
	local path="$1"
	chmod -R u+w "$path" 2>/dev/null || true
	git -C "$repo_root" worktree remove --force "$path" >/dev/null 2>&1 || true
	if [[ "$keep_artifacts" != "1" ]]; then
		rm -rf -- "$path"
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
	if ! measure "$go_log" run_bounded go -C "$work" test -count=1 ./internal/beads/contract; then
		echo "backtest: go test failed for $ref (see $go_log)" >&2
		exit 1
	fi
	go_seconds="$MEASURE_SECONDS"

	bazel_cold_profile="$work/bazel-cold.profile.gz"
	bazel_cold_bep="$work/bazel-cold.bep.json"
	bazel_forced_profile="$work/bazel-forced.profile.gz"
	bazel_forced_bep="$work/bazel-forced.bep.json"
	bazel_noop_bep="$work/bazel-noop.bep.json"
	bazel_incremental_profile="$work/bazel-incremental.profile.gz"
	bazel_incremental_bep="$work/bazel-incremental.bep.json"
	bazel_cold_log="$work/bazel-cold.log"
	bazel_forced_log="$work/bazel-forced.log"
	bazel_noop_log="$work/bazel-noop.log"
	bazel_incremental_log="$work/bazel-incremental.log"
	bazel_base="$work/bazel-output"
	current_bazel_base="$bazel_base"
	if ! measure "$bazel_cold_log" run_bounded "$bazel_bin" --output_base="$bazel_base" test \
		--noshow_progress --cache_test_results=no --profile="$bazel_cold_profile" \
		--build_event_json_file="$bazel_cold_bep" \
		//internal/beads/contract:contract_test; then
		echo "backtest: Bazel cold run failed for $ref (see $bazel_cold_log)" >&2
		exit 1
	fi
	cold_seconds="$MEASURE_SECONDS"

	if ! measure "$bazel_forced_log" run_bounded "$bazel_bin" --output_base="$bazel_base" test --noshow_progress \
		--cache_test_results=no --profile="$bazel_forced_profile" --build_event_json_file="$bazel_forced_bep" \
		//internal/beads/contract:contract_test; then
		echo "backtest: Bazel forced run failed for $ref (see $bazel_forced_log)" >&2
		exit 1
	fi
	forced_seconds="$MEASURE_SECONDS"
	if ! measure "$bazel_noop_log" run_bounded "$bazel_bin" --output_base="$bazel_base" test --noshow_progress \
		--cache_test_results=yes --build_event_json_file="$bazel_noop_bep" //internal/beads/contract:contract_test; then
		echo "backtest: Bazel cached no-op failed for $ref (see $bazel_noop_log)" >&2
		exit 1
	fi
	noop_seconds="$MEASURE_SECONDS"
	# A one-line source edit models the common developer loop. The disposable
	# worktree is discarded after the run, so the historical checkout is safe.
	printf '\n// bazel backtest edit\n' >> "$work/internal/beads/contract/metadata.go"
	if ! measure "$bazel_incremental_log" run_bounded "$bazel_bin" --output_base="$bazel_base" test --noshow_progress \
		--cache_test_results=no --profile="$bazel_incremental_profile" \
		--build_event_json_file="$bazel_incremental_bep" //internal/beads/contract:contract_test; then
		echo "backtest: Bazel incremental run failed for $ref (see $bazel_incremental_log)" >&2
		exit 1
	fi
	incremental_seconds="$MEASURE_SECONDS"
	process_count() { grep -Eo '[0-9]+ processes:' "$1" | tail -1 | awk '{print $1}' || true; }
	cache_count() { grep -Eo '[0-9]+ action cache hit' "$1" | tail -1 | awk '{print $1}' || true; }
	cold_processes="$(process_count "$bazel_cold_log")"; cold_processes="${cold_processes:-unknown}"
	forced_processes="$(process_count "$bazel_forced_log")"; forced_processes="${forced_processes:-unknown}"
	noop_processes="$(process_count "$bazel_noop_log")"; noop_processes="${noop_processes:-unknown}"
	incremental_processes="$(process_count "$bazel_incremental_log")"; incremental_processes="${incremental_processes:-unknown}"
	cold_hits="$(cache_count "$bazel_cold_log")"; cold_hits="${cold_hits:-0}"
	forced_hits="$(cache_count "$bazel_forced_log")"; forced_hits="${forced_hits:-0}"
	noop_hits="$(cache_count "$bazel_noop_log")"; noop_hits="${noop_hits:-0}"
	incremental_hits="$(cache_count "$bazel_incremental_log")"; incremental_hits="${incremental_hits:-0}"

	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s/%s/%s/%s\t%s/%s/%s/%s\n' "${resolved:0:12}" "$go_seconds" "$cold_seconds" "$forced_seconds" "$noop_seconds" "$incremental_seconds" "$cold_processes" "$forced_processes" "$noop_processes" "$incremental_processes" "$cold_hits" "$forced_hits" "$noop_hits" "$incremental_hits"
	shutdown_bazel "$bazel_base"
	current_bazel_base=""
	cleanup_worktree "$work"
	current_work=""
done
