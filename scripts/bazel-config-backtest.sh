#!/usr/bin/env bash
# Measure selective Bazel scenarios against Go for a configurable target.
#
# This is intentionally a research harness: it does not alter CI or claim
# that historical revisions carried the current BUILD graph.  The graph files
# are copied from the checkout under test, while source edits are disposable.
# Defaults preserve the original contract pilot target. For config runs set
# BACKTEST_TARGET=//internal/config:config_envname_test and point the source,
# test, and graph-file variables at the bounded config BUILD slice.
# Linux with GNU `/usr/bin/time -f` and `timeout` (or `gtimeout`) is required.
# Set BACKTEST_SAMPLES, BACKTEST_TIMEOUT, and BACKTEST_SCENARIOS to tune runs;
# pass one or more git refs as positional arguments.
set -euo pipefail

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,12p' "$0"
  exit 0
fi

repo_root="$(git rev-parse --show-toplevel)"
bazel_bin="${BAZEL_BIN:-$(command -v bazelisk || command -v bazel || true)}"
samples="${BACKTEST_SAMPLES:-20}"
timeout_s="${BACKTEST_TIMEOUT:-300}"
target="${BACKTEST_TARGET:-//internal/beads/contract:contract_test}"
go_package="${BACKTEST_GO_PACKAGE:-./internal/beads/contract}"
source_file="${BACKTEST_SOURCE_FILE:-internal/beads/contract/metadata.go}"
test_file="${BACKTEST_TEST_FILE:-internal/beads/contract/metadata_test.go}"
unrelated_file="${BACKTEST_UNRELATED_FILE:-docs/README.md}"
graph_files="${BACKTEST_GRAPH_FILES:-MODULE.bazel MODULE.bazel.lock .bazelrc BUILD.bazel internal/beads/contract/BUILD.bazel internal/beadmeta/BUILD.bazel internal/fsys/BUILD.bazel internal/pidutil/BUILD.bazel internal/testenv/BUILD.bazel internal/config/BUILD.bazel internal/config/config_envname_bazel_test.go internal/config/diagnostic_locations_test.go}"
scenario_csv="${BACKTEST_SCENARIOS:-cold,forced,no-op,source-edit,test-edit,unrelated-edit,go-mod}"
if [[ -z "$bazel_bin" ]]; then echo "set BAZEL_BIN or install bazelisk" >&2; exit 127; fi
[[ "$(uname -s)" == Linux ]] || { echo "Linux is required for this harness (GNU timing semantics)" >&2; exit 2; }
if ! command -v timeout >/dev/null 2>&1 && ! command -v gtimeout >/dev/null 2>&1; then echo "GNU timeout (or gtimeout) is required for bounded runs" >&2; exit 127; fi
[[ -x /usr/bin/time ]] || { echo "/usr/bin/time is required" >&2; exit 127; }
if ! /usr/bin/time -f '%e' true >/dev/null 2>&1; then echo "/usr/bin/time does not support GNU -f; install GNU time" >&2; exit 127; fi
[[ "$samples" =~ ^[1-9][0-9]*$ ]] || { echo "BACKTEST_SAMPLES must be positive" >&2; exit 2; }
[[ "$samples" -le 1000 ]] || { echo "BACKTEST_SAMPLES must be <= 1000" >&2; exit 2; }
[[ "$target" == //*:* ]] || { echo "BACKTEST_TARGET must look like //pkg:name" >&2; exit 2; }

IFS=',' read -r -a scenarios <<<"$scenario_csv"
allowed=" cold forced no-op source-edit test-edit unrelated-edit go-mod "
for s in "${scenarios[@]}"; do [[ "$allowed" == *" $s "* ]] || { echo "unknown scenario: $s" >&2; exit 2; }; done
refs=("$@")
if ((${#refs[@]} == 0)); then refs=("7c33f3f7f1" "128bd64033" "a784438ce0" "58a47d6bdc"); fi

now_ns() {
  if command -v perl >/dev/null 2>&1; then perl -MTime::HiRes=time -e 'printf "%.0f\n", time()*1e9'; return; fi
  local stamp; stamp="$(date +%s%N)"
  [[ "$stamp" =~ ^[0-9]+$ ]] && printf '%s\n' "$stamp" || printf '%s000000000\n' "$(date +%s)"
}
shutdown_bazel() { "$bazel_bin" --output_base="$1" shutdown >/dev/null 2>&1 || true; }

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

printf 'ref\tsample\tscenario\tstatus\twall_s\tuser_s\trss_kb\tactions\texecuted\thits\tmisses\tanalysis_ms\tselected\n'
records="$tmp_root/results.tsv"
printf 'ref\tsample\tscenario\tstatus\twall_s\tuser_s\trss_kb\tactions\texecuted\thits\tmisses\tanalysis_ms\tselected\n' >"$records"
for ref in "${refs[@]}"; do
  resolved="$(git -C "$repo_root" rev-parse --verify "$ref^{commit}")"
  work="$tmp_root/work-${resolved:0:12}"; git -C "$repo_root" worktree add --detach --quiet "$work" "$resolved"; current_work="$work"
  for rel in $graph_files; do [[ -f "$repo_root/$rel" ]] && { mkdir -p "$work/$(dirname "$rel")"; cp -f "$repo_root/$rel" "$work/$rel"; }; done
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
  # Preserve files so each scenario starts from the same source state.
  for f in "$source_file" "$test_file" "$unrelated_file" go.mod; do [[ -f "$work/$f" ]] && cp -f "$work/$f" "$tmp_root/$(basename "$f").orig"; done
  base="$work/bazel-output"; current_base="$base"
  warm_ready=0
  for ((sample=1; sample<=samples; sample++)); do
    order=("${scenarios[@]}"); if ((sample % 2 == 0)); then order=(); for ((i=${#scenarios[@]}-1;i>=0;i--)); do order+=("${scenarios[$i]}"); done; fi
    for scenario in "${order[@]}"; do
      cp -f "$tmp_root/$(basename "$source_file").orig" "$work/$source_file" 2>/dev/null || true
      cp -f "$tmp_root/$(basename "$test_file").orig" "$work/$test_file" 2>/dev/null || true
      cp -f "$tmp_root/$(basename "$unrelated_file").orig" "$work/$unrelated_file" 2>/dev/null || true
      cp -f "$tmp_root/go.mod.orig" "$work/go.mod" 2>/dev/null || true
      case "$scenario" in
        source-edit) printf '\n// bazel backtest source edit\n' >>"$work/$source_file";;
        test-edit) printf '\n// bazel backtest test edit\n' >>"$work/$test_file";;
        unrelated-edit) mkdir -p "$(dirname "$work/$unrelated_file")"; printf '\n<!-- bazel backtest unrelated edit -->\n' >>"$work/$unrelated_file";;
        go-mod) printf '\n// bazel backtest module edit\n' >>"$work/go.mod";;
      esac
      out="$tmp_root/${resolved:0:12}-${sample}-${scenario//[^a-zA-Z0-9]/_}.log"; bep="$out.bep.json"; rm -f "$bep"
      run_base="$base"
      if [[ "$scenario" == cold ]]; then run_base="$work/cold-output-$sample"; fi
      if [[ "$scenario" != cold && "$warm_ready" == 0 ]]; then
        # Prime the persistent warm server/cache outside measured scenarios.
        if command -v timeout >/dev/null 2>&1; then timeout --signal=TERM --kill-after=15 "$timeout_s" "$bazel_bin" --output_base="$base" test --noshow_progress --cache_test_results=no "$target" >/dev/null 2>&1; else gtimeout --signal=TERM --kill-after=15 "$timeout_s" "$bazel_bin" --output_base="$base" test --noshow_progress --cache_test_results=no "$target" >/dev/null 2>&1; fi
        warm_ready=1
      fi
      cache_results=no; [[ "$scenario" == no-op ]] && cache_results=yes
      cmd=("$bazel_bin" --output_base="$run_base" test --noshow_progress --build_event_json_file="$bep" --cache_test_results="$cache_results" "$target")
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
a=e=hi=mi=analysis='unknown'; selected='unknown'
try:
  for line in open(sys.argv[1],encoding='utf-8'):
    try: j=json.loads(line)
    except Exception: continue
    bm=j.get('buildMetrics',{}); s=bm.get('actionSummary',{})
    if s:
      a=s.get('actionsCreated',0); e=s.get('actionsExecuted',0); c=s.get('actionCacheStatistics',{}); hi=c.get('hitCount',c.get('hits',0)); mi=c.get('missCount',c.get('misses',0))
    am=bm.get('analysisMetrics',{}); analysis=am.get('totalTimeInMs',am.get('analysisTimeInMs',analysis))
    # BEP configured events include transitive dependencies; without a
    # target-pattern correlation, reporting a numeric selected count would be
    # misleading. Keep this metric explicitly unknown until that mapping is
    # implemented.
except FileNotFoundError: pass
print(a,e,hi,mi,analysis,selected)
PY
      )"; read -r actions executed hits misses analysis selected <<<"$metrics"
      row="$(printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s' "${resolved:0:12}" "$sample" "$scenario" "$status" "$wall" "$user" "$rss" "$actions" "$executed" "$hits" "$misses" "$analysis" "$selected")"
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
    if len(p) >= 5:
        try: rows.append((p[0], p[2], float(p[4])))
        except ValueError: pass
for ref in sorted({r[0] for r in rows}):
  for scenario in sorted({r[1] for r in rows if r[0] == ref}):
    vals = [r[2] for r in rows if r[0] == ref and r[1] == scenario]
    failures = sum(1 for line in open(sys.argv[1], encoding='utf-8')
                   if line.startswith(ref+'\t') and ('\t'+scenario+'\t') in line and line.split('\t')[3] != '0')
    if vals:
      p95 = statistics.quantiles(vals, n=20, method='inclusive')[18] if len(vals)>1 else vals[0]
      print(f"summary\t{ref}\t{scenario}\tn={len(vals)}\tfailures={failures}\tp50_s={statistics.median(vals):.3f}\tp95_s={p95:.3f}")
PY
