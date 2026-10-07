#!/usr/bin/env bash
# start-rig.sh -- render nativelink-s2.json5 with free loopback ports, launch
# the S2 rig's single NativeLink process in the background, wait for both
# listeners to come up, and print the chosen ports (and a stop command) so a
# Bazel client can be pointed at it.
#
# Usage: ./start-rig.sh            # starts, prints env exports, backgrounds
#        source <(./start-rig.sh)  # same, but also exports into your shell
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
state=/data/tmp/microvm-s2/state
bin=/data/tmp/microvm-s2/bin

mkdir -p "$state"/{cas/content,cas/tmp,ac-oss/content,ac-oss/tmp} \
         "$state"/{worker-oss-cas/content,worker-oss-cas/tmp,worker-fork-cas/content,worker-fork-cas/tmp} \
         "$state"/{work-oss,work-fork} /data/tmp/microvm-s2/run

for attempt in 1 2 3 4 5 6; do
	base=$("$here/pick-ports.sh")
	p1=$base; p2=$((base+1)); p3=$((base+2)); p4=$((base+3))
	cfg="/data/tmp/microvm-s2/run/nativelink-s2.rendered.json5"
	sed -e "s/__P1__/$p1/g; s/__P2__/$p2/g; s/__P3__/$p3/g; s/__P4__/$p4/g" \
		"$here/nativelink-s2.json5" > "$cfg"

	log="/data/tmp/microvm-s2/run/nativelink.log"
	: > "$log"
	"$bin/nativelink" "$cfg" >"$log" 2>&1 &
	pid=$!
	echo "$pid" > /data/tmp/microvm-s2/run/nativelink.pid

	ok=1
	for _ in $(seq 1 50); do
		if ! kill -0 "$pid" 2>/dev/null; then ok=0; break; fi
		if grep -q "already in use" "$log"; then ok=0; break; fi
		have=$(grep -c "Ready, listening" "$log" || true)
		[ "$have" -ge 4 ] && break
		sleep 0.1
	done
	if [ "$ok" = 0 ] || ! kill -0 "$pid" 2>/dev/null; then
		echo "# start-rig: attempt $attempt (base=$base) failed, retrying" >&2
		tail -5 "$log" >&2 || true
		wait "$pid" 2>/dev/null || true
		continue
	fi

	echo "export MICROVM_S2_OSS_CLIENT=127.0.0.1:$p1"
	echo "export MICROVM_S2_OSS_WORKER_API=127.0.0.1:$p2"
	echo "export MICROVM_S2_FORK_CLIENT=127.0.0.1:$p3"
	echo "export MICROVM_S2_FORK_WORKER_API=127.0.0.1:$p4"
	echo "export MICROVM_S2_PID=$pid"
	echo "# stop with: kill $pid"
	echo "# log: $log"
	exit 0
done

echo "start-rig: giving up after repeated port races" >&2
exit 1
