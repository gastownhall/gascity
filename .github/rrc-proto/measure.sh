#!/usr/bin/env bash
# THROWAWAY prototype (ga-vnycm2.13): cold-client analysis time per lane with
# and without Bazel 9's remote repo contents cache, against a throwaway
# NativeLink 1.7.1 on this runner (verify CAS + completeness-checking AC, as
# rbe-west's instance oss) reached through a netns with netem delay to
# emulate the WAN. Never merged.
set -euo pipefail
RC="${RC:?}"           # setup-bazel's generated rc
RTT_MS="${RTT_MS:-44}"
lanes=("$@")
W=/tmp/rrc; mkdir -p $W/runs
NL_IP=10.231.0.2

# --- throwaway NativeLink in its own netns --------------------------------
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
sudo ip netns add nl
sudo ip link add rrc0 type veth peer name rrc1
sudo ip link set rrc1 netns nl
sudo ip addr add 10.231.0.1/24 dev rrc0; sudo ip link set rrc0 up
sudo ip -n nl addr add $NL_IP/24 dev rrc1; sudo ip -n nl link set rrc1 up; sudo ip -n nl link set lo up
half=$((RTT_MS / 2))
setrtt() { local h=$(($1 / 2)); sudo tc qdisc replace dev rrc0 root netem delay ${h}ms limit 100000; sudo ip netns exec nl tc qdisc replace dev rrc1 root netem delay ${h}ms limit 100000; }
setrtt "$RTT_MS"
sudo ip netns exec nl sudo -u "$USER" $W/nativelink $W/nl.json >$W/nl.log 2>&1 &
for _ in $(seq 50); do (exec 3<>/dev/tcp/$NL_IP/50051) 2>/dev/null && break; sleep 0.2; done
ping -c 3 -q $NL_IP | tail -1

URL=grpc://$NL_IP:50051
run() { # run <name> <mode> <args...>
	local name=$1 mode=$2; shift 2
	local ob=$W/ob/$name out=$W/runs/$name
	mkdir -p "$ob" "$out"
	local startup=(--bazelrc="$RC" --output_base="$ob")
	local flags=(--nobuild --build_event_json_file="$out/bep.json" --profile="$out/profile.json.gz")
	case $mode in
	base) ;;
	write) startup+=(--experimental_remote_repo_contents_cache); flags+=(--remote_cache=$URL --remote_instance_name=oss --remote_upload_local_results) ;;
	read) startup+=(--experimental_remote_repo_contents_cache); flags+=(--remote_cache=$URL --remote_instance_name=oss --noremote_upload_local_results) ;;
	esac
	local t0 t1 rc=0
	t0=$(date +%s.%N)
	bazelisk "${startup[@]}" test "$@" "${flags[@]}" >"$out/log" 2>&1 || rc=$?
	t1=$(date +%s.%N)
	bazelisk "${startup[@]}" shutdown >/dev/null 2>&1 || true
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
for lane in "${lanes[@]}"; do
	# shellcheck disable=SC2086
	{
	run "$lane-base1" base ${cmd[$lane]}
	run "$lane-write" write ${cmd[$lane]}
	run "$lane-read1" read ${cmd[$lane]}
	run "$lane-base2" base ${cmd[$lane]}
	run "$lane-read2" read ${cmd[$lane]}
	setrtt 0
	run "$lane-read-rtt0" read ${cmd[$lane]}
	setrtt 100
	run "$lane-read-rtt100" read ${cmd[$lane]}
	setrtt "$RTT_MS"
	run "$lane-base3" base ${cmd[$lane]}
	run "$lane-read3" read ${cmd[$lane]}
	}
done
du -sh $W/cas $W/ac
echo "AC entries: $(find $W/ac -type f | wc -l)"
cat $W/results.txt
