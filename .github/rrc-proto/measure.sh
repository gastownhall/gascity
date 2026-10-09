#!/usr/bin/env bash
# THROWAWAY prototype (ga-vnycm2.13): cold-client analysis time per lane with
# and without Bazel 9's remote repo contents cache, against a throwaway
# NativeLink 1.7.1 on this runner (verify CAS + completeness-checking AC, as
# rbe-west's instance oss) reached through a userspace delay proxy that
# emulates the WAN round trip. Never merged.
set -euo pipefail
RC="${RC:?}"           # setup-bazel's generated rc
RTT_MS="${RTT_MS:-44}"
lanes=("$@")
W=/tmp/rrc; mkdir -p $W/runs
NL_IP=127.0.0.1

# --- throwaway NativeLink + delay proxy ----------------------------------
curl -fsSL -o $W/nl.tgz https://github.com/TraceMachina/nativelink/releases/download/v1.7.1/nativelink-1.7.1-x86_64-unknown-linux-musl.tar.gz
echo "a3d7abc2598e976d022fcdabe88a2f8fae46a3ae64f1868698002ca968dd88e9  $W/nl.tgz" | sha256sum -c -
tar -C $W -xzf $W/nl.tgz nativelink
cat >$W/nl.json <<JSON
{"stores":[
 {"name":"CAS","verify":{"backend":{"filesystem":{"content_path":"$W/cas","temp_path":"$W/tmp-cas","eviction_policy":{"max_bytes":30000000000}}},"verify_size":true,"verify_hash":true}},
 {"name":"AC","completeness_checking":{"backend":{"filesystem":{"content_path":"$W/ac","temp_path":"$W/tmp-ac","eviction_policy":{"max_bytes":2000000000}}},"cas_store":{"ref_store":{"name":"CAS"}}}}],
 "servers":[{"name":"proto","listener":{"http":{"socket_address":"$NL_IP:50051"}},
  "services":{"cas":[{"instance_name":"oss","cas_store":"CAS"}],"ac":[{"instance_name":"oss","ac_store":"AC"}],
   "bytestream":[{"instance_name":"oss","cas_store":"CAS"}],"capabilities":[{"instance_name":"oss"}]}}]}
JSON
echo "$RTT_MS" >$W/rtt_ms
setrtt() { echo "$1" >$W/rtt_ms; }
$W/nativelink $W/nl.json >$W/nl.log 2>&1 &
python3 "$(dirname "$0")/delayproxy.py" 50052 50051 $W/rtt_ms >$W/proxy.log 2>&1 &
for _ in $(seq 50); do (exec 3<>/dev/tcp/127.0.0.1/50051) 2>/dev/null && break; sleep 0.2; done
sleep 1

URL=grpc://127.0.0.1:50052
# Bazel release -> pinned sha256 (Bazelisk verifies the download).
declare -A bsha=([9.2.0]=7668a95db1250f12c40407251e4e203b4ec8bf39bc495d2f485b2d8c99048694 [9.3.0]=d302d22ed77ee5658b87857249baa579db84300ecebe0a1e9a357b5b20f80eb8)
run() { # run <name> <bazel version> <mode> <args...>
	local name=$1 bv=$2 mode=$3; shift 3
	local ob=$W/ob/$name out=$W/runs/$name
	mkdir -p "$ob" "$out"
	local startup=(--bazelrc="$RC" --output_base="$ob")
	local flags=(--nobuild --build_event_json_file="$out/bep.json" --profile="$out/profile.json.gz")
	case $mode in
	base) ;;
	write) startup+=(--experimental_remote_repo_contents_cache); flags+=(--remote_cache=grpc://127.0.0.1:50051 --remote_instance_name=oss --remote_upload_local_results) ;;
	read) startup+=(--experimental_remote_repo_contents_cache); flags+=(--remote_cache=$URL --remote_instance_name=oss --noremote_upload_local_results) ;;
	esac
	local t0 t1 rc=0
	t0=$(date +%s.%N)
	USE_BAZEL_VERSION=$bv BAZELISK_VERIFY_SHA256=${bsha[$bv]} bazelisk "${startup[@]}" test "$@" "${flags[@]}" >"$out/log" 2>&1 || rc=$?
	t1=$(date +%s.%N)
	USE_BAZEL_VERSION=$bv BAZELISK_VERIFY_SHA256=${bsha[$bv]} bazelisk "${startup[@]}" shutdown >/dev/null 2>&1 || true
	local an rules
	an=$(jq -r 'select(.buildMetrics) | .buildMetrics.timingMetrics.analysisPhaseTimeInMs' "$out/bep.json" | tail -1)
	rules=$(zcat "$out/profile.json.gz" | jq '[.traceEvents[]|select(.cat=="Starlark repository function call")]|length')
	printf '%-28s rc=%s wall=%6.1fs analysis_ms=%6s repo_rules_run=%s\n' "$name" "$rc" "$(echo "$t1-$t0" | bc)" "$an" "$rules" | tee -a $W/results.txt
	grep -E "^(WARNING|ERROR).*(repo|remote|cache)" "$out/log" | head -5 || true
	sudo rm -rf "$ob"
}
declare -A cmd=(
	[unit]="--config=ci --keep_going //..."
	[acceptance]="--config=ci --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests"
	[integration-packages]="--config=ci --config=integration --keep_going //test:integration_packages"
	[integration-smoke]="--config=ci --config=integration-smoke --keep_going //test/integration:integration_test"
)
# This runner's real round trip to rbe-west (TCP connect to :443, median of 15).
MEASURED=$(python3 - <<'PY'
import socket, statistics, time
ts = []
for _ in range(15):
    t = time.monotonic(); s = socket.create_connection(("rbe-west.ops.gascity.com", 443), timeout=5)
    ts.append((time.monotonic() - t) * 1000); s.close(); time.sleep(0.1)
print(round(statistics.median(ts)))
PY
)
echo "runner -> rbe-west TCP connect RTT: ${MEASURED} ms" | tee -a $W/results.txt
LPT=--loading_phase_threads=64
for lane in "${lanes[@]}"; do
	c=${cmd[$lane]}
	# shellcheck disable=SC2086
	{
	run "$lane-base-9.2-asis" 9.2.0 base $c
	run "$lane-base-9.3" 9.3.0 base $c
	run "$lane-base-9.3-lpt64" 9.3.0 base $LPT $c
	run "$lane-write-9.3" 9.3.0 write $LPT $c
	setrtt "$MEASURED"
	run "$lane-read-9.3-lpt64-rttM" 9.3.0 read $LPT $c
	run "$lane-read-9.3-lpt64-rttM-2" 9.3.0 read $LPT $c
	run "$lane-read-9.3-rttM-nolpt" 9.3.0 read $c
	setrtt 0
	run "$lane-read-9.3-lpt64-rtt0" 9.3.0 read $LPT $c
	setrtt 100
	run "$lane-read-9.3-lpt64-rtt100" 9.3.0 read $LPT $c
	setrtt "$MEASURED"
	run "$lane-base-9.3-lpt64-2" 9.3.0 base $LPT $c
	run "$lane-read-9.3-lpt64-rttM-3" 9.3.0 read $LPT $c
	}
done
du -sh $W/cas $W/ac
echo "AC entries: $(find $W/ac -type f | wc -l)"
cat $W/results.txt
