#!/usr/bin/env bash
# Measure selective Bazel scenarios against Go for a configurable target.
#
# This is intentionally a research harness: it does not alter CI or claim
# that historical revisions carried the current BUILD graph.  The graph files
# are copied from the checkout under test, while source edits are disposable.
# Defaults run the three bounded config pilot targets. Set BACKTEST_TARGETS to
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
target_labels_csv="${BACKTEST_TARGETS:-//internal/config:config_envname_test,//internal/config:config_diagnostic_locations_test,//internal/config:config_storage_endpoint_test}"
go_package="${BACKTEST_GO_PACKAGE:-./internal/config}"
source_file="${BACKTEST_SOURCE_FILE:-internal/config/storage_endpoint.go}"
test_file="${BACKTEST_TEST_FILE:-internal/config/storage_endpoint_bazel_test.go}"
unrelated_file="${BACKTEST_UNRELATED_FILE:-docs/README.md}"
graph_files="${BACKTEST_GRAPH_FILES:-MODULE.bazel MODULE.bazel.lock .bazelrc BUILD.bazel internal/beads/contract/BUILD.bazel internal/beadmeta/BUILD.bazel internal/fsys/BUILD.bazel internal/pidutil/BUILD.bazel internal/testenv/BUILD.bazel internal/config/BUILD.bazel internal/config/config_envname_bazel_test.go internal/config/diagnostic_locations_test.go internal/config/storage_endpoint_bazel_test.go}"
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
[[ "${#target_labels[@]}" -eq 3 ]] || { echo "BACKTEST_TARGETS must contain the three config pilot labels" >&2; exit 2; }
for expected in //internal/config:config_envname_test //internal/config:config_diagnostic_locations_test //internal/config:config_storage_endpoint_test; do
  printf '%s\n' "${target_labels[@]}" | grep -Fxq "$expected" || { echo "BACKTEST_TARGETS missing required label: $expected" >&2; exit 2; }
done

IFS=',' read -r -a scenarios <<<"$scenario_csv"
read -r -a graph_array <<<"$graph_files"
allowed=" cold forced no-op source-edit test-edit unrelated-edit go-mod "
for s in "${scenarios[@]}"; do [[ "$allowed" == *" $s "* ]] || { echo "unknown scenario: $s" >&2; exit 2; }; done
refs=("$@")
if ((${#refs[@]} == 0)); then refs=("7c33f3f7f1" "128bd64033" "a784438ce0" "58a47d6bdc"); fi

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
shutdown_bazel() {
  local base="$1"
  if command -v timeout >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown >/dev/null 2>&1 || true
  else
    gtimeout --signal=TERM --kill-after=3 10 "$bazel_bin" --output_base="$base" shutdown >/dev/null 2>&1 || true
  fi
}

tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/gascity-config-backtest.XXXXXX")"
current_base=""; current_work=""
cleanup() {
  [[ -n "$current_base" ]] && shutdown_bazel "$current_base"
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
for ref in "${refs[@]}"; do
  resolved="$(git -C "$repo_root" rev-parse --verify "$ref^{commit}")"
  work="$tmp_root/work-${resolved:0:12}"; git -C "$repo_root" worktree add --detach --quiet "$work" "$resolved"; current_work="$work"
  for rel in "${graph_array[@]}"; do [[ -f "$repo_root/$rel" ]] && { mkdir -p "$work/$(dirname "$rel")"; cp -f "$repo_root/$rel" "$work/$rel"; }; done
  go_log="$tmp_root/${resolved:0:12}-go.log"; go_start="$(now_ns)"; go_status=0
  if command -v timeout >/dev/null 2>&1; then timeout --signal=TERM --kill-after=15 "$timeout_s" go -C "$work" test -count=1 "$go_package" >"$go_log" 2>&1 || go_status=$?; else gtimeout --signal=TERM --kill-after=15 "$timeout_s" go -C "$work" test -count=1 "$go_package" >"$go_log" 2>&1 || go_status=$?; fi
  go_end="$(now_ns)"; go_wall="$(awk -v n="$((go_end-go_start))" 'BEGIN{printf "%.3f",n/1e9}')"
  printf 'go\t%s\tstatus=%s\twall_s=%s\tpackage=%s\n' "${resolved:0:12}" "$go_status" "$go_wall" "$go_package"
  if [[ "$go_status" != 0 ]]; then
    printf 'skip\t%s\tgo baseline failed or timed out; Bazel rows are non-comparable\n' "${resolved:0:12}" >&2
    git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true
    current_work=""
    continue
  fi
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
  prime_warm_base() {
    [[ "$warm_ready" == 1 ]] && return
    restore_pristine
    if command -v timeout >/dev/null 2>&1; then
      timeout --signal=TERM --kill-after=15 "$timeout_s" "$bazel_bin" --output_base="$base" test --noshow_progress --cache_test_results=no "${target_labels[@]}" >/dev/null 2>&1
    else
      gtimeout --signal=TERM --kill-after=15 "$timeout_s" "$bazel_bin" --output_base="$base" test --noshow_progress --cache_test_results=no "${target_labels[@]}" >/dev/null 2>&1
    fi
    warm_ready=1
  }
  for ((sample=1; sample<=samples; sample++)); do
    order=("${scenarios[@]}"); if ((sample % 2 == 0)); then order=(); for ((i=${#scenarios[@]}-1;i>=0;i--)); do order+=("${scenarios[$i]}"); done; fi
    for scenario in "${order[@]}"; do
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
        row="$(printf '%s\t%s\t%s\tnot-run\t%s\t%s\t%s\t%s\t\t\tunknown\tunknown\t\t0\t0\t0\t0\t0\t0\t0\tunknown\tunknown' "${resolved:0:12}" "$sample" "$scenario" "$selection_reason" "$conservative" "$selection_error_escaped" "$requested_labels")"
        printf '%s\n' "$row" | tee -a "$records"
        continue
      fi
      [[ "$scenario" == cold ]] || prime_warm_base
      restore_pristine
      case "$scenario" in
        source-edit) printf '\n// bazel backtest source edit\n' >>"$work/$source_file";;
        test-edit) printf '\n// bazel backtest test edit\n' >>"$work/$test_file";;
        unrelated-edit) mkdir -p "$(dirname "$work/$unrelated_file")"; printf '\n<!-- bazel backtest unrelated edit -->\n' >>"$work/$unrelated_file";;
        go-mod) printf '\n// bazel backtest module edit\n' >>"$work/go.mod";;
      esac
      out="$tmp_root/${resolved:0:12}-${sample}-${scenario//[^a-zA-Z0-9]/_}.log"; bep="$out.bep.json"; rm -f "$bep"
      run_base="$base"
      if [[ "$scenario" == cold ]]; then run_base="$work/cold-output-$sample"; fi
      cache_results=no; [[ "$scenario" == no-op ]] && cache_results=yes
      cmd=("$bazel_bin" --output_base="$run_base" test --noshow_progress --build_event_json_file="$bep" --cache_test_results="$cache_results" "${selected_labels[@]}")
      start="$(now_ns)"; status=0
      if command -v timeout >/dev/null 2>&1; then
        /usr/bin/time -f 'user=%U rss=%M' -o "$out.time" timeout --signal=TERM --kill-after=15 "$timeout_s" "${cmd[@]}" >"$out" 2>&1 || status=$?
      else
        /usr/bin/time -f 'user=%U rss=%M' -o "$out.time" gtimeout --signal=TERM --kill-after=15 "$timeout_s" "${cmd[@]}" >"$out" 2>&1 || status=$?
      fi
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
      row="$(printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s' "${resolved:0:12}" "$sample" "$scenario" "$row_status" "$selection_reason" "$conservative" "$selection_error_escaped" "$requested_labels" "$configured_labels" "$completed_labels" "$configured_count" "$completed_count" "$bep_error_escaped" "$wall" "$user" "$rss" "$actions" "$executed" "$hits" "$misses" "$analysis" "$bep_cpu")"
      printf '%s\n' "$row" | tee -a "$records"
      [[ "$scenario" == cold ]] && shutdown_bazel "$run_base"
    done
  done
  shutdown_bazel "$base"; current_base=""; git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || true; current_work=""
done

python3 - "$records" <<'PY'
import statistics, sys
rows = []
for line in open(sys.argv[1], encoding='utf-8'):
    if line.startswith('ref\t') or not line.strip(): continue
    p = line.rstrip('\n').split('\t')
    if len(p) >= 14:
        try: rows.append((p[0], p[2], float(p[13])))
        except ValueError: pass
for ref in sorted({r[0] for r in rows}):
  for scenario in sorted({r[1] for r in rows if r[0] == ref}):
    vals = [r[2] for r in rows if r[0] == ref and r[1] == scenario]
    failures = sum(1 for line in open(sys.argv[1], encoding='utf-8')
                   if line.startswith(ref+'\t') and ('\t'+scenario+'\t') in line
                   and line.split('\t')[3] not in {'0', 'not-run'})
    not_run = sum(1 for line in open(sys.argv[1], encoding='utf-8')
                  if line.startswith(ref+'\t') and ('\t'+scenario+'\t') in line
                  and line.split('\t')[3] == 'not-run')
    if vals:
      p95 = statistics.quantiles(vals, n=20, method='inclusive')[18] if len(vals)>1 else vals[0]
      print(f"summary\t{ref}\t{scenario}\tn={len(vals)}\tfailures={failures}\tnot_run={not_run}\tp50_s={statistics.median(vals):.3f}\tp95_s={p95:.3f}")
PY
