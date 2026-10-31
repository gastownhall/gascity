#!/usr/bin/env bash
# Runs one "get it running" variant in a fresh gce2e-base container and grades
# it. Usage: run-variant.sh <round> <id> <link|clone> <prompt>
#
# Environment:
#   GCE2E_HOME    results root and credential location (default ~/gce2e)
#   GCE2E_CRED    Claude Code credential JSON (default $GCE2E_HOME/.cred.json)
#   GCE2E_BRANCH  branch the clone variants check out (default main)
#
# A variant passes when the agent's final message links a formula run, the
# city's API reports that run completed, and hello.sh is committed in a repo
# under the dev user's home. Passing containers are deleted; failing ones are
# kept for inspection.
set -uo pipefail

round="$1" id="$2" mode="$3" prompt="$4"
home_dir="${GCE2E_HOME:-$HOME/gce2e}"
cred="${GCE2E_CRED:-$home_dir/.cred.json}"
branch="${GCE2E_BRANCH:-main}"
c="gce2e-r${round}-${id}"
out="$home_dir/results/r${round}/${id}"
mkdir -p "$out"
log() { echo "[$(date +%T)] [$c] $*" | tee -a "$out/harness.log"; }
grade() {
  jq -n --arg id "$id" --arg mode "$mode" --arg verdict "$1" --arg reason "$2" --arg link "${link:-}" \
    --arg dur "${dur:-0}" --arg rc "${rc:-}" --arg turns "${turns:-}" --arg cost "${cost:-}" --arg asked "${asked:-}" \
    '{id:$id,mode:$mode,verdict:$verdict,reason:$reason,link:$link,seconds:($dur|tonumber),exit:$rc,turns:$turns,cost_usd:$cost,ask_user_calls:$asked}' \
    > "$out/grade.json"
  log "$1: $2 ${link:-}"
}
in_box() { incus exec "$c" -- su - dev -c "$1"; }

[ -r "$cred" ] || { echo "missing Claude credential at $cred" >&2; exit 2; }

log "launch"
incus launch gce2e-base "$c" >/dev/null
for _ in $(seq 1 60); do incus exec "$c" -- systemctl is-system-running --wait >/dev/null 2>&1 && break; sleep 2; done
if [ "$(incus list "$c" -c s --format csv)" != RUNNING ]; then
  grade HARNESS-ERROR "container did not boot"
  exit 2
fi
in_box 'mkdir -p ~/.claude'
incus file push --mode 0600 "$cred" "$c/home/dev/.claude/.credentials.json" >/dev/null
incus exec "$c" -- chown -R dev:dev /home/dev/.claude

workdir=/home/dev
if [ "$mode" = clone ]; then
  log "clone $branch"
  in_box "git clone -q -b $branch https://github.com/gastownhall/gascity.git ~/gascity"
  workdir=/home/dev/gascity
fi
printf '%s' "$prompt" > "$out/prompt.txt"
incus file push "$out/prompt.txt" "$c/tmp/prompt.txt" >/dev/null

log "agent start (mode=$mode)"
start=$(date +%s)
in_box "cd $workdir && timeout 60m claude -p \"\$(cat /tmp/prompt.txt)\" --dangerously-skip-permissions --output-format stream-json --verbose" \
  > "$out/transcript.jsonl" 2> "$out/agent.stderr"
rc=$?
dur=$(($(date +%s) - start))
log "agent exit=$rc after ${dur}s"

t="$out/transcript.jsonl"
jq -r 'select(.type=="result") | .result // empty' "$t" > "$out/final.txt"
asked=$(grep -c '"name":"AskUserQuestion"' "$t" || true)
turns=$(jq -s '[.[]|select(.type=="result")|.num_turns]|add' "$t")
cost=$(jq -s '[.[]|select(.type=="result")|.total_cost_usd]|add' "$t")
link=$(grep -oE 'http://(127\.0\.0\.1|localhost):[0-9]+/city/[^/ )`"]+/runs/[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*' "$out/final.txt" | tail -1)
if [ -z "$link" ]; then
  grade FAIL "no run link in final message"
  exit 1
fi

city=$(sed -E 's#.*/city/([^/]+)/runs/.*#\1#' <<<"$link")
run=$(sed -E 's#.*/runs/##' <<<"$link")
status=""
# The run projection answers 503 while a new city is still setting up, and a
# run can still be finishing when the agent reports; wait up to ten minutes.
for _ in $(seq 1 120); do
  body=$(in_box "curl -s http://127.0.0.1:8372/v0/city/$city/runs/$run")
  status=$(jq -r '.run_id as $r | if $r then .status else empty end' <<<"$body" 2>/dev/null)
  [ "$status" = completed ] && break
  sleep 5
done
printf '%s\n' "$body" > "$out/run-api.txt"
if [ "$status" != completed ]; then
  grade FAIL "run $run status is '${status:-unresolved}', want completed"
  exit 1
fi

committed=$(in_box 'for f in $(find ~ -maxdepth 3 -name hello.sh -not -path "*/.gc/*" -not -path "*/gascity/*"); do git -C "$(dirname "$f")" log --oneline -- hello.sh | head -1; done')
if [ -z "$committed" ]; then
  grade FAIL "run $run completed but no committed hello.sh"
  exit 1
fi

grade PASS "run $run completed; hello.sh committed"
incus delete -f "$c" >/dev/null
