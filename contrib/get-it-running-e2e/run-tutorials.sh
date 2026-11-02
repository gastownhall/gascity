#!/usr/bin/env bash
# Walks the user docs the way a reader does: one fresh gce2e-base container,
# and one Claude Code session per page (installation, quickstart, tutorials
# 01-07, in order) that follows the page literally and logs every step. A page
# passes when it logged steps and none failed or needed improvising; skipped
# steps (alternative install methods, optional cleanup) are listed in the
# summary but do not fail a page.
#
# Usage: run-tutorials.sh <round> <source>
#   <source> is a gascity checkout directory (uncommitted changes included) or
#   a gastownhall/gascity ref: a branch, a commit, or pr/<number>. Its docs/
#   are the pages under test, and by default its gc is built and installed in
#   place of the Homebrew gc once the Installation page is done, so the
#   tutorials run against the change under review.
#
# Environment (and GCE2E_HOME and GCE2E_CRED, as in run-variant.sh):
#   GCE2E_GC=release   keep the latest Homebrew gc instead of building <source>
set -uo pipefail

round="$1" source="$2"
home_dir="${GCE2E_HOME:-$HOME/gce2e}"
cred="${GCE2E_CRED:-$home_dir/.cred.json}"
gc_mode="${GCE2E_GC:-source}"
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
case "$gc_mode" in source | release) ;; *) echo "GCE2E_GC must be source or release" >&2; exit 2 ;; esac

# Resolve <source> to a checkout. A ref is checked out as a detached worktree
# of the harness's gascity clone, removed again when the round ends.
worktree=""
if [ -d "$source" ]; then
  src=$(cd "$source" && pwd)
else
  clone="$home_dir/gascity"
  [ -d "$clone/.git" ] || git clone -q https://github.com/gastownhall/gascity.git "$clone" || exit 2
  case "$source" in pr/*) ref="pull/${source#pr/}/head" ;; *) ref="$source" ;; esac
  git -C "$clone" fetch -q origin "$ref" || { echo "cannot fetch $source" >&2; exit 2; }
  worktree="$home_dir/src-t${round}"
  git -C "$clone" worktree remove -f "$worktree" 2>/dev/null
  git -C "$clone" worktree add -q --detach "$worktree" FETCH_HEAD || exit 2
  src="$worktree"
  trap 'git -C "$clone" worktree remove -f "$worktree"' EXIT
fi
[ -d "$src/docs/tutorials" ] || { echo "no docs/tutorials under $src" >&2; exit 2; }
log "source $source -> $src ($(git -C "$src" rev-parse --short HEAD 2>/dev/null || echo not-git)), gc=$gc_mode"

if [ "$gc_mode" = source ]; then
  # Built the way .goreleaser.yml builds a release (static, CGO off) and
  # versioned the way make build versions one: the exact tag, or dev.
  gc_version=$(git -C "$src" describe --tags --exact-match 2>/dev/null | sed 's/^v//')
  gc_version="${gc_version:-dev}"
  gc_commit=$(git -C "$src" rev-parse --short HEAD 2>/dev/null || echo unknown)
  [ -n "$(git -C "$src" status --porcelain 2>/dev/null)" ] && gc_commit="$gc_commit-dirty"
  log "build gc $gc_version ($gc_commit)"
  if ! (cd "$src" && CGO_ENABLED=0 GOTOOLCHAIN=auto go build -buildvcs=false -trimpath \
    -ldflags "-s -w -X main.version=$gc_version -X main.commit=$gc_commit -X main.date=$(date -u +%FT%TZ)" \
    -o "$out/gc" ./cmd/gc) > "$out/build.log" 2>&1; then
    log "HARNESS-ERROR: gc build failed; see $out/build.log"
    exit 2
  fi
fi

log "launch"
incus launch gce2e-base "$c" >/dev/null
for _ in $(seq 1 60); do incus exec "$c" -- systemctl is-system-running --wait >/dev/null 2>&1 && break; sleep 2; done
if [ "$(incus list "$c" -c s --format csv)" != RUNNING ]; then
  log "HARNESS-ERROR: container did not boot"
  exit 2
fi
in_box 'mkdir -p ~/.claude ~/walkthrough'
incus file push --mode 0600 "$cred" "$c/home/dev/.claude/.credentials.json" >/dev/null
tar -C "$src/docs" -cf - . | incus exec "$c" -- bash -c 'mkdir -p /opt/docs && tar -C /opt/docs -xf - && chmod -R a+rX /opt/docs'
incus exec "$c" -- chown -R dev:dev /home/dev/.claude /home/dev/walkthrough

# Puts the branch gc where Homebrew's gc is, so every page after Installation
# runs it, and warns when the branch pins a different bd than Homebrew gave.
swap_in_source_gc() {
  incus file push --mode 0755 "$out/gc" "$c/tmp/gc-under-test" >/dev/null || return 1
  # shellcheck disable=SC2016 # brew --prefix expands inside the container
  in_box 'install -m 0755 /tmp/gc-under-test "$(brew --prefix)/bin/gc"' || return 1
  got=$(in_box 'gc version' 2>&1 | head -1)
  log "gc under test: $got (commit $gc_commit)"
  [ "$got" = "$gc_version" ] || return 1
  want_bd=$(sed -n 's/^BD_VERSION=v//p' "$src/deps.env" 2>/dev/null)
  have_bd=$(in_box 'bd version' 2>/dev/null | head -1)
  case "$have_bd" in *"$want_bd"*) ;; *) log "WARNING: branch pins bd $want_bd but Homebrew installed: $have_bd" ;; esac
}

build_note=""
i=0
for page in "${pages[@]}"; do
  i=$((i + 1))
  name=$(basename "$page" .md)
  report="/home/dev/walkthrough/$(printf '%02d' "$i")-$name.jsonl"
  prompt="You are a new Gas City user working through its documentation in order, on this machine. The docs are in /opt/docs (a Mintlify site: a link like /tutorials/02-agents means /opt/docs/tutorials/02-agents.md). Earlier pages in the reading order have already been followed in this machine's state; do not redo them.${build_note}

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

  if [ "$i" = 1 ] && [ "$gc_mode" = source ]; then
    if ! swap_in_source_gc; then
      log "HARNESS-ERROR: could not install the gc under test over the Homebrew gc"
      exit 2
    fi
    build_note=" The gc on this machine is a development build of the change under review, so gc version prints $gc_version rather than the latest release number; that is expected."
  fi
done

log "summary:"
for f in "$out"/[0-9][0-9]-*.jsonl; do
  case "$f" in *.transcript.jsonl) continue ;; esac
  jq -r 'select(.result != "worked") | "  \(.page) | \(.result) | \(.step) | \(.note)"' "$f" 2>/dev/null
done | tee -a "$out/harness.log"
log "container $c kept for inspection; delete with: incus delete -f $c"
