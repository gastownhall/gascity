#!/usr/bin/env bash
# Walks the user docs the way a reader does, against the latest Homebrew
# release of gc: one fresh gce2e-base container, and one Claude Code session
# per page (installation, quickstart, tutorials 01-07, in order) that follows
# the page literally and logs every step. A page passes when it logged steps
# and none failed or needed improvising; skipped steps (alternative install
# methods, optional cleanup) are listed in the summary but do not fail a page.
#
# Usage: run-tutorials.sh <round> <docs-dir>
#   <docs-dir> is a docs/ tree to test (usually the working tree's docs/).
# Honors GCE2E_HOME and GCE2E_CRED like run-variant.sh.
set -uo pipefail

round="$1" docs_src="$2"
home_dir="${GCE2E_HOME:-$HOME/gce2e}"
cred="${GCE2E_CRED:-$home_dir/.cred.json}"
c="gce2e-t${round}"
out="$home_dir/results/t${round}"
mkdir -p "$out"
log() { echo "[$(date +%T)] [$c] $*" | tee -a "$out/harness.log"; }
in_box() { incus exec "$c" -- su - dev -c "$1"; }

pages=(
  getting-started/installation.md
  getting-started/quickstart.md
  tutorials/01-cities-and-rigs.md
  tutorials/02-agents.md
  tutorials/03-sessions.md
  tutorials/04-communication.md
  tutorials/05-formulas.md
  tutorials/06-beads.md
  tutorials/07-orders.md
)

[ -r "$cred" ] || { echo "missing Claude credential at $cred" >&2; exit 2; }
[ -d "$docs_src/tutorials" ] || { echo "no tutorials under $docs_src" >&2; exit 2; }

log "launch"
incus launch gce2e-base "$c" >/dev/null
for _ in $(seq 1 60); do incus exec "$c" -- systemctl is-system-running --wait >/dev/null 2>&1 && break; sleep 2; done
if [ "$(incus list "$c" -c s --format csv)" != RUNNING ]; then
  log "HARNESS-ERROR: container did not boot"
  exit 2
fi
in_box 'mkdir -p ~/.claude ~/walkthrough'
incus file push --mode 0600 "$cred" "$c/home/dev/.claude/.credentials.json" >/dev/null
tar -C "$docs_src" -cf - . | incus exec "$c" -- bash -c 'mkdir -p /opt/docs && tar -C /opt/docs -xf - && chmod -R a+rX /opt/docs'
incus exec "$c" -- chown -R dev:dev /home/dev/.claude /home/dev/walkthrough

i=0
for page in "${pages[@]}"; do
  i=$((i + 1))
  name=$(basename "$page" .md)
  report="/home/dev/walkthrough/$(printf '%02d' "$i")-$name.jsonl"
  prompt="You are a new Gas City user working through its documentation in order, on this machine. The docs are in /opt/docs (a Mintlify site: a link like /tutorials/02-agents means /opt/docs/tutorials/02-agents.md). Earlier pages in the reading order have already been followed in this machine's state; do not redo them.

Follow /opt/docs/$page from top to bottom, exactly as written:
- Run every command the page tells the reader to run, as written. Replace only placeholders the page itself tells you to fill in.
- Do not fix, work around, skip, or add steps the page does not give. If a step fails or its output does not match what the page says, record it and continue with the next step if you can.
- Wait for work the page says will happen (an agent finishing, a run closing) for up to 10 minutes per step before calling it failed.

Append one JSON object per step, one per line, to $report:
{\"page\": \"$page\", \"step\": \"<heading or short description>\", \"command\": \"<command run, or empty>\", \"result\": \"worked|failed|improvised|skipped\", \"note\": \"<for anything but worked: what happened and what the page should say instead>\"}
Use improvised when you had to do something the page did not tell you to. Use skipped only for steps that cannot run here (say why): steps the page marks optional or as an alternative to what you did, and steps that need an interactive terminal (attaching to a session, a second terminal window) because this shell has no TTY. A step that needs a second terminal can still be tried by running the long-lived command in the background; if that works, log it as worked.

Finish with a two-sentence summary of whether a reader could get through this page."
  log "page $i/${#pages[@]} start: $page"
  printf '%s' "$prompt" > "$out/prompt-$i.txt"
  if ! incus file push --mode 0644 "$out/prompt-$i.txt" "$c/tmp/prompt-$i.txt"; then
    log "HARNESS-ERROR: could not push the prompt for $page"
    exit 2
  fi
  start=$(date +%s)
  in_box "cd ~ && timeout 90m claude -p \"\$(cat /tmp/prompt-$i.txt)\" --dangerously-skip-permissions --output-format stream-json --verbose" \
    > "$out/$(printf '%02d' "$i")-$name.transcript.jsonl" 2> "$out/$(printf '%02d' "$i")-$name.stderr"
  rc=$?
  in_box "cat $report 2>/dev/null" > "$out/$(printf '%02d' "$i")-$name.jsonl"
  steps=$(jq -s 'length' "$out/$(printf '%02d' "$i")-$name.jsonl" 2>/dev/null || echo 0)
  bad=$(jq -s '[.[] | select(.result == "failed" or .result == "improvised")] | length' "$out/$(printf '%02d' "$i")-$name.jsonl" 2>/dev/null || echo "?")
  verdict=PASS
  { [ "$steps" -gt 0 ] && [ "$bad" = 0 ]; } || verdict=FAIL
  log "page $i/${#pages[@]} $verdict: $page steps=$steps not-worked=$bad exit=$rc $(($(date +%s) - start))s"
done

log "summary:"
for f in "$out"/[0-9][0-9]-*.jsonl; do
  case "$f" in *.transcript.jsonl) continue ;; esac
  jq -r 'select(.result != "worked") | "  \(.page) | \(.result) | \(.step) | \(.note)"' "$f" 2>/dev/null
done | tee -a "$out/harness.log"
log "container $c kept for inspection; delete with: incus delete -f $c"
