#!/usr/bin/env bash
# End-to-end demo: Gas City extmsg fabric driven by openclaw's shipped
# telegram connector (outbound) + the bridge's own getUpdates poll (inbound),
# on Linux, against a fake local Telegram Bot API server.
#
#   [fake Bot API] <-> [openclaw send pipeline / bridge getUpdates] <-> [telegram-bridge.mjs] <-> [gc extmsg fabric] <-> [agent session]
#
# Everything runs isolated: a dedicated GC_HOME, supervisor on
# $SUPERVISOR_PORT, and file-backed fake agents. Artifacts live under
# $DEMO_DIR (default /tmp/gc-openclaw-telegram-demo) and are preserved after
# the run for inspection.
#
# To run against REAL Telegram instead: skip this script, get a token from
# @BotFather, and start telegram-bridge.mjs with TELEGRAM_BOT_TOKEN=<token>
# (TELEGRAM_API_ROOT defaults to https://api.telegram.org).
#
# Usage: ./demo-telegram.sh
# Env:   GC_BIN (prebuilt gc binary; default: builds into .cache/gc)
#        DEMO_DIR (default /tmp/gc-openclaw-telegram-demo)
#        SUPERVISOR_PORT (default 9872), BRIDGE_PORT (default 8931),
#        FAKE_TG_PORT (default 8932)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$ROOT/../.." && pwd)"
# Outside the repo tree: a city inside it would inherit the repo's beads (bd)
# context and trip bd's init-safety guard.
DEMO="${DEMO_DIR:-/tmp/gc-openclaw-telegram-demo}"
SUPERVISOR_PORT="${SUPERVISOR_PORT:-9872}"
BRIDGE_PORT="${BRIDGE_PORT:-8931}"
FAKE_TG_PORT="${FAKE_TG_PORT:-8932}"
CITY_NAME="openclaw-tg-demo"
BASE="http://127.0.0.1:$SUPERVISOR_PORT/v0/city/$CITY_NAME"
TOKEN="123456:TESTTOKEN"
CHAT_ID="7113355" # the demo user's DM chat id

export GC_HOME="$DEMO/gc-home"
export DEMO_AGENT_STATE="$DEMO/agents"
export FAKE_TG_DIR="$DEMO/telegram"

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
FAKE_TG_PID=""
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
  kill_and_wait "$BRIDGE_PID"
  kill_and_wait "$FAKE_TG_PID"
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
if [ -e "$DEMO" ] && [ ! -e "$DEMO/.gc-openclaw-telegram-demo" ]; then
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
mkdir -p "$GC_HOME" "$DEMO_AGENT_STATE" "$FAKE_TG_DIR"
touch "$DEMO/.gc-openclaw-telegram-demo" # sentinel: marks $DEMO as safe to wipe next run
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

step "Start the fake Telegram Bot API server"
FAKE_TG_PORT="$FAKE_TG_PORT" FAKE_TG_TOKEN="$TOKEN" FAKE_TG_SENDER_ID="$CHAT_ID" \
  node "$ROOT/fake-telegram/bot-api" > "$DEMO/fake-tg.log" 2>&1 &
FAKE_TG_PID=$!
wait_for 15 "fake Bot API" curl -fs "http://127.0.0.1:$FAKE_TG_PORT/bot$TOKEN/getMe"

step "Start the bridge (openclaw telegram connector -> gc adapter)"
# ALLOW_FROM gates inbound at the edge: only the demo user may reach gc.
GC_CITY="$CITY_NAME" GC_BASE_URL="http://127.0.0.1:$SUPERVISOR_PORT" GC_SCOPE_ID="$CITY_NAME" \
  BRIDGE_PORT="$BRIDGE_PORT" TELEGRAM_BOT_TOKEN="$TOKEN" \
  TELEGRAM_API_ROOT="http://127.0.0.1:$FAKE_TG_PORT" \
  ALLOW_FROM="$CHAT_ID" \
  node "$ROOT/telegram-bridge.mjs" > "$DEMO/bridge.log" 2>&1 &
BRIDGE_PID=$!
wait_for 30 "adapter registration" grep -q "registered adapter" "$DEMO/bridge.log"
grep "telegram probe ok\|registered adapter" "$DEMO/bridge.log"

step "Create an agent session and bind the Telegram DM to it"
SESSION_JSON="$("$GC" --city "$DEMO/city" session new assistant --no-attach --json 2>/dev/null | tail -1)"
SID="$(printf '%s' "$SESSION_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
SNAME="$(printf '%s' "$SESSION_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_name"])')"
echo "session: $SID ($SNAME)"
NUDGE_LOG="$DEMO_AGENT_STATE/$(printf '%s' "$SNAME" | tr -c 'A-Za-z0-9_.-' '_').nudges.log"
wait_for 60 "session start" test -e "$DEMO_AGENT_STATE/$(printf '%s' "$SNAME" | tr -c 'A-Za-z0-9_.-' '_').running"

CONV="{\"scope_id\":\"$CITY_NAME\",\"provider\":\"telegram\",\"account_id\":\"default\",\"conversation_id\":\"$CHAT_ID\",\"kind\":\"dm\"}"
curl -fsX POST "$BASE/extmsg/bind" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"conversation\":$CONV}" >/dev/null
echo "bound chat $CHAT_ID (dm) -> $SID"

step "INBOUND: simulate an incoming Telegram message"
INBOUND_TEXT="Hey, what is the status of the build?"
python3 -c 'import json,sys; print(json.dumps({"text": sys.argv[1], "sender_id": int(sys.argv[2])}))' \
  "$INBOUND_TEXT" "$CHAT_ID" >> "$FAKE_TG_DIR/inbox.jsonl"
echo "appended to fake Bot API inbox: \"$INBOUND_TEXT\""
wait_for 30 "bridge forward" grep -q "inbound $CHAT_ID" "$DEMO/bridge.log"
grep "inbound $CHAT_ID" "$DEMO/bridge.log" | tail -1
wait_for 30 "agent nudge" grep -q "status of the build" "$NUDGE_LOG"
bold "agent session received:"
sed -n '/<system-reminder>/,/<\/system-reminder>/p' "$NUDGE_LOG" | head -20

step "OUTBOUND: the session replies through gc"
REPLY="**assistant:** Build is green — all tests passing."
OUTBOUND_BODY="$(python3 -c 'import json,sys; print(json.dumps({"session_id": sys.argv[1], "conversation": json.loads(sys.argv[2]), "text": sys.argv[3]}))' "$SID" "$CONV" "$REPLY")"
RECEIPT="$(curl -fsX POST "$BASE/extmsg/outbound" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' -d "$OUTBOUND_BODY")"
printf '%s' "$RECEIPT" | python3 -c 'import json,sys; r=json.load(sys.stdin)["Receipt"]; print("receipt: delivered=%s message_id=%s" % (r["Delivered"], r["MessageID"]))'
wait_for 10 "fake Bot API delivery" grep -q "Build is green" "$FAKE_TG_DIR/outbox.jsonl"
bold "delivered to Telegram (note: markdown converted to Telegram HTML by openclaw's send pipeline):"
tail -1 "$FAKE_TG_DIR/outbox.jsonl"

step "Conversation transcript (gc extmsg fabric)"
curl -fs "$BASE/extmsg/transcript?scope_id=$CITY_NAME&provider=telegram&account_id=default&conversation_id=$CHAT_ID&kind=dm" \
  | python3 -c 'import json,sys; [print("  %2d %-9s %s" % (r["Sequence"], r["Kind"], r["Text"])) for r in json.load(sys.stdin)["items"]]'

step "CHILD CONVERSATION: per-workstream forum topic in a supergroup"
GROUP_ID="-1007113355" # negative id = group chat; the fake treats it as a forum supergroup
GROUP_CONV="{\"scope_id\":\"$CITY_NAME\",\"provider\":\"telegram\",\"account_id\":\"default\",\"conversation_id\":\"$GROUP_ID\",\"kind\":\"room\"}"
CHILD_JSON="$(curl -fsX POST "http://127.0.0.1:$BRIDGE_PORT/child-conversation" -H 'Content-Type: application/json' \
  -d "{\"conversation\":$GROUP_CONV,\"label\":\"build-pipeline\"}")"
CHILD_ID="$(printf '%s' "$CHILD_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["conversation_id"])')"
printf '%s' "$CHILD_JSON" | python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["kind"]=="thread" and c["parent_conversation_id"], c; print("child conversation: %s (parent %s)" % (c["conversation_id"], c["parent_conversation_id"]))'
# DMs cannot host forum topics — the bridge must refuse cleanly, not 500.
DM_CHILD_STATUS="$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$BRIDGE_PORT/child-conversation" \
  -H 'Content-Type: application/json' -d "{\"conversation\":$CONV,\"label\":\"nope\"}")"
[ "$DM_CHILD_STATUS" = "400" ] || die "child-conversation on a DM returned $DM_CHILD_STATUS, want 400"
echo "child-conversation on a DM correctly rejected (400)"

CHILD_CONV="{\"scope_id\":\"$CITY_NAME\",\"provider\":\"telegram\",\"account_id\":\"default\",\"conversation_id\":\"$CHILD_ID\",\"parent_conversation_id\":\"$GROUP_ID\",\"kind\":\"thread\"}"
curl -fsX POST "$BASE/extmsg/bind" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"$SID\",\"conversation\":$CHILD_CONV}" >/dev/null
echo "bound child conversation $CHILD_ID -> $SID"

TOPIC_REPLY="**assistant:** Pipeline thread opened — tracking the build here."
TOPIC_BODY="$(python3 -c 'import json,sys; print(json.dumps({"session_id": sys.argv[1], "conversation": json.loads(sys.argv[2]), "text": sys.argv[3]}))' "$SID" "$CHILD_CONV" "$TOPIC_REPLY")"
curl -fsX POST "$BASE/extmsg/outbound" -H 'X-GC-Request: 1' -H 'Content-Type: application/json' -d "$TOPIC_BODY" >/dev/null
wait_for 10 "topic delivery" grep -q "message_thread_id" "$FAKE_TG_DIR/outbox.jsonl"
bold "delivered into the forum topic (note message_thread_id):"
grep "message_thread_id" "$FAKE_TG_DIR/outbox.jsonl" | tail -1

TOPIC_ID="${CHILD_ID##*:topic:}"
python3 -c 'import json,sys; print(json.dumps({"text": sys.argv[1], "sender_id": int(sys.argv[2]), "chat_id": int(sys.argv[3]), "chat_type": "supergroup", "message_thread_id": int(sys.argv[4])}))' \
  "Looks good, keep me posted in this thread." "$CHAT_ID" "$GROUP_ID" "$TOPIC_ID" >> "$FAKE_TG_DIR/inbox.jsonl"
wait_for 30 "topic inbound" grep -qF "inbound $CHILD_ID" "$DEMO/bridge.log"
grep -F "inbound $CHILD_ID" "$DEMO/bridge.log" | tail -1
wait_for 30 "topic nudge" grep -q "keep me posted in this thread" "$NUDGE_LOG"

step "ALLOW_FROM: an unallowed sender is dropped at the bridge edge"
python3 -c 'import json,sys; print(json.dumps({"text": "ignore me, I am a stranger", "sender_id": 999999, "username": "stranger"}))' \
  >> "$FAKE_TG_DIR/inbox.jsonl"
wait_for 30 "edge drop" grep -q "dropping inbound from unallowed sender 999999" "$DEMO/bridge.log"
grep "dropping inbound from unallowed sender 999999" "$DEMO/bridge.log" | tail -1
grep -q "ignore me, I am a stranger" "$DEMO/bridge.log" && die "stranger text reached the inbound forward path"
echo "stranger never reached gc (no inbound forward logged)"

bold "child-conversation transcript (own thread, parented on the group chat):"
curl -fs "$BASE/extmsg/transcript?scope_id=$CITY_NAME&provider=telegram&account_id=default&conversation_id=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$CHILD_ID")&parent_conversation_id=$GROUP_ID&kind=thread" \
  | python3 -c 'import json,sys; [print("  %2d %-9s %s" % (r["Sequence"], r["Kind"], r["Text"])) for r in json.load(sys.stdin)["items"]]'

bold ""
bold "DEMO COMPLETE: openclaw's shipped telegram send pipeline (npm, unmodified) +"
bold "a 30-line getUpdates poll drove Gas City's extmsg fabric end to end."
bold "Same bridge, real Telegram: TELEGRAM_BOT_TOKEN=<BotFather token> node telegram-bridge.mjs"
