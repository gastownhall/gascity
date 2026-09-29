#!/usr/bin/env bash
# Run a NativeLink remote-execution worker on a Blacksmith runner for the life of
# this workflow run's Bazel job, then drain and exit.
#
# Blacksmith donates this compute for OSS-repo workflows only. The worker
# registers with rbe-west's OSS scheduler (clients reach it with
# --remote_instance_name=oss); developer and agent builds use the default
# instance and never run here. The OSS and default instances share one cache.
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
set -euo pipefail

: "${RBE_WORKER_TLS_CERT:?}" "${RBE_WORKER_TLS_KEY:?}" "${RBE_WEST_HOST:?}" "${WORKER_NAME:?}"
WORKER_MODE=${WORKER_MODE:-run}
case "$WORKER_MODE" in
run) : "${BAZEL_JOB_NAME:?}" ;;
pool) POOL_IDLE_MINUTES=${POOL_IDLE_MINUTES:-15} POOL_MAX_MINUTES=${POOL_MAX_MINUTES:-300} ;;
*) echo "WORKER_MODE must be run or pool" >&2; exit 2 ;;
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
jq -n --arg host "grpcs://${RBE_WEST_HOST}:443" --arg root "$ROOT" --arg store "$STORE" --arg name "$WORKER_NAME" --argjson slots "$slots" '
  { cert_file: ($root + "/pki/worker.pem"), key_file: ($root + "/pki/worker.key"),
    ca_file: "/etc/ssl/certs/ca-certificates.crt" } as $tls |
  {
    stores: [
      { name: "REMOTE_CAS", grpc: { instance_name: "", endpoints: [{ address: $host, tls_config: $tls }], store_type: "cas" } },
      { name: "REMOTE_AC", grpc: { instance_name: "", endpoints: [{ address: $host, tls_config: $tls }], store_type: "ac" } },
      { name: "WFS", fast_slow: {
          fast: { filesystem: { content_path: ($store + "/content"), temp_path: ($store + "/tmp"),
                                eviction_policy: { max_bytes: 150000000000 } } },
          slow: { ref_store: { name: "REMOTE_CAS" } } } }
    ],
    workers: [ { local: {
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
      } } } ],
    servers: []
  }' >"$ROOT/worker.json"

"$NL_BIN_DIR/nativelink" "$ROOT/worker.json" >"$ROOT/worker.log" 2>&1 &
nl=$!
echo "worker $WORKER_NAME started (pid $nl, $slots slots)"

# SIGTERM makes nativelink finish in-flight actions and send GoingAway.
if [ "$WORKER_MODE" = run ]; then
	# Until this run's Bazel job completes. 30s polls keep a run's two
	# workers well under GITHUB_TOKEN's 1,000 requests/hour/repository.
	jobs_url="repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT}/jobs?per_page=100"
	while kill -0 "$nl" 2>/dev/null; do
		status=$(gh api "$jobs_url" --jq ".jobs[] | select(.name == \"$BAZEL_JOB_NAME\") | .status" 2>/dev/null || true)
		[ "$status" = "completed" ] && break
		sleep 30
	done
else
	# Pool: one running action = one directory under work/. No GitHub API use.
	started=$SECONDS idle=0
	while kill -0 "$nl" 2>/dev/null; do
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
