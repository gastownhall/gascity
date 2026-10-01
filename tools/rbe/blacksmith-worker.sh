#!/usr/bin/env bash
# Run a NativeLink remote-execution worker on a Blacksmith runner for the life of
# this workflow run's Bazel job, then drain and exit.
#
# Blacksmith donates this compute for OSS-repo workflows only. The worker
# registers with rbe-west's OSS scheduler (clients reach it with
# --remote_instance_name=oss); developer and agent builds use the default
# instance and never run here. The OSS and default instances share one CAS,
# but rbe-west splits the action cache: results written here go to the "oss"
# action cache, which fork PRs read anonymously and the default instance
# reads through (never writes). So REMOTE_AC must say "oss".
#
# Env (from the workflow):
#   RBE_WORKER_TLS_CERT / RBE_WORKER_TLS_KEY  base64 PEM, CN=rbe-oss-worker
#   RBE_WEST_HOST       e.g. rbe-west.ops.gascity.com (from a secret; not in git)
#   WORKER_NAME         unique per worker
#   WORKER_MODE         run  (default): serve until BAZEL_JOB_NAME in this run completes
#                                       (needs GH_TOKEN with actions:read)
#                       pool: serve the shared OSS queue; retire after POOL_IDLE_MINUTES
#                             with nothing in flight, or at POOL_MAX_MINUTES
#   CACHE_DIR           optional (Blacksmith sticky disk): keeps the worker's local
#                       CAS warm across runs
#   RBE_ACTION_ISOLATION  1 (default): every action runs as a per-action slot
#                       user in private namespaces (below). 0: the rollback
#                       switch, actions run as this runner user as before.
set -euo pipefail

: "${RBE_WORKER_TLS_CERT:?}" "${RBE_WORKER_TLS_KEY:?}" "${RBE_WEST_HOST:?}" "${WORKER_NAME:?}"
WORKER_MODE=${WORKER_MODE:-run}
case "$WORKER_MODE" in
run) : "${BAZEL_JOB_NAME:?}" ;;
pool) POOL_IDLE_MINUTES=${POOL_IDLE_MINUTES:-15} POOL_MAX_MINUTES=${POOL_MAX_MINUTES:-300} ;;
*) echo "WORKER_MODE must be run or pool" >&2; exit 2 ;;
esac
ACTION_ISOLATION=${RBE_ACTION_ISOLATION:-1}
case "$ACTION_ISOLATION" in
0 | 1) ;;
*) echo "RBE_ACTION_ISOLATION must be 0 or 1" >&2; exit 2 ;;
esac
NL_VERSION=1.7.1
NL_SHA256=a3d7abc2598e976d022fcdabe88a2f8fae46a3ae64f1868698002ca968dd88e9
GO_VERSION=$(awk '/^go /{print $2; exit}' go.mod)
DOLT_VERSION=2.1.8
DOLT_SHA256=f66318f08ed66e409fc39363ae0fff8ce6fbf6dba9f5bac632b91527b9632a74
ROOT="$RUNNER_TEMP/nl-worker"
NL_BIN_DIR="$RUNNER_TEMP/nl-bin"

# Host toolset: test actions exec tools via the client PATH
# (/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin), and cgo actions compile
# against host headers. lld: Bazel's auto-configured C toolchain on a client
# that has lld links with -fuse-ld=lld, so those cgo link actions fail here
# ("collect2: fatal error: cannot find 'ld'") without it.
# Keep in sync with infra nativelink-cas/scripts/elastic.sh.
sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 apt-get install -y -qq \
	make jq sqlite3 tmux lsof cmake git lld libicu-dev zlib1g-dev libsqlite3-dev \
	libbz2-dev liblzma-dev libffi-dev libexpat1-dev libxml2-dev libreadline-dev \
	libncurses-dev python3-dev >/dev/null
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
	sum=$(curl -fsSL "https://go.dev/dl/?mode=json&include=all" |
		jq -r --arg f "go${GO_VERSION}.linux-amd64.tar.gz" '.[].files[] | select(.filename==$f) | .sha256')
	curl -fsSL -o "$RUNNER_TEMP/go.tgz" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
	echo "${sum}  $RUNNER_TEMP/go.tgz" | sha256sum -c -
	sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf "$RUNNER_TEMP/go.tgz"
fi
if ! dolt version 2>/dev/null | grep -q "$DOLT_VERSION"; then
	curl -fsSL -o "$RUNNER_TEMP/dolt.tgz" "https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-amd64.tar.gz"
	echo "${DOLT_SHA256}  $RUNNER_TEMP/dolt.tgz" | sha256sum -c -
	tar -C "$RUNNER_TEMP" -xzf "$RUNNER_TEMP/dolt.tgz"
	sudo cp -f "$RUNNER_TEMP/dolt-linux-amd64/bin/dolt" /usr/local/bin/dolt
fi
curl -fsSL -o "$RUNNER_TEMP/nl.tgz" "https://github.com/TraceMachina/nativelink/releases/download/v${NL_VERSION}/nativelink-${NL_VERSION}-x86_64-unknown-linux-musl.tar.gz"
echo "${NL_SHA256}  $RUNNER_TEMP/nl.tgz" | sha256sum -c -
mkdir -p "$NL_BIN_DIR" && tar -C "$NL_BIN_DIR" -xzf "$RUNNER_TEMP/nl.tgz" nativelink

# Local CAS (and its temp dir, which must share the filesystem for renames)
# lives on the sticky disk when one is mounted. content.exec sits next to
# content and is wiped by NativeLink at startup.
STORE=${CACHE_DIR:-$ROOT}
mkdir -p "$ROOT"/{work,pki} "$STORE"/{content,tmp}
umask 077
printf '%s' "$RBE_WORKER_TLS_CERT" | base64 -d >"$ROOT/pki/worker.pem"
printf '%s' "$RBE_WORKER_TLS_KEY" | base64 -d >"$ROOT/pki/worker.key"
umask 022

# One action per two vCPUs. NativeLink ignores the client cert when
# use_native_roots is set, so trust the system bundle via ca_file instead.
slots=$(($(nproc) / 2)); [ "$slots" -ge 1 ] || slots=1

# Action isolation (S11.3; infra nativelink-cas/west README "Action isolation";
# rbe-action-* here are copies of infra's, keep them in sync). Actions run as
# per-action slot users rbe-aNN (no sudo, no docker, no other group) in private
# pid/mount/ipc namespaces with this runner's home hidden: the checkout,
# $RUNNER_TEMP and pki/worker.key are invisible and unreadable, and so is this
# step's environment (another uid, another pid namespace). An action can no
# longer take the worker cert, so it cannot write the action cache or register
# workers. The runner's own sudo (NOPASSWD) runs the root launcher; slot egress
# is filtered by nftables (NETNS=0).
LIB=/usr/local/libexec/rbe-action
isolation='{}'
if [ "$ACTION_ISOLATION" = 1 ]; then
	# The nft rules below cover uids 59000-59063.
	[ "$slots" -le 64 ] || slots=64
	SLOT_UID0=59000
	# Canonical paths: the launcher compares them with the action's pwd -P.
	MASK_ROOT=$(cd "$HOME" && pwd -P)
	WORK_ROOT=$(cd "$ROOT/work" && pwd -P)
	# The CAS must stay under the mask: a CACHE_DIR (sticky disk) outside $HOME
	# would be visible to actions, so isolation refuses it.
	for d in "$WORK_ROOT" "$(cd "$ROOT" && pwd -P)" "$(cd "$STORE" && pwd -P)"; do
		case "$d" in "$MASK_ROOT"/*) ;; *) echo "isolation: $d must be under $MASK_ROOT (MASK_ROOT, hidden from actions)" >&2; exit 1 ;; esac
	done
	sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 apt-get install -y -qq \
		gcc libc6-dev nftables file util-linux procps >/dev/null
	# The nft rules cover uids 59000-59063 whatever owns them: anything already
	# there (an image user or group) would be filtered like a slot, and a slot
	# sharing its uid or gid would share its files.
	taken=$(awk -F: '$3 >= 59000 && $3 <= 59063 { print FILENAME ": " $1 " (" $3 ")" }' /etc/passwd /etc/group)
	[ -z "$taken" ] || { echo "isolation: uids/gids 59000-59063 must be free for the slot users, taken: $taken" >&2; exit 1; }
	for i in $(seq 0 $((slots - 1))); do
		u=$(printf 'rbe-a%02d' "$i") id=$((SLOT_UID0 + i))
		sudo groupadd --system --gid "$id" "$u"
		sudo useradd --system --uid "$id" --gid "$id" --no-create-home --home-dir /var/lib/rbe-action/home --shell /bin/bash "$u"
	done
	sudo install -d -m 0755 /var/lib/rbe-action /var/lib/rbe-action/home "$LIB" /etc/rbe-west
	gcc -static -O2 -Wall -Wextra -o "$RUNNER_TEMP/rbe-entry" tools/rbe/rbe-action-entry.c
	gcc -static -O2 -Wall -Wextra -DRBE_ACTION_EXEC -o "$RUNNER_TEMP/rbe-exec" tools/rbe/rbe-action-entry.c
	sudo install -m 0755 "$RUNNER_TEMP/rbe-entry" "$LIB/entry"
	sudo install -m 0755 "$RUNNER_TEMP/rbe-exec" "$LIB/exec"
	sudo install -m 0755 tools/rbe/rbe-action-launch "$LIB/launch"
	sudo install -m 0755 tools/rbe/rbe-action-sweep "$LIB/sweep"
	sudo install -m 0755 tools/rbe/rbe-action-selftest "$LIB/selftest"
	sudo tee /etc/rbe-west/rbe-action.env >/dev/null <<-EOF
		WORK_ROOT=$WORK_ROOT
		MASK_ROOT=$MASK_ROOT
		WORKER_UID=$(id -u)
		WORKER_GID=$(id -g)
		SLOT_UID0=$SLOT_UID0
		SLOT_COUNT=$slots
		BACKSTOP_S=1260
		MAX_TIMEOUT_S=1200
		HOME_DIR=/var/lib/rbe-action/home
		NETNS=0
		SHM_SIZE=8g
		TMPFS_DIRS="/run/lock /var/crash"
		EGRESS_CHAIN="inet rbe_action output"
		PROBE_DENY="169.254.169.254:80"
		WORKER_JSON=$ROOT/worker.json
	EOF
	printf 'Defaults!%s/launch !pam_session, !log_allowed, !use_pty, !lecture\n' "$LIB" |
		sudo tee /etc/sudoers.d/rbe-action >/dev/null
	sudo chmod 0440 /etc/sudoers.d/rbe-action && sudo visudo -cq
	# Slot DNS: a loopback nameserver (systemd-resolved's 127.0.0.53, which
	# queries upstream as itself) needs nothing. A non-loopback IPv4 one, e.g. a
	# private VPC resolver, is allowed to slots on port 53 alone; with neither
	# (IPv6 only), slots could not resolve at all: refuse. No nameserver line:
	# glibc uses 127.0.0.1.
	nameservers=$(awk '$1 == "nameserver" { print $2 }' /etc/resolv.conf)
	dns_v4=$(grep -E '^[0-9]+(\.[0-9]+){3}$' <<<"$nameservers" | grep -v '^127\.' | paste -sd, - | sed 's/,/, /g' || true)
	dns_allow=
	if [ -n "$dns_v4" ]; then
		dns_allow="meta skuid 59000-59063 ip daddr { $dns_v4 } meta l4proto { tcp, udp } th dport 53 accept"
	elif [ -n "$nameservers" ] && ! grep -qE '^(127\.|::1$)' <<<"$nameservers"; then
		echo "isolation: no nameserver in /etc/resolv.conf that slot users can reach (loopback or IPv4): $nameservers" >&2
		exit 1
	fi
	# Slot users never reach link-local (cloud metadata), private/CGNAT,
	# reserved or multicast ranges, or IPv6 beyond loopback (ff00::/8
	# included), as on the MAIN worker; other destinations are counted for the
	# NETNS=1 decision. Loopback first: slots' own servers and the local resolver.
	sudo nft -f - <<-EOF
		table inet rbe_action
		delete table inet rbe_action
		table inet rbe_action {
			set slot_dst { type ipv4_addr . inet_proto . inet_service; size 65536; flags dynamic,timeout; timeout 1d; counter; }
			chain output {
				type filter hook output priority 0; policy accept;
				oif lo accept
				$dns_allow
				meta skuid 59000-59063 meta nfproto ipv6 meta l4proto tcp reject with tcp reset
				meta skuid 59000-59063 meta nfproto ipv6 reject with icmpx admin-prohibited
				meta skuid 59000-59063 ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 } meta l4proto tcp reject with tcp reset
				meta skuid 59000-59063 ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/3 } reject with icmpx admin-prohibited
				meta skuid 59000-59063 meta l4proto { tcp, udp } ct state new update @slot_dst { ip daddr . meta l4proto . th dport }
			}
		}
	EOF
	isolation='{ "entrypoint": "/usr/local/libexec/rbe-action/entry", "timeout_handled_externally": true, "max_action_timeout": 1260,
		"additional_environment": { "RBE_X_TIMEOUT_MS": "timeout_millis", "RBE_X_SIDE_CHANNEL": "side_channel_file" } }'
fi
jq -n --arg host "grpcs://${RBE_WEST_HOST}:443" --arg root "$ROOT" --arg store "$STORE" --arg name "$WORKER_NAME" --argjson slots "$slots" --argjson isolation "$isolation" '
  { cert_file: ($root + "/pki/worker.pem"), key_file: ($root + "/pki/worker.key"),
    ca_file: "/etc/ssl/certs/ca-certificates.crt" } as $tls |
  {
    stores: [
      { name: "REMOTE_CAS", grpc: { instance_name: "", endpoints: [{ address: $host, tls_config: $tls }], store_type: "cas" } },
      { name: "REMOTE_AC", grpc: { instance_name: "oss", endpoints: [{ address: $host, tls_config: $tls }], store_type: "ac" } },
      { name: "WFS", fast_slow: {
          fast: { filesystem: { content_path: ($store + "/content"), temp_path: ($store + "/tmp"),
                                eviction_policy: { max_bytes: 150000000000 } } },
          slow: { ref_store: { name: "REMOTE_CAS" } } } }
    ],
    workers: [ { local: ({
      name: $name,
      worker_api_endpoint: { uri: $host, tls_config: $tls },
      cas_fast_slow_store: "WFS",
      upload_action_result: { ac_store: "REMOTE_AC" },
      work_directory: ($root + "/work"),
      max_inflight_tasks: $slots,
      platform_properties: {
        OSFamily: { values: ["linux"] },
        "container-image": { values: [""] },
        ISA: { values: ["x86_64"] }
      } } + $isolation) } ],
    servers: []
  }' >"$ROOT/worker.json"

if [ "$ACTION_ISOLATION" = 1 ]; then
	# This VM is ephemeral and runs only this worker: no directory on / but
	# /tmp and /var/tmp (private per action) and TMPFS_DIRS may be writable by
	# every user, or one action could leave files for a later one.
	shared_dirs=$(sudo find / -xdev -type d -perm -0002 2>/dev/null | grep -vxE '/tmp|/var/tmp|/run/lock|/var/crash' || true)
	if [ -n "$shared_dirs" ]; then
		echo "isolation: removing world write from: $(tr '\n' ' ' <<<"$shared_dirs")"
		xargs -r -d '\n' sudo chmod o-w <<<"$shared_dirs"
	fi
	# Full selftest (a few seconds): it also checks worker.json routes actions
	# through the entrypoint, the timeout path, and that this image has no
	# shared world-writable directory outside TMPFS_DIRS.
	sudo "$LIB/selftest"
	LC_ALL=C sudo -l -U rbe-a00 2>&1 | grep -q 'not allowed to run sudo' || { echo "isolation: slot users must have no sudo" >&2; exit 1; }
	# /run is not masked: a socket a slot user could open there (a 0666
	# docker.sock) is a way out of the sandbox.
	open_socks=$(sudo find /run /var/run -xdev -maxdepth 3 -type s -perm -o+w 2>/dev/null | grep -vxE '/run/systemd/(notify|journal/.*|private|io\.system\.ManagedOOM)|/run/dbus/system_bus_socket' || true)
	[ -z "$open_socks" ] || { echo "isolation: world-writable sockets reachable by actions: $open_socks" >&2; exit 1; }
	# What S11.3 is about, on this VM's layout: a probe action through the real
	# entrypoint must run as a slot user and fail to read the worker key, find
	# this step's environment in any process, or sudo.
	probe=$WORK_ROOT/isolation-probe-$$
	mkdir -p "$probe/work"
	# shellcheck disable=SC2016 # the action's own script
	out=$(cd "$probe/work" && env -i PATH=/usr/bin:/bin "$LIB/entry" /bin/bash -c '
		echo "uid $(id -u)"
		cat "$1" >/dev/null 2>&1 && echo "LEAK worker.key"
		grep -qs RBE_WORKER_TLS_KEY /proc/[0-9]*/environ && echo "LEAK step environment"
		sudo -n true >/dev/null 2>&1 && echo "LEAK sudo"
		true' probe "$ROOT/pki/worker.key" 2>&1) || true
	rm -rf "$probe"
	echo "isolation probe: $(tr '\n' ' ' <<<"$out")"
	if ! grep -qE "^uid 590[0-9]{2}$" <<<"$out" || grep -q LEAK <<<"$out"; then
		echo "isolation probe failed; RBE_ACTION_ISOLATION=0 (repository variable) is the rollback" >&2
		exit 1
	fi
	# The cert is in files from here on. NativeLink's persistent-worker spawn
	# hands its own environment to the action: give it none of this step's.
	env -i PATH="$PATH" HOME="$HOME" "$NL_BIN_DIR/nativelink" "$ROOT/worker.json" >"$ROOT/worker.log" 2>&1 &
else
	"$NL_BIN_DIR/nativelink" "$ROOT/worker.json" >"$ROOT/worker.log" 2>&1 &
fi
nl=$!
echo "worker $WORKER_NAME started (pid $nl, $slots slots, action isolation $ACTION_ISOLATION)"

# A launcher NativeLink killed (cancel, backstop timeout) leaves a slot-owned
# action directory NativeLink cannot remove, which pool mode would count as in
# flight forever: the sweep removes it (the MAIN worker runs it on a timer).
sweep() { [ "$ACTION_ISOLATION" = 0 ] || sudo "$LIB/sweep" || true; }

# SIGTERM makes nativelink finish in-flight actions and send GoingAway.
if [ "$WORKER_MODE" = run ]; then
	# Until this run's Bazel job completes. 30s polls keep a run's two
	# workers well under GITHUB_TOKEN's 1,000 requests/hour/repository.
	jobs_url="repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT}/jobs?per_page=100"
	while kill -0 "$nl" 2>/dev/null; do
		sweep
		status=$(gh api "$jobs_url" --jq ".jobs[] | select(.name == \"$BAZEL_JOB_NAME\") | .status" 2>/dev/null || true)
		[ "$status" = "completed" ] && break
		sleep 30
	done
else
	# Pool: one running action = one directory under work/. No GitHub API use.
	started=$SECONDS idle=0
	while kill -0 "$nl" 2>/dev/null; do
		sweep
		inflight=$(find "$ROOT/work" -mindepth 1 -maxdepth 1 -type d | wc -l)
		if [ "$inflight" -eq 0 ]; then idle=$((idle + 30)); else idle=0; fi
		[ "$idle" -ge $((POOL_IDLE_MINUTES * 60)) ] && { echo "idle ${POOL_IDLE_MINUTES}m, retiring"; break; }
		[ $((SECONDS - started)) -ge $((POOL_MAX_MINUTES * 60)) ] && { echo "max age ${POOL_MAX_MINUTES}m, retiring"; break; }
		sleep 30
	done
fi
if kill -0 "$nl" 2>/dev/null; then
	kill -TERM "$nl"
	timeout 300 tail --pid="$nl" -f /dev/null || kill -KILL "$nl"
fi
grep -E 'registered|GoingAway|ERROR' "$ROOT/worker.log" | tail -20 || true
if [ "$ACTION_ISOLATION" = 1 ]; then
	echo "slot egress (destination . protocol . port, packets): for the NETNS=1 decision"
	sudo nft list set inet rbe_action slot_dst | sed -n '/elements/,/}/p' || true
fi
