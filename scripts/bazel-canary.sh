#!/usr/bin/env bash
# Run the bounded Bazel config canary after resolving changed PR paths.
#
# This is intentionally a non-blocking evidence lane. Go remains the parity
# authority, and every resolver/BEP failure fails closed rather than silently
# dropping coverage. The script writes all evidence below $OUT.
set -uo pipefail

mode="${1:-run}"
if [[ "$mode" != resolve && "$mode" != run ]]; then
  echo "usage: $0 [resolve|run]" >&2
  exit 2
fi

all_labels=(
  //internal/config:config_diagnostic_locations_test
  //internal/config:config_envname_test
  //internal/config:config_storage_endpoint_test
)

out="${OUT:?OUT must name an artifact directory}"
resolver="${RESOLVER:-scripts/bazel_target_resolver.py}"
event_name="${EVENT_NAME:-}"
base_sha="${PR_BASE_SHA:-}"
head_sha="${PR_HEAD_SHA:-}"
mkdir -p "$out"
summary="$out/summary.txt"
selection="$out/selection.resolver.json"
normalized="$out/selection.normalized.json"
diff_file="$out/changed.name-status.z"
changed_paths_file="${CHANGED_PATHS_FILE:-}"
selection_reason="unavailable"
selection_conservative=true
selection_error=""
selected=()
cache_root="${CACHE_ROOT:-${out}.cache}"
repository_cache="$cache_root/repository-cache"
bazelisk_home="$cache_root/bazelisk-home"
output_base="$cache_root/output-base"

sanitize() {
  # Resolver diagnostics can originate in git/Python. Keep them single-line
  # and bounded before writing GitHub's line-oriented output protocol.
  printf '%s' "${1:-}" | tr '\r\n\t' '   ' | cut -c1-240
}

write_summary_selection() {
  local labels_csv
  labels_csv="$(IFS=,; printf '%s' "${selected[*]}")"
  {
    echo "selection_reason=$selection_reason"
    echo "selection_conservative=$selection_conservative"
    echo "selection_error=$selection_error"
    echo "requested_labels=$labels_csv"
    echo "requested_count=${#selected[@]}"
    echo "graph_metrics_scope=graph-wide"
    echo "remote_cache=disabled (explicit empty URL)"
    echo "output_base=$output_base"
    echo "repository_cache=$repository_cache"
  } >"$summary"
}

write_summary_not_run() {
  {
    echo "configured_labels="
    echo "configured_count=0"
    echo "completed_labels="
    echo "completed_count=0"
    echo "bep_error="
    echo "status=$1"
  } >>"$summary"
}

fail_closed() {
  selected=("${all_labels[@]}")
  selection_conservative=true
  selection_reason="${1:-unavailable}"
  selection_error="${2:-resolver unavailable}"
}

# Build the changed-path input from the PR merge base. The first workflow
# invocation persists a normalized selection; the execution invocation consumes
# that exact file so a second resolver run cannot silently change the request.
if [[ "$mode" == run && -s "$normalized" ]]; then
  selection="$normalized"
elif [[ -n "$changed_paths_file" ]]; then
  if ! cp -f -- "$changed_paths_file" "$diff_file" 2>"$out/diff.stderr"; then
    fail_closed unavailable "unable to read supplied changed-path input"
  elif ! python3 "$resolver" resolve "$diff_file" --format json >"$selection" 2>"$out/resolver.stderr"; then
    fail_closed unavailable "changed-file resolver failed"
  fi
elif [[ "$event_name" == pull_request ]]; then
  if [[ ! "$base_sha" =~ ^[0-9a-fA-F]{40}$ || ! "$head_sha" =~ ^[0-9a-fA-F]{40}$ ]]; then
    fail_closed unavailable "pull request base/head SHA unavailable"
  else
    # checkout@v6 normally includes both commits with fetch-depth: 0. A head
    # from a fork may still be absent from the origin refspec, so make one
    # bounded best-effort fetch before falling back conservatively.
    if ! git cat-file -e "$base_sha^{commit}" >/dev/null 2>&1 || \
       ! git cat-file -e "$head_sha^{commit}" >/dev/null 2>&1; then
      timeout --signal=TERM --kill-after=5 30 git fetch --no-tags --no-write-fetch-head origin "$base_sha" "$head_sha" \
        >"$out/fetch.log" 2>&1 || true
    fi
    if ! git cat-file -e "$base_sha^{commit}" >/dev/null 2>&1 || \
       ! git cat-file -e "$head_sha^{commit}" >/dev/null 2>&1; then
      fail_closed unavailable "pull request base/head commit unavailable"
    elif ! git diff --name-status -z "$base_sha...$head_sha" >"$diff_file" 2>"$out/diff.stderr"; then
      fail_closed unavailable "unable to compute pull request diff"
    elif ! python3 "$resolver" resolve "$diff_file" --format json >"$selection" 2>"$out/resolver.stderr"; then
      fail_closed unavailable "changed-file resolver failed"
    fi
  fi
else
  fail_closed manual "workflow_dispatch has no pull request diff"
fi

if ((${#selected[@]} == 0)) && [[ -s "$selection" ]]; then
  # Validate the helper's shape and labels before trusting it. Empty labels are
  # allowed only for the explicit unrelated result; malformed/unknown output
  # falls back to all three targets.
  if ! readarray -t parsed < <(python3 - "$selection" <<'PY'
import json
import pathlib
import sys

allowed = {
    "//internal/config:config_diagnostic_locations_test",
    "//internal/config:config_envname_test",
    "//internal/config:config_storage_endpoint_test",
}
try:
    payload = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
    labels = payload["labels"]
    reason = payload["reason"]
    conservative = payload["conservative"]
    if not isinstance(labels, list) or any(not isinstance(x, str) or x not in allowed for x in labels):
        raise ValueError("malformed resolver labels")
    if len(labels) != len(set(labels)) or not isinstance(reason, str) or not isinstance(conservative, bool):
        raise ValueError("malformed resolver result")
    if not labels and (reason != "unrelated" or conservative):
        raise ValueError("empty selection must be explicitly non-conservative unrelated")
    if labels and reason == "unrelated":
        raise ValueError("unrelated selection must not contain labels")
    print("OK")
    print(reason)
    print(str(conservative).lower())
    print(payload.get("error") or "")
    print("\n".join(sorted(labels)))
except (OSError, UnicodeDecodeError, json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
    print("ERROR")
    print("unavailable")
    print("true")
    print(str(exc))
PY
  ); then
    fail_closed unavailable "resolver output validation failed"
  elif [[ "${parsed[0]:-ERROR}" != OK ]]; then
    fail_closed unavailable "${parsed[3]:-malformed resolver output}"
  else
    selection_reason="$(sanitize "${parsed[1]}")"
    selection_conservative="${parsed[2]}"
    selection_error="$(sanitize "${parsed[3]}")"
    if ((${#parsed[@]} > 4)); then
      mapfile -t selected < <(printf '%s\n' "${parsed[@]:4}" | sed '/^$/d')
    fi
  fi
fi

selection_reason="$(sanitize "$selection_reason")"
selection_error="$(sanitize "$selection_error")"

printf '{"labels":[' >"$normalized"
for label in "${selected[@]}"; do printf '%s' "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$label")" >>"$normalized"; [[ "$label" == "${selected[-1]}" ]] || printf ',' >>"$normalized"; done
printf '],"conservative":%s,"reason":%s,"error":%s}\n' \
  "$selection_conservative" \
  "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$selection_reason")" \
  "$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1] or None))' "$selection_error")" >>"$normalized"
write_summary_selection

labels_csv="$(IFS=,; printf '%s' "${selected[*]}")"
run_bazel=false
if ((${#selected[@]} > 0)); then run_bazel=true; fi
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  {
    echo "labels_csv=$labels_csv"
    echo "run_bazel=$run_bazel"
    echo "reason=$selection_reason"
    echo "conservative=$selection_conservative"
    echo "selection_error=$selection_error"
  } >>"$GITHUB_OUTPUT"
fi

# The workflow invokes this script once before installing Bazel to decide
# whether the canary is relevant. That phase must never attempt toolchain work;
# the run phase repeats the same pure resolution just before executing tests.
if [[ "$mode" == resolve ]]; then
  write_summary_not_run "$([[ "$run_bazel" == true ]] && echo selected || echo skipped)"
  exit 0
fi

if [[ "$run_bazel" != true ]]; then
  write_summary_not_run skipped
  exit 0
fi

bazel="${BAZEL_BIN:-$(command -v bazelisk || command -v bazel || true)}"
if [[ -z "$bazel" || ! -x "$bazel" ]]; then
  echo "status=bazel-unavailable" >>"$summary"
  exit 127
fi
mkdir -p "$repository_cache" "$bazelisk_home"
cleanup() {
  env BAZELISK_HOME="$bazelisk_home" timeout --signal=TERM --kill-after=3 10 "$bazel" --output_base="$output_base" shutdown \
    >"$out/shutdown.log" 2>&1 || true
}
trap cleanup EXIT

{
  echo "status=running"
  echo "target_invocation=single Bazel test command"
  echo "cache_test_results=disabled"
  echo "bazelisk_home=$bazelisk_home"
} >>"$summary"

/usr/bin/time -f 'go_wall_s=%e go_user_s=%U go_maxrss_kb=%M' -o "$out/go.time" \
  timeout --signal=TERM --kill-after=15 5m go test -count=1 ./internal/config \
  >"$out/go.log" 2>&1
go_status=$?
echo "go_status=$go_status" >>"$summary"
go_wall="unknown"; go_user="unknown"; go_rss="unknown"
if [[ -f "$out/go.time" ]]; then
  go_wall="$(sed -n 's/.*go_wall_s=\([^ ]*\).*/\1/p' "$out/go.time" | tail -1)"
  go_user="$(sed -n 's/.*go_user_s=\([^ ]*\).*/\1/p' "$out/go.time" | tail -1)"
  go_rss="$(sed -n 's/.*go_maxrss_kb=\([^ ]*\).*/\1/p' "$out/go.time" | tail -1)"
fi
[[ -n "$go_wall" ]] || go_wall=unknown; [[ -n "$go_user" ]] || go_user=unknown; [[ -n "$go_rss" ]] || go_rss=unknown
{
  echo "go_wall_s=$go_wall"
  echo "go_user_s=$go_user"
  echo "go_maxrss_kb=$go_rss"
} >>"$summary"
if [[ "$go_status" != 0 ]]; then
  echo "Go parity failed; Bazel results are non-comparable." >>"$summary"
  exit "$go_status"
fi

bep="$out/bazel.bep.json"
profile="$out/bazel.profile.gz"
IFS=',' read -r -a target_args <<<"$labels_csv"
common=(--noshow_progress --remote_cache= --repository_cache="$repository_cache" --cache_test_results=no)
/usr/bin/time -f 'bazel_wall_s=%e bazel_user_s=%U bazel_maxrss_kb=%M' -o "$out/bazel.time" \
  env BAZELISK_HOME="$bazelisk_home" timeout --signal=TERM --kill-after=15 5m \
  "$bazel" --output_base="$output_base" test "${common[@]}" \
  --build_event_json_file="$bep" --profile="$profile" "${target_args[@]}" \
  >"$out/bazel.log" 2>&1
bazel_status=$?
echo "bazel_status=$bazel_status" >>"$summary"
bazel_wall="unknown"; bazel_user="unknown"; bazel_rss="unknown"
if [[ -f "$out/bazel.time" ]]; then
  bazel_wall="$(sed -n 's/.*bazel_wall_s=\([^ ]*\).*/\1/p' "$out/bazel.time" | tail -1)"
  bazel_user="$(sed -n 's/.*bazel_user_s=\([^ ]*\).*/\1/p' "$out/bazel.time" | tail -1)"
  bazel_rss="$(sed -n 's/.*bazel_maxrss_kb=\([^ ]*\).*/\1/p' "$out/bazel.time" | tail -1)"
fi
[[ -n "$bazel_wall" ]] || bazel_wall=unknown; [[ -n "$bazel_user" ]] || bazel_user=unknown; [[ -n "$bazel_rss" ]] || bazel_rss=unknown
{
  echo "bazel_wall_s=$bazel_wall"
  echo "bazel_user_s=$bazel_user"
  echo "bazel_maxrss_kb=$bazel_rss"
} >>"$summary"

label_flags=()
for label in "${target_args[@]}"; do label_flags+=(--label "$label"); done
bep_payload="$out/bep-correlation.json"
if python3 "$resolver" bep "$bep" --format json "${label_flags[@]}" >"$bep_payload" 2>"$out/bep-resolver.stderr"; then
  bep_status=0
else
  bep_status=$?
fi
echo "bep_status=$bep_status" >>"$summary"

# Preserve exact requested-target correlation and keep action metrics visibly
# graph-wide: Bazel's action summary includes transitive dependencies.
python3 - "$bep" "$bep_payload" "$summary" <<'PY'
import json
import pathlib
import sys

bep_path, payload_path, summary_path = sys.argv[1:]
try:
    payload = json.loads(pathlib.Path(payload_path).read_text(encoding="utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError):
    payload = {}

def value_list(key):
    value = payload.get(key)
    return ",".join(value) if isinstance(value, list) and all(isinstance(x, str) for x in value) else ""

def value_count(key):
    value = payload.get(key)
    return str(value) if isinstance(value, int) and value >= 0 else "unknown"

metrics = {key: "unknown" for key in ("actions_created", "actions_executed", "action_cache_hits", "action_cache_misses", "analysis_phase_ms", "bep_cpu_ms")}

def integer(value):
    # Bazel versions differ: protobuf-to-JSON may emit int64 fields as JSON
    # numbers or decimal strings. Accept only non-negative canonical integers.
    if isinstance(value, bool):
        return None
    if isinstance(value, int) and value >= 0:
        return value
    if isinstance(value, str) and value.isdigit():
        return int(value)
    return None

def first_integer(mapping, *keys):
    if not isinstance(mapping, dict):
        return None
    for key in keys:
        parsed = integer(mapping.get(key))
        if parsed is not None:
            return parsed
    return None

try:
    with pathlib.Path(bep_path).open(encoding="utf-8") as source:
        for line in source:
            if not line.strip():
                continue
            event = json.loads(line)
            build = event.get("buildMetrics", {})
            if not isinstance(build, dict):
                continue
            action = build.get("actionSummary", {})
            if isinstance(action, dict):
                for source_key, output_key in (("actionsCreated", "actions_created"), ("actionsExecuted", "actions_executed")):
                    parsed = first_integer(action, source_key)
                    if parsed is not None: metrics[output_key] = str(parsed)
                cache = action.get("actionCacheStatistics", {})
                if isinstance(cache, dict):
                    for source_keys, output_key in ((("hitCount", "hits"), "action_cache_hits"), (("missCount", "misses"), "action_cache_misses")):
                        parsed = first_integer(cache, *source_keys)
                        if parsed is not None: metrics[output_key] = str(parsed)
            timing = build.get("timingMetrics", {})
            if isinstance(timing, dict):
                for source_key, output_key in (("analysisPhaseTimeInMs", "analysis_phase_ms"), ("cpuTimeInMs", "bep_cpu_ms")):
                    parsed = integer(timing.get(source_key))
                    if parsed is not None: metrics[output_key] = str(parsed)
except (OSError, UnicodeDecodeError, json.JSONDecodeError):
    pass

with pathlib.Path(summary_path).open("a", encoding="utf-8") as summary:
    summary.write(f"configured_labels={value_list('configured')}\n")
    summary.write(f"configured_count={value_count('configured_count')}\n")
    summary.write(f"completed_labels={value_list('completed')}\n")
    summary.write(f"completed_count={value_count('completed_count')}\n")
    summary.write(f"bep_error={str(payload.get('error') or '').replace(chr(10), ' ')[:240]}\n")
    for key, value in metrics.items(): summary.write(f"{key}={value}\n")
    summary.write("graph_metrics_scope=graph-wide (not target-specific)\n")
PY

if [[ "$bazel_status" != 0 || "$bep_status" != 0 ]]; then
  if [[ "$bazel_status" != 0 ]]; then exit "$bazel_status"; fi
  exit "$bep_status"
fi

echo "status=passed" >>"$summary"
