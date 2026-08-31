#!/usr/bin/env bash
# Measure selective Bazel scenarios against Go for a configurable target.
#
# This is intentionally a research harness: it does not alter CI or claim
# that historical revisions carried the current BUILD graph.  The graph files
# are copied from the checkout under test, while source edits are disposable.
# Defaults run the five bounded config pilot targets. Set BACKTEST_TARGETS to
# a comma-separated label list to override the selection set.
# Linux with GNU `/usr/bin/time -f` and `timeout` (or `gtimeout`) is required.
# Set BACKTEST_SAMPLES, BACKTEST_TIMEOUT, and BACKTEST_SCENARIOS to tune runs;
# pass one or more git refs as positional arguments.
set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,12p' "$0"
  exit 0
fi

repo_root="$(git rev-parse --show-toplevel)"
resolver="$repo_root/scripts/bazel_target_resolver.py"
bazel_bin="${BAZEL_BIN:-$(command -v bazelisk || command -v bazel || true)}"
samples="${BACKTEST_SAMPLES:-20}"
timeout_s="${BACKTEST_TIMEOUT:-300}"
target_labels_csv="${BACKTEST_TARGETS:-//internal/config:config_diagnostic_locations_test,//internal/config:config_envname_test,//internal/config:config_identity_seam_test,//internal/config:config_session_setup_path_test,//internal/config:config_storage_endpoint_test}"
go_package="${BACKTEST_GO_PACKAGE:-./internal/config}"
source_file="${BACKTEST_SOURCE_FILE:-internal/config/session_setup_path.go}"
test_file="${BACKTEST_TEST_FILE:-internal/config/session_setup_path_test.go}"
unrelated_file="${BACKTEST_UNRELATED_FILE:-docs/README.md}"
graph_files="${BACKTEST_GRAPH_FILES:-MODULE.bazel MODULE.bazel.lock .bazelrc BUILD.bazel internal/beads/contract/BUILD.bazel internal/beadmeta/BUILD.bazel internal/fsys/BUILD.bazel internal/pidutil/BUILD.bazel internal/testenv/BUILD.bazel internal/config/BUILD.bazel internal/config/diagnostic_locations.go internal/config/envname.go internal/config/config_envname_bazel_test.go internal/config/diagnostic_locations_fixture_bazel_test.go internal/config/diagnostic_locations_test.go internal/config/testdata/diagnostic_locator.toml internal/config/identity_seam.go internal/config/identity_seam_bazel_test.go internal/config/session_setup_path.go internal/config/session_setup_path_test.go internal/config/storage_binding_validation.go internal/config/storage_endpoint.go internal/config/storage_endpoint_bazel_test.go}"
scenario_csv="${BACKTEST_SCENARIOS:-cold,forced,no-op,source-edit,test-edit,unrelated-edit,go-mod}"
if [[ -z "$bazel_bin" ]]; then echo "set BAZEL_BIN or install bazelisk" >&2; exit 127; fi
[[ "$(uname -s)" == Linux ]] || { echo "Linux is required for this harness (GNU timing semantics)" >&2; exit 2; }
if ! command -v timeout >/dev/null 2>&1 && ! command -v gtimeout >/dev/null 2>&1; then echo "GNU timeout (or gtimeout) is required for bounded runs" >&2; exit 127; fi
[[ -x /usr/bin/time ]] || { echo "/usr/bin/time is required" >&2; exit 127; }
if ! /usr/bin/time -f '%e' true >/dev/null 2>&1; then echo "/usr/bin/time does not support GNU -f; install GNU time" >&2; exit 127; fi
[[ "$samples" =~ ^[1-9][0-9]*$ ]] || { echo "BACKTEST_SAMPLES must be positive" >&2; exit 2; }
[[ "$samples" -le 1000 ]] || { echo "BACKTEST_SAMPLES must be <= 1000" >&2; exit 2; }
IFS=',' read -r -a target_labels <<<"$target_labels_csv"
for target in "${target_labels[@]}"; do [[ "$target" == //*:* ]] || { echo "BACKTEST_TARGETS contains invalid label: $target" >&2; exit 2; }; done
[[ "${#target_labels[@]}" -eq 5 ]] || { echo "BACKTEST_TARGETS must contain the five config pilot labels" >&2; exit 2; }
for expected in //internal/config:config_diagnostic_locations_test //internal/config:config_envname_test //internal/config:config_identity_seam_test //internal/config:config_session_setup_path_test //internal/config:config_storage_endpoint_test; do
  printf '%s\n' "${target_labels[@]}" | grep -Fxq "$expected" || { echo "BACKTEST_TARGETS missing required label: $expected" >&2; exit 2; }
done

IFS=',' read -r -a scenarios <<<"$scenario_csv"
read -r -a graph_array <<<"$graph_files"
allowed=" go cold forced no-op source-edit test-edit unrelated-edit go-mod "
for s in "${scenarios[@]}"; do [[ "$allowed" == *" $s "* ]] || { echo "unknown scenario: $s" >&2; exit 2; }; done
# Go is a measured pseudo-scenario, not a selectable Bazel target. Always add
# it exactly once so every requested scenario has a same-sample Go baseline.
scenario_order=(go)
for s in "${scenarios[@]}"; do
  [[ "$s" == go ]] && continue
  scenario_order+=("$s")
done
refs=("$@")
if ((${#refs[@]} == 0)); then refs=("7c33f3f7f1" "128bd64033" "a784438ce0" "58a47d6bdc"); fi
artifact_dir="${BACKTEST_ARTIFACT_DIR:-}"

copy_artifact() {
  local source="$1" name="$2"
  [[ -n "$artifact_dir" && -f "$source" ]] || return 0
  mkdir -p "$artifact_dir"
  cp -f -- "$source" "$artifact_dir/$name"
}

now_ns() {
  if command -v perl >/dev/null 2>&1; then perl -MTime::HiRes=time -e 'printf "%.0f\n", time()*1e9'; return; fi
  local stamp; stamp="$(date +%s%N)"
  [[ "$stamp" =~ ^[0-9]+$ ]] && printf '%s\n' "$stamp" || printf '%s000000000\n' "$(date +%s)"
}
tsv_escape() {
  local value="$1"
  value=${value//\\/\\\\}; value=${value//$'\t'/\\t}; value=${value//$'\r'/\\r}; value=${value//$'\n'/\\n}
  printf '%s' "$value"
}
run_bounded_in_worktree() {
  local work="$1"
  shift
  (
    cd "$work" || exit 1
    if command -v timeout >/dev/null 2>&1; then
      timeout --signal=TERM --kill-after=15 "$timeout_s" "$@"
    else
      gtimeout --signal=TERM --kill-after=15 "$timeout_s" "$@"
    fi
  )
}
run_timed_in_worktree() {
  local work="$1"
  local time_file="$2"
  shift 2
  local status=0
  (
    cd "$work" || exit 1
    if command -v timeout >/dev/null 2>&1; then
      /usr/bin/time -f 'user=%U rss=%M' -o "$time_file" \
        timeout --signal=TERM --kill-after=15 "$timeout_s" "$@"
    else
      /usr/bin/time -f 'user=%U rss=%M' -o "$time_file" \
        gtimeout --signal=TERM --kill-after=15 "$timeout_s" "$@"
    fi
  ) || status=$?
  return "$status"
}
shutdown_bazel() {
  local base="$1"
  local work="${2:-$(dirname "$base")}"
  if command -v timeout >/dev/null 2>&1; then
    (
      cd "$work" || exit 1
      timeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown
    ) >/dev/null 2>&1 || true
  else
    (
      cd "$work" || exit 1
      gtimeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown
    ) >/dev/null 2>&1 || true
  fi
}

tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/gascity-config-backtest.XXXXXX")"
current_base=""; current_work=""
cleanup() {
  [[ -n "$current_base" ]] && shutdown_bazel "$current_base" "$current_work"
  [[ -n "$current_work" ]] && { chmod -R u+w "$current_work" 2>/dev/null || true; git -C "$repo_root" worktree remove --force "$current_work" >/dev/null 2>&1 || true; }
  if [[ "${BACKTEST_KEEP_ARTIFACTS:-0}" != 1 ]]; then
    chmod -R u+w "$tmp_root" 2>/dev/null || true
    rm -rf -- "$tmp_root" || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

printf 'ref\tsample\tscenario\tstatus\tselection_reason\tconservative\tselection_error\trequested_labels\tconfigured_labels\tcompleted_labels\tconfigured_count\tcompleted_count\tbep_error\twall_s\tclient_user_s\trss_kb\tactions\texecuted\thits\tmisses\tanalysis_ms\tbep_cpu_ms\n'
records="$tmp_root/results.tsv"
printf 'ref\tsample\tscenario\tstatus\tselection_reason\tconservative\tselection_error\trequested_labels\tconfigured_labels\tcompleted_labels\tconfigured_count\tcompleted_count\tbep_error\twall_s\tclient_user_s\trss_kb\tactions\texecuted\thits\tmisses\tanalysis_ms\tbep_cpu_ms\n' >"$records"
emit_row() {
  local row
  row="$(printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s' "$@")"
  printf '%s\n' "$row" | tee -a "$records"
}
emit_skip_row() {
  local ref="$1" sample="$2" scenario="$3" reason="$4" error="$5"
  emit_skip_row_status "$ref" "$sample" "$scenario" skip-go-failed "$reason" "$error"
}
emit_skip_row_status() {
  local ref="$1" sample="$2" scenario="$3" status="$4" reason="$5" error="$6"
  emit_row "$ref" "$sample" "$scenario" "$status" "$reason" true "$error" "" "" "" unknown unknown "" 0 unknown unknown unknown unknown unknown unknown unknown unknown
}
for ref in "${refs[@]}"; do
  resolved="$(git -C "$repo_root" rev-parse --verify "$ref^{commit}")"
  short_ref="${resolved:0:12}"
  work="$tmp_root/work-$short_ref"; git -C "$repo_root" worktree add --detach --quiet "$work" "$resolved"; current_work="$work"
  for rel in "${graph_array[@]}"; do [[ -f "$repo_root/$rel" ]] && { mkdir -p "$work/$(dirname "$rel")"; cp -f "$repo_root/$rel" "$work/$rel"; }; done
  go_cache="$tmp_root/go-cache-$short_ref"
  mkdir -p "$go_cache"
  go_prime_log="$tmp_root/${short_ref}-go-prime.log"
  go_prime_time="$tmp_root/${short_ref}-go-prime.time"
  go_prime_status=0
  if run_timed_in_worktree "$work" "$go_prime_time" env GOCACHE="$go_cache" go test -count=1 "$go_package" >"$go_prime_log" 2>&1; then
    :
  else
    go_prime_status=$?
  fi
  if [[ "$go_prime_status" != 0 ]]; then
    printf 'go-prime\t%s\tstatus=%s\tcache=%s\n' "$short_ref" "$go_prime_status" "$go_cache" >&2
    if [[ -n "$artifact_dir" ]]; then
      mkdir -p "$artifact_dir"
      cp -f "$go_prime_log" "$artifact_dir/go-prime-$short_ref.log"
      if [[ -f "$go_prime_time" ]]; then cp -f "$go_prime_time" "$artifact_dir/go-prime-$short_ref.time"; fi
    fi
    # Preserve a row for every requested sample/scenario so the report makes
    # the non-comparable revision explicit instead of silently dropping it.
    for ((sample=1; sample<=samples; sample++)); do
      for scenario in "${scenario_order[@]}"; do
        emit_skip_row "$short_ref" "$sample" "$scenario" go-prime "go prime failed (status $go_prime_status)"
      done
    done
    git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true
    current_work=""
    continue
  fi
  copy_artifact "$go_prime_log" "go-prime-$short_ref.log"
  copy_artifact "$go_prime_time" "go-prime-$short_ref.time"
  # Snapshot every mutable path under its full relative path. This includes
  # graph files such as MODULE.bazel.lock, which Bazel/module resolution may
  # rewrite, and records absent paths so no scenario contaminates the next.
  pristine="$tmp_root/pristine-${resolved:0:12}"
  mkdir -p "$pristine/files"
  declare -A snapshot_seen=()
  snapshot_paths=("${graph_array[@]}" "$source_file" "$test_file" "$unrelated_file" go.mod)
  : >"$pristine/existing"; : >"$pristine/missing"
  for f in "${snapshot_paths[@]}"; do
    [[ -n "${snapshot_seen[$f]:-}" ]] && continue
    snapshot_seen[$f]=1
    if [[ -f "$work/$f" ]]; then
      mkdir -p "$pristine/files/$(dirname "$f")"
      cp -f "$work/$f" "$pristine/files/$f"
      printf '%s\n' "$f" >>"$pristine/existing"
    else
      printf '%s\n' "$f" >>"$pristine/missing"
    fi
  done
  restore_pristine() {
    local path
    while IFS= read -r path; do
      [[ -n "$path" ]] || continue
      mkdir -p "$(dirname "$work/$path")"
      cp -f "$pristine/files/$path" "$work/$path"
    done <"$pristine/existing"
    while IFS= read -r path; do
      [[ -n "$path" ]] || continue
      rm -f -- "$work/$path"
    done <"$pristine/missing"
  }
  base="$work/bazel-output"; current_base="$base"
  warm_ready=0
  warm_prime_log="$tmp_root/${short_ref}-bazel-prime.log"
  prime_warm_base() {
    [[ "$warm_ready" == 1 ]] && return
    restore_pristine
    local status=0
    run_bounded_in_worktree "$work" "$bazel_bin" --output_base="$base" test --noshow_progress --cache_test_results=no "${target_labels[@]}" >"$warm_prime_log" 2>&1 || status=$?
    [[ "$status" == 0 ]] || return "$status"
    warm_ready=1
  }
  needs_warm=0
  for scenario in "${scenario_order[@]}"; do
    case "$scenario" in go|cold|unrelated-edit) ;; *) needs_warm=1 ;; esac
  done
  if [[ "$needs_warm" == 1 ]]; then
    warm_prime_status=0
    prime_warm_base || warm_prime_status=$?
    if [[ "$warm_prime_status" != 0 ]]; then
      printf 'bazel-prime\t%s\tstatus=%s\tlog=%s\n' "$short_ref" "$warm_prime_status" "$warm_prime_log" >&2
      if [[ -n "$artifact_dir" ]]; then
        mkdir -p "$artifact_dir"
        cp -f "$warm_prime_log" "$artifact_dir/bazel-prime-$short_ref.log"
      fi
      for ((sample=1; sample<=samples; sample++)); do
        for scenario in "${scenario_order[@]}"; do
          emit_skip_row_status "$short_ref" "$sample" "$scenario" skip-bazel-prime bazel-prime "Bazel warm prime failed (status $warm_prime_status)"
        done
      done
      shutdown_bazel "$base" "$work"
      current_base=""
      git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true
      current_work=""
      continue
    fi
    copy_artifact "$warm_prime_log" "bazel-prime-$short_ref.log"
  fi
  for ((sample=1; sample<=samples; sample++)); do
    # Go is always measured first, so a failed baseline cannot leave earlier
    # Bazel rows looking comparable. Rotate only the Bazel scenarios to retain
    # positional bias control for their measurements.
    order=()
    order+=(go)
    bazel_scenarios=("${scenario_order[@]:1}")
    scenario_count="${#bazel_scenarios[@]}"
    if ((scenario_count > 0)); then
      pair="$(((sample - 1) / 2 % scenario_count))"
      if ((sample % 2 == 1)); then
        offset="$pair"
        for ((i=0; i<scenario_count; i++)); do order+=("${bazel_scenarios[$(((offset + i) % scenario_count))]}"); done
      else
        offset="$(((scenario_count - 1 - pair + scenario_count) % scenario_count))"
        for ((i=0; i<scenario_count; i++)); do order+=("${bazel_scenarios[$(((offset - i + scenario_count) % scenario_count))]}"); done
      fi
    fi
    go_failed_sample=0
    for scenario in "${order[@]}"; do
      restore_pristine
      if [[ "$scenario" == go ]]; then
        go_log="$tmp_root/${short_ref}-${sample}-go.log"
        go_time="$go_log.time"
        go_start="$(now_ns)"
        go_status=0
        if run_timed_in_worktree "$work" "$go_time" env GOCACHE="$go_cache" go test -count=1 "$go_package" >"$go_log" 2>&1; then
          :
        else
          go_status=$?
          go_failed_sample=1
        fi
        go_end="$(now_ns)"
        go_wall="$(awk -v n="$((go_end-go_start))" 'BEGIN{printf "%.3f",n/1e9}')"
        go_user="$(sed -n 's/.*user=\([^ ]*\).*/\1/p' "$go_time" | tail -1)"
        go_rss="$(sed -n 's/.*rss=\([^ ]*\).*/\1/p' "$go_time" | tail -1)"
        [[ -n "$go_user" ]] || go_user=unknown
        [[ -n "$go_rss" ]] || go_rss=unknown
        emit_row "$short_ref" "$sample" go "$go_status" go-baseline false "" "" "" "" unknown unknown "" "$go_wall" "$go_user" "$go_rss" unknown unknown unknown unknown unknown unknown
        copy_artifact "$go_log" "$short_ref-$sample-go.log"
        copy_artifact "$go_time" "$short_ref-$sample-go.time"
        continue
      fi
      if [[ "$go_failed_sample" == 1 ]]; then
        emit_skip_row "$short_ref" "$sample" "$scenario" go-baseline "Go baseline failed for sample $sample"
        continue
      fi
      diff_input="$tmp_root/${resolved:0:12}-${sample}-${scenario//[^a-zA-Z0-9]/_}.diff"
      case "$scenario" in
        cold|forced|no-op) printf 'M\0MODULE.bazel\0' >"$diff_input";;
        source-edit) printf 'M\0%s\0' "$source_file" >"$diff_input";;
        test-edit) printf 'M\0%s\0' "$test_file" >"$diff_input";;
        unrelated-edit) printf 'M\0%s\0' "$unrelated_file" >"$diff_input";;
        go-mod) printf 'M\0go.mod\0' >"$diff_input";;
      esac
      if ! selection_json="$(python3 "$resolver" resolve "$diff_input" --format json)"; then
        # Resolver/tool failures are fail-closed: run all configured targets
        # with an explicit unavailable reason rather than silently selecting 0.
        selection_json="$(python3 - "$target_labels_csv" <<'PY'
import json,sys
print(json.dumps({"labels":sys.argv[1].split(","),"conservative":True,"reason":"unavailable","error":"resolver failed"}))
PY
        )"
      fi
      selection_reason="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["reason"])' <<<"$selection_json")"
      conservative="$(python3 -c 'import json,sys; print(str(json.load(sys.stdin)["conservative"]).lower())' <<<"$selection_json")"
      selection_error="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("error") or "")' <<<"$selection_json")"
      mapfile -t selected_labels < <(python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["labels"]))' <<<"$selection_json")
      if ((${#selected_labels[@]} == 1)) && [[ -z "${selected_labels[0]}" ]]; then selected_labels=(); fi
      if [[ "$scenario" == cold || "$scenario" == forced || "$scenario" == no-op ]]; then selection_reason=baseline; conservative=true; fi
      requested_labels="$(IFS=,; echo "${selected_labels[*]}")"
      if ((${#selected_labels[@]} == 0)); then
        selection_error_escaped="$(tsv_escape "$selection_error")"
        emit_row "$short_ref" "$sample" "$scenario" not-run "$selection_reason" "$conservative" "$selection_error_escaped" "$requested_labels" "" "" unknown unknown "" 0 0 0 0 0 0 0 unknown unknown
        continue
      fi
      [[ "$scenario" == cold ]] || prime_warm_base
      case "$scenario" in
        source-edit) printf '\n// bazel backtest source edit sample=%s\n' "$sample" >>"$work/$source_file";;
        test-edit) printf '\n// bazel backtest test edit sample=%s\n' "$sample" >>"$work/$test_file";;
        unrelated-edit) mkdir -p "$(dirname "$work/$unrelated_file")"; printf '\n<!-- bazel backtest unrelated edit sample=%s -->\n' "$sample" >>"$work/$unrelated_file";;
        go-mod) printf '\n// bazel backtest module edit sample=%s\n' "$sample" >>"$work/go.mod";;
      esac
      out="$tmp_root/${resolved:0:12}-${sample}-${scenario//[^a-zA-Z0-9]/_}.log"; bep="$out.bep.json"; rm -f "$bep"
      run_base="$base"
      if [[ "$scenario" == cold ]]; then run_base="$work/cold-output-$sample"; fi
      current_base="$run_base"
      cache_results=no; [[ "$scenario" == no-op ]] && cache_results=yes
      cmd=("$bazel_bin" --output_base="$run_base" test --noshow_progress --build_event_json_file="$bep" --cache_test_results="$cache_results" "${selected_labels[@]}")
      start="$(now_ns)"; status=0
      (
        cd "$work" || exit 1
        if command -v timeout >/dev/null 2>&1; then
          /usr/bin/time -f 'user=%U rss=%M' -o "$out.time" timeout --signal=TERM --kill-after=15 "$timeout_s" "${cmd[@]}"
        else
          /usr/bin/time -f 'user=%U rss=%M' -o "$out.time" gtimeout --signal=TERM --kill-after=15 "$timeout_s" "${cmd[@]}"
        fi
      ) >"$out" 2>&1 || status=$?
      end="$(now_ns)"
      wall="$(awk -v n="$((end-start))" 'BEGIN{printf "%.3f",n/1e9}')"; user="$(sed -n 's/.*user=\([^ ]*\).*/\1/p' "$out.time" | tail -1)"; rss="$(sed -n 's/.*rss=\([^ ]*\).*/\1/p' "$out.time" | tail -1)"; [[ -n "$user" ]] || user=unknown; [[ -n "$rss" ]] || rss=unknown
      metrics="$(python3 - "$bep" <<'PY'
import json,sys
a=e=hi=mi=analysis=cpu='unknown'; selected='unknown'
try:
  for line in open(sys.argv[1],encoding='utf-8'):
    try: j=json.loads(line)
    except Exception: continue
    bm=j.get('buildMetrics',{}); s=bm.get('actionSummary',{})
    if s:
      a=s.get('actionsCreated',0); e=s.get('actionsExecuted',0); c=s.get('actionCacheStatistics',{}); hi=c.get('hitCount',c.get('hits',0)); mi=c.get('missCount',c.get('misses',0))
    tm=bm.get('timingMetrics',{})
    analysis=tm.get('analysisPhaseTimeInMs',analysis)
    cpu=tm.get('cpuTimeInMs',cpu)
    # BEP configured events include transitive dependencies; without a
    # target-pattern correlation, reporting a numeric selected count would be
    # misleading. Keep this metric explicitly unknown until that mapping is
    # implemented.
except FileNotFoundError: pass
print(a,e,hi,mi,analysis,cpu,selected)
PY
      )"; read -r actions executed hits misses analysis bep_cpu _selected <<<"$metrics"
      label_flags=(); for label in "${selected_labels[@]}"; do label_flags+=(--label "$label"); done
      bep_stderr="$out.bep.err"; bep_status=0
      if bep_json="$(python3 "$resolver" bep "$bep" --format json "${label_flags[@]}" 2>"$bep_stderr")"; then :; else bep_status=$?; fi
      bep_payload="$bep_json"; [[ -n "$bep_payload" ]] || bep_payload='{}'
      configured_labels="$(python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("configured",[])))' <<<"$bep_payload")"
      completed_labels="$(python3 -c 'import json,sys; print(",".join(json.load(sys.stdin).get("completed",[])))' <<<"$bep_payload")"
      configured_count="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("configured_count") or "unknown")' <<<"$bep_payload")"
      completed_count="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("completed_count") or "unknown")' <<<"$bep_payload")"
      bep_error="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("error") or "")' <<<"$bep_payload")"
      [[ "$bep_status" == 0 ]] || bep_error="resolver_failed: $(head -c 256 "$bep_stderr")"
      if [[ -n "$bep_error" ]]; then row_status='bep-error'; else row_status="$status"; fi
      selection_error_escaped="$(tsv_escape "$selection_error")"; bep_error_escaped="$(tsv_escape "$bep_error")"
      emit_row "$short_ref" "$sample" "$scenario" "$row_status" "$selection_reason" "$conservative" "$selection_error_escaped" "$requested_labels" "$configured_labels" "$completed_labels" "$configured_count" "$completed_count" "$bep_error_escaped" "$wall" "$user" "$rss" "$actions" "$executed" "$hits" "$misses" "$analysis" "$bep_cpu"
      artifact_stem="$short_ref-$sample-${scenario//[^a-zA-Z0-9]/_}"
      copy_artifact "$out" "$artifact_stem.log"
      copy_artifact "$out.time" "$artifact_stem.time"
      copy_artifact "$bep" "$artifact_stem.bep.json"
      copy_artifact "$bep_stderr" "$artifact_stem.bep.err"
      if [[ "$scenario" == cold ]]; then
        shutdown_bazel "$run_base" "$work"
        case "$run_base" in
          "$work"/cold-output-[0-9]*)
            # Bazel's sandbox/toolchain outputs can be read-only even after
            # the server exits. Restore owner write permission before removing
            # this harness-owned, disposable output base; never broaden the
            # deletion match beyond the validated worktree-local path.
            chmod -R u+w -- "$run_base" 2>/dev/null || true
            rm -rf -- "$run_base"
            ;;
        esac
        current_base="$base"
      fi
    done
  done
  shutdown_bazel "$base" "$work"; current_base=""; git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true; current_work=""
done

copy_artifact "$records" results.tsv

summary_records="$tmp_root/summary.tsv"
python3 - "$records" "$samples" "$scenario_csv" >"$summary_records" <<'PY'
import statistics, sys
expected_samples = int(sys.argv[2])
expected_scenarios = {"go"}
expected_scenarios.update(filter(None, sys.argv[3].split(",")))
rows = []
with open(sys.argv[1], encoding="utf-8") as stream:
    for line in stream:
        if line.startswith("ref\t") or not line.strip():
            continue
        fields = line.rstrip("\n").split("\t")
        if len(fields) >= 14:
            rows.append(fields)
for ref in sorted({row[0] for row in rows}):
    for scenario in sorted(expected_scenarios | {row[2] for row in rows if row[0] == ref}):
        selected = [row for row in rows if row[0] == ref and row[2] == scenario]
        successful = [float(row[13]) for row in selected if row[3] == "0"]
        skipped = sum(1 for row in selected if row[3] == "not-run" or row[3].startswith("skip-"))
        failures = len(selected) - len(successful) - skipped
        diagnostics = []
        valid = len(selected) == expected_samples
        if not valid:
            diagnostics.append(f"expected {expected_samples} rows, got {len(selected)}")
        if scenario == "unrelated-edit":
            if len(selected) != expected_samples or any(row[3] != "not-run" for row in selected):
                valid = False
                diagnostics.append("unrelated changes must be skipped")
        else:
            if len(successful) != expected_samples:
                valid = False
                diagnostics.append(f"expected {expected_samples} successful rows, got {len(successful)}")
            for row in selected:
                if row[12]:
                    valid = False
                    diagnostics.append("non-empty BEP error")
                if row[3] == "0" and scenario != "go" and not (row[7] == row[8] == row[9]):
                    valid = False
                    diagnostics.append("requested/configured/completed labels differ")
        if failures:
            valid = False
            diagnostics.append(f"{failures} failed rows")
        p50 = f"{statistics.median(successful):.3f}" if successful else "NA"
        p95 = (f"{statistics.quantiles(successful, n=20, method='inclusive')[18]:.3f}"
               if len(successful) > 1 else (f"{successful[0]:.3f}" if successful else "NA"))
        detail = "; ".join(dict.fromkeys(diagnostics))[:240]
        print(f"summary\t{ref}\t{scenario}\tattempted={len(successful)+failures}\tsuccess={len(successful)}\tfailure={failures}\tskipped={skipped}\tp50_s={p50}\tp95_s={p95}\tvalid={str(valid).lower()}\tdiagnostics={detail}")
PY
cat "$summary_records"
copy_artifact "$summary_records" summary.tsv
