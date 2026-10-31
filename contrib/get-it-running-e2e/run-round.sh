#!/usr/bin/env bash
# Runs all six "get it running" variants in parallel and prints a summary.
# Usage: run-round.sh <round>   (honors GCE2E_HOME, GCE2E_CRED, GCE2E_BRANCH)
#
# L1-L3 start Claude Code in an empty home directory and hand it the repo
# link; C1-C3 start it inside a fresh clone. No prompt mentions formulas or the
# dashboard: the README has to carry the agent to a completed formula run.
set -uo pipefail

round="$1"
here=$(cd "$(dirname "$0")" && pwd)
branch="${GCE2E_BRANCH:-main}"
url=https://github.com/gastownhall/gascity
[ "$branch" = main ] || url="$url/tree/$branch"
v="$here/run-variant.sh"

bash "$v" "$round" L1 link "Get this running: $url" &
bash "$v" "$round" L2 link "Set up $url on my machine and show me it working" &
bash "$v" "$round" L3 link "I want to try $url. Install it and get it going." &
bash "$v" "$round" C1 clone "get this running" &
bash "$v" "$round" C2 clone "set this up for me" &
bash "$v" "$round" C3 clone "how do I use this? get it going" &
n=0
for j in $(jobs -p); do
  wait "$j"
  n=$((n + 1))
  echo "[$(date +%T)] [$n/6] variants finished"
done

echo "== round $round summary"
for id in L1 L2 L3 C1 C2 C3; do
  jq -r '"\(.id) \(.verdict) \(.seconds)s turns=\(.turns) $\(.cost_usd) asks=\(.ask_user_calls) \(.reason)"' \
    "${GCE2E_HOME:-$HOME/gce2e}/results/r$round/$id/grade.json" 2>/dev/null || echo "$id NO-GRADE"
done
