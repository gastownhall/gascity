#!/usr/bin/env bash
# End-to-end demo: Gas City extmsg fabric driven by openclaw's shipped
# iMessage connector, on Linux, against a fake `imsg` CLI.
#
#   [fake iMessage] <-> [openclaw connector code] <-> [bridge.mjs] <-> [gc extmsg fabric] <-> [agent session]
#
# Everything runs isolated: a dedicated GC_HOME, supervisor on
# $SUPERVISOR_PORT, and file-backed fake agents. Artifacts live under
# $DEMO_DIR (default /tmp/gc-openclaw-bridge-demo) and are preserved after
# the run for inspection.
#
# Usage: ./demo.sh
# Env:   GC_BIN (prebuilt gc binary; default: builds into .cache/gc)
#        DEMO_DIR (default /tmp/gc-openclaw-bridge-demo)
#        SUPERVISOR_PORT (default 9870), BRIDGE_PORT (default 8930)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$ROOT/../.." && pwd)"
# Outside the repo tree: a city inside it would inherit the repo's beads (bd)
# context and trip bd's init-safety guard.
DEMO="${DEMO_DIR:-/tmp/gc-openclaw-bridge-demo}"
SUPERVISOR_PORT="${SUPERVISOR_PORT:-9870}"
BRIDGE_PORT="${BRIDGE_PORT:-8930}"
CITY_NAME="openclaw-demo"
BASE="http://127.0.0.1:$SUPERVISOR_PORT/v0/city/$CITY_NAME"
SENDER="+15551234567"

export GC_HOME="$DEMO/gc-home"
export DEMO_AGENT_STATE="$DEMO/agents"
export FAKE_IMSG_DIR="$DEMO/imsg"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mdemo failed: %s\033[0m\n' "$*" >&2; exit 1; }

wait_for() { # wait_for <seconds> <description> <command...>
  local secs="$1" desc="$2"
  shift 2
  for _ in $(seq 1 "$secs"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  die "timed out waiting for $desc"
}

BRIDGE_PID=""
SUPERVISOR_PID=""
GC=""
kill_and_wait() { # kill_and_wait <pid> — SIGTERM, wait briefly, escalate to SIGKILL
  local pid="$1"
  [ -n "$pid" ] || return 0
  kill "$pid" 2>/dev/null || return 0
  for _ in 1 2 3 4 5 6; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.5
  done
  kill -9 "$pid" 2>/dev/null || true
}
cleanup() {
  step "Cleanup"
  kill_and_wait "$BRIDGE_PID" # its imsg rpc child exits with it (stdin close)
  [ -n "$GC" ] && "$GC" supervisor uninstall >/dev/null 2>&1 || true
  kill_and_wait "$SUPERVISOR_PID"
  # The controller can leave a managed dolt sql-server (+ scope watchdog)
  # running under the sandbox; their cmdlines all reference $DEMO, so this
  # sweep is scoped to processes this run created.
  pkill -f -- "$DEMO" 2>/dev/null || true
  echo "artifacts preserved under $DEMO (logs, transcripts, inbox/outbox)"
}
trap cleanup EXIT

step "Preflight"
command -v node >/dev/null || die "node is required"
command -v curl >/dev/null || die "curl is required"
command -v python3 >/dev/null || die "python3 is required"
if curl -fs --max-time 2 "http://127.0.0.1:$SUPERVISOR_PORT/v0/cities" >/dev/null 2>&1; then
  die "port $SUPERVISOR_PORT is already in use (leaked supervisor from a previous run, or another service); kill it or set SUPERVISOR_PORT"
fi
# Refuse to wipe a directory this script didn't create.
if [ -e "$DEMO" ] && [ ! -e "$DEMO/.gc-openclaw-bridge-demo" ]; then
  die "$DEMO exists but was not created by this demo; remove it or set DEMO_DIR elsewhere"
fi
if [ ! -d "$ROOT/node_modules/openclaw/dist" ]; then
  bold "installing openclaw from npm (pinned via package-lock.json)..."
  (cd "$ROOT" && npm install --no-audit --no-fund)
fi
GC="${GC_BIN:-$ROOT/.cache/gc}"
if [ ! -x "$GC" ]; then
  command -v go >/dev/null || die "go is required to build gc (or set GC_BIN)"
  bold "building gc..."
  mkdir -p "$ROOT/.cache"
  (cd "$REPO" && go build -o "$GC" ./cmd/gc)
fi
echo "gc: $GC"
echo "openclaw: $(node -p "require('$ROOT/node_modules/openclaw/package.json').version")"

step "Start isolated gc supervisor (GC_HOME=$GC_HOME, port $SUPERVISOR_PORT)"
rm -rf "$DEMO"
mkdir -p "$GC_HOME" "$DEMO_AGENT_STATE" "$FAKE_IMSG_DIR"
touch "$DEMO/.gc-openclaw-bridge-demo" # sentinel: marks $DEMO as safe to wipe next run
printf '[supervisor]\nport = %s\n' "$SUPERVISOR_PORT" > "$GC_HOME/supervisor.toml"
"$GC" supervisor run > "$DEMO/supervisor.stdout.log" 2>&1 &
SUPERVISOR_PID=$!
wait_for 30 "supervisor API" curl -fs "http://127.0.0.1:$SUPERVISOR_PORT/v0/cities"
kill -0 "$SUPERVISOR_PID" 2>/dev/null || die "our supervisor exited; something else answered on $SUPERVISOR_PORT (see $DEMO/supervisor.stdout.log)"

step "Create demo city"
"$GC" init --template minimal --default-provider claude --skip-provider-readiness \
  --name "$CITY_NAME" "$DEMO/city" > "$DEMO/init.log" 2>&1 || true
grep -q '"name":"'"$CITY_NAME"'"' <(curl -fs "http://127.0.0.1:$SUPERVISOR_PORT/v0/cities") \
  || die "city did not register (see $DEMO/init.log)"

# Demo-friendly config: file-backed exec session provider, a neutral agent
# provider (no readiness prompts / process checks), no builtin packs.
python3 - "$DEMO/city/city.toml" "$ROOT/demo/agent-exec.sh" <<'EOF'
import sys
path, exec_script = sys.argv[1], sys.argv[2]
s = open(path).read()
s = s.replace('includes = [".gc/system/packs/core", ".gc/system/packs/bd"]\n', '')
s += f'''
[session]
provider = "exec:{exec_script}"

[providers.demo-null]
command = "sleep"
args = ["infinity"]
prompt_mode = "none"
ready_delay_ms = 0
'''
open(path, 'w').write(s)
EOF
mkdir -p "$DEMO/city/agents/assistant"
printf 'provider = "demo-null"\nidle_timeout = "2h"\n' > "$DEMO/city/agents/assistant/agent.toml"
printf 'You are the demo assistant. Reply helpfully to messages.\n' > "$DEMO/city/agents/assistant/prompt.md"
"$GC" supervisor reload >/dev/null 2>&1
sleep 2

step "Start the bridge (openclaw connector -> gc adapter)"
GC_CITY="$CITY_NAME" GC_BASE_URL="http://127.0.0.1:$SUPERVISOR_PORT" GC_SCOPE_ID="$CITY_NAME" \
  BRIDGE_PORT="$BRIDGE_PORT" node "$ROOT/bridge.mjs" > "$DEMO/bridge.log" 2>&1 &
BRIDGE_PID=$!
wait_for 30 "adapter registration" grep -q "registered adapter" "$DEMO/bridge.log"
grep "imsg probe ok\|registered adapter" "$DEMO/bridge.log"

step "Create an agent session and bind the iMessage conversation to it"
SESSION_JSON="$("$GC" --city "$DEMO/city" session new assistant --no-attach --json 2>/dev/null | tail -1)"
SID="$(printf '%s' "$SESSION_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
SNAME="$(printf '%s' "$SESSION_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_name"])')"
echo "session: $SID ($SNAME)"
NUDGE_LOG="$DEMO_AGENT_STATE/$(printf '%s' "$SNAME" | tr -c 'A-Za-z0-9_.-' '_').nudges.log"
wait_for 60 "session start" test -e "$DEMO_AGENT_STATE/$(printf '%s' "$SNAME" | tr -c 'A-Za-z0-9_.-' '_').running"

CONV="{\"scope_id\":\"$CITY_NAME\",\"provider\":\"imessage\",\"account_id\":\"default\",\"conversation_id\":\"$SENDER\",\"kind\":\"dm\"}"
curl -fsX POST "$BASE/extmsg/bind" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"conversation\":$CONV}" >/dev/null
echo "bound $SENDER (dm) -> $SID"

step "INBOUND: simulate an incoming iMessage"
INBOUND_TEXT="Hey, what is the status of the build?"
python3 -c 'import json,sys; print(json.dumps({"text": sys.argv[1], "sender": sys.argv[2]}))' \
  "$INBOUND_TEXT" "$SENDER" >> "$FAKE_IMSG_DIR/inbox.jsonl"
echo "appended to fake-imsg inbox: \"$INBOUND_TEXT\""
wait_for 30 "bridge forward" grep -q "inbound $SENDER" "$DEMO/bridge.log"
grep "inbound $SENDER" "$DEMO/bridge.log" | tail -1
wait_for 30 "agent nudge" grep -q "status of the build" "$NUDGE_LOG"
bold "agent session received:"
sed -n '/<system-reminder>/,/<\/system-reminder>/p' "$NUDGE_LOG" | head -20

step "OUTBOUND: the session replies through gc"
REPLY="**assistant:** Build is green — all tests passing."
OUTBOUND_BODY="$(python3 -c 'import json,sys; print(json.dumps({"session_id": sys.argv[1], "conversation": json.loads(sys.argv[2]), "text": sys.argv[3]}))' "$SID" "$CONV" "$REPLY")"
RECEIPT="$(curl -fsX POST "$BASE/extmsg/outbound" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' -d "$OUTBOUND_BODY")"
printf '%s' "$RECEIPT" | python3 -c 'import json,sys; r=json.load(sys.stdin)["Receipt"]; print("receipt: delivered=%s message_id=%s" % (r["Delivered"], r["MessageID"]))'
wait_for 10 "fake-imsg delivery" grep -q "Build is green" "$FAKE_IMSG_DIR/outbox.jsonl"
bold "delivered to iMessage (note: markdown converted to native formatting runs by openclaw's send pipeline):"
tail -1 "$FAKE_IMSG_DIR/outbox.jsonl"

step "Conversation transcript (gc extmsg fabric)"
curl -fs "$BASE/extmsg/transcript?scope_id=$CITY_NAME&provider=imessage&account_id=default&conversation_id=%2B${SENDER#+}&kind=dm" \
  | python3 -c 'import json,sys; [print("  %2d %-9s %s" % (r["Sequence"], r["Kind"], r["Text"])) for r in json.load(sys.stdin)["items"]]'

bold ""
bold "DEMO COMPLETE: openclaw's shipped iMessage connector (npm, unmodified) drove"
bold "Gas City's extmsg fabric end to end — inbound -> session nudge, outbound -> receipt."
