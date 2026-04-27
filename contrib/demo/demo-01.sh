#!/usr/bin/env bash
# demo-01.sh — Gas City onboarding demo: zero to orchestration in 3 minutes.
#
# Three progressive demos showing Gas City's core dispatch workflow:
#
#   Part 1: Init city, add rig, sling inline text → pool agent writes README
#   Part 2: Create explicit bead, sling it → agent writes hello-world
#   Part 3: Create convoy with 3 variants, sling convoy → 3 parallel pool agents
#
# Prerequisites:
#   - gc, bd, jq, tmux in PATH
#   - Claude Code installed (provider: claude)
#
# Usage:
#   ./demo-01.sh             # run all three parts interactively
#   ./demo-01.sh part1       # run only Part 1
#   ./demo-01.sh part2       # run only Part 2
#   ./demo-01.sh part3       # run only Part 3
#
# Environment:
#   DEMO_CITY  — city directory (default: ~/onboarding-demo)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=narrate.sh
source "$SCRIPT_DIR/narrate.sh"

DEMO_CITY="${DEMO_CITY:-$HOME/onboarding-demo}"
RIG_NAME="my-project"
RIG_DIR="$DEMO_CITY/$RIG_NAME"
POOL="$RIG_NAME/claude"

# Isolate from other gc processes (tests, other cities) that pollute
# the shared ~/.gc registry. All demo state lives under GC_HOME.
export GC_HOME="${GC_HOME:-$HOME/.gc-demo}"
GC_BIN="${GC_BIN:-gc}"

# ── Preflight checks ─────────────────────────────────────────────────────

preflight() {
    local missing=()
    for cmd in "$GC_BIN" bd jq tmux; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        echo "Missing required commands: ${missing[*]}" >&2
        exit 1
    fi
}

# ── Cleanup ──────────────────────────────────────────────────────────────

cleanup() {
    if [ -d "$DEMO_CITY" ]; then
        (cd "$DEMO_CITY" && "$GC_BIN" stop 2>/dev/null) || true
    fi
    # Kill all non-system dolt servers (system dolt runs on port 3307).
    ps aux | grep "dolt sql-server" | grep -v grep | grep -v "port=3307" \
        | awk '{print $2}' | xargs -r kill -9 2>/dev/null || true
    rm -rf "$DEMO_CITY"
    # Restart supervisor under isolated GC_HOME with current binary.
    local pid
    pid=$("$GC_BIN" supervisor status 2>&1 | grep -oP 'PID \K\d+' || true)
    if [ -n "$pid" ]; then
        kill -9 "$pid" 2>/dev/null || true
    fi
    rm -rf "$GC_HOME"
    mkdir -p "$GC_HOME"
    sleep 1
    "$GC_BIN" supervisor start 2>/dev/null || true
}

# ── Helpers ──────────────────────────────────────────────────────────────

# run_show — Echo a command, then run it. Makes the demo self-documenting.
run_show() {
    echo -e "  ${NARR_DIM}\$ $*${NARR_NC}"
    "$@"
    echo ""
}

# bd_in_rig — Run bd from the rig directory so beads land in the rig store.
bd_in_rig() {
    (cd "$RIG_DIR" && bd "$@")
}

# bd_create_in_rig — Create a bead in the rig store, return the new bead ID.
bd_create_in_rig() {
    local id
    id=$(cd "$RIG_DIR" && bd create --json "$@" | jq -r '.id')
    echo "$id"
}

# wait_for_bead_closed — Poll until a bead reaches "closed" status.
wait_for_bead_closed() {
    local bead_id="$1"
    local timeout="${2:-120}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        local status
        status=$(cd "$RIG_DIR" && bd show --json "$bead_id" 2>/dev/null \
            | jq -r '.[0].status // "unknown"' 2>/dev/null || echo "unknown")
        if [ "$status" = "closed" ]; then
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
        printf "\r  ${NARR_DIM}Waiting for %s... (%ds)${NARR_NC}  " "$bead_id" "$elapsed"
    done
    echo ""
    echo "  Timed out waiting for $bead_id after ${timeout}s"
    return 1
}

# wait_for_pool_agent — Poll gc rig status until a pool agent appears running.
wait_for_pool_agent() {
    local timeout="${1:-60}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        local status_output
        status_output=$(cd "$DEMO_CITY" && "$GC_BIN" rig status "$RIG_NAME" 2>/dev/null || true)
        if echo "$status_output" | grep -q "running"; then
            echo ""
            echo "$status_output"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
        printf "\r  ${NARR_DIM}Waiting for pool agent... (%ds)${NARR_NC}  " "$elapsed"
    done
    echo ""
    echo "  (Pool agent did not appear within ${timeout}s — continuing)"
    return 0
}

# show_rig_files — List files in the rig directory (excluding .beads/).
show_rig_files() {
    echo -e "  ${NARR_CYAN}Files in $RIG_NAME/:${NARR_NC}"
    (cd "$RIG_DIR" && find . -not -path './.beads*' -not -path '.' -not -path './.git*' \
        | sort | while read -r f; do echo "    $f"; done) || echo "    (empty)"
    echo ""
}

# ── Part 1: Zero to Working ─────────────────────────────────────────────

part1() {
    narrate "Part 1: Zero to Working" --sub "Init city, add rig, sling inline text"

    step "Initialize a new city"
    run_show "$GC_BIN" init "$DEMO_CITY"

    step "Default city.toml"
    echo -e "  ${NARR_DIM}\$ cat $DEMO_CITY/city.toml${NARR_NC}"
    cat "$DEMO_CITY/city.toml"
    echo ""

    step "Add a rig (project directory)"
    mkdir -p "$RIG_DIR"
    echo -e "  ${NARR_DIM}\$ gc rig add $RIG_NAME${NARR_NC}"
    (cd "$DEMO_CITY" && "$GC_BIN" rig add "$RIG_NAME")
    echo ""

    step "Rig directory is empty — no code yet"
    echo -e "  ${NARR_DIM}\$ ls -la $RIG_DIR/${NARR_NC}"
    ls -la "$RIG_DIR/" | grep -v '^\.' | head -5 || echo "    (empty — just .beads/)"
    echo ""

    # Ensure reconciler picks up the new rig's implicit pool agents.
    "$GC_BIN" supervisor reload 2>/dev/null || true

    step "Sling inline text — creates a bead and routes it to the pool"
    echo -e "  ${NARR_DIM}\$ gc sling $POOL \"Write a README.md for this project\" ${NARR_NC}"
    local sling_output
    sling_output=$(cd "$DEMO_CITY" && "$GC_BIN" sling "$POOL" "Write a README.md for this project" 2>&1) || true
    echo "$sling_output"
    echo ""

    # Extract bead ID from "Created mp-xxx" line.
    local sling_bead_id
    sling_bead_id=$(echo "$sling_output" | grep -oP 'Created \K\S+' || true)

    step "Watch the agent work (Ctrl+C to skip ahead)"
    echo -e "  ${NARR_DIM}Tracking bead $sling_bead_id${NARR_NC}"
    trap true INT
    while true; do
        local status
        status=$(cd "$RIG_DIR" && bd show --json "$sling_bead_id" 2>/dev/null \
            | jq -r '.[0].status // "unknown"' 2>/dev/null || echo "unknown")
        printf "\r  Bead %s: %s  " "$sling_bead_id" "$status"
        if [ "$status" = "closed" ]; then
            echo ""
            break
        fi
        sleep 3
    done
    trap - INT
    echo ""

    step "The agent wrote a README"
    echo -e "  ${NARR_DIM}\$ cat $RIG_DIR/README.md${NARR_NC}"
    cat "$RIG_DIR/README.md" 2>/dev/null || echo "  (README.md not yet created — agent may still be working)"
    echo ""

    pause "Part 1 complete — press Enter to continue to Part 2..."
}

# ── Part 2: Explicit Bead ───────────────────────────────────────────────

part2() {
    narrate "Part 2: Explicit Bead" --sub "Create a bead, then sling it"

    step "Create a bead explicitly"
    echo -e "  ${NARR_DIM}\$ cd $RIG_DIR && bd create \"Write a hello-world script in the language of your choice\"${NARR_NC}"
    local bead_id
    bead_id=$(bd_create_in_rig "Write a hello-world script in the language of your choice")
    echo "  Created bead: $bead_id"
    echo ""

    step "Show the bead"
    echo -e "  ${NARR_DIM}\$ bd show $bead_id${NARR_NC}"
    bd_in_rig show "$bead_id"
    echo ""

    step "Sling the bead to the pool"
    echo -e "  ${NARR_DIM}\$ gc sling $POOL $bead_id${NARR_NC}"
    (cd "$DEMO_CITY" && "$GC_BIN" sling "$POOL" "$bead_id")
    echo ""

    step "Watch the bead get picked up (Ctrl+C to stop)"
    echo -e "  ${NARR_DIM}\$ watch bd show $bead_id${NARR_NC}"
    trap true INT
    while true; do
        local status
        status=$(cd "$RIG_DIR" && bd show --json "$bead_id" 2>/dev/null \
            | jq -r '.[0].status // "unknown"' 2>/dev/null || echo "unknown")
        printf "\r  Bead %s: %s  " "$bead_id" "$status"
        if [ "$status" = "closed" ]; then
            echo ""
            break
        fi
        sleep 3
    done
    trap - INT
    echo ""

    step "Show bead status"
    echo -e "  ${NARR_DIM}\$ bd show $bead_id${NARR_NC}"
    bd_in_rig show "$bead_id"
    echo ""

    step "Files created"
    show_rig_files

    pause "Part 2 complete — press Enter to continue to Part 3..."
}

# ── Part 3: Convoy Fan-Out ──────────────────────────────────────────────

part3() {
    narrate "Part 3: Convoy Fan-Out" --sub "3 beads in a convoy → 3 parallel pool agents"

    step "Create 3 beads"
    local id1 id2 id3
    id1=$(bd_create_in_rig "Hello world in Python")
    echo "  $id1 — Hello world in Python"
    id2=$(bd_create_in_rig "Hello world in Rust")
    echo "  $id2 — Hello world in Rust"
    id3=$(bd_create_in_rig "Hello world in Haskell")
    echo "  $id3 — Hello world in Haskell"
    echo ""

    step "Group them in a convoy"
    echo -e "  ${NARR_DIM}\$ gc convoy create \"Hello World Variants\" $id1 $id2 $id3${NARR_NC}"
    local convoy_output convoy_id
    convoy_output=$(cd "$DEMO_CITY" && "$GC_BIN" convoy create "Hello World Variants" "$id1" "$id2" "$id3" 2>&1) || true
    echo "$convoy_output"
    convoy_id=$(echo "$convoy_output" | grep -oP 'convoy \K\S+')
    echo ""

    step "Sling the convoy — expands to 3 parallel pool agents"
    echo -e "  ${NARR_DIM}\$ gc sling $POOL $convoy_id${NARR_NC}"
    (cd "$DEMO_CITY" && "$GC_BIN" sling "$POOL" "$convoy_id")
    echo ""

    step "Watch 3 pool agents spin up"
    wait_for_pool_agent 60

    step "Watch convoy progress (Ctrl+C to stop)"
    echo -e "  ${NARR_DIM}Tracking: $id1, $id2, $id3${NARR_NC}"
    trap true INT
    while true; do
        local s1 s2 s3 done_count
        s1=$(cd "$RIG_DIR" && bd show --json "$id1" 2>/dev/null | jq -r '.[0].status // "?"' 2>/dev/null || echo "?")
        s2=$(cd "$RIG_DIR" && bd show --json "$id2" 2>/dev/null | jq -r '.[0].status // "?"' 2>/dev/null || echo "?")
        s3=$(cd "$RIG_DIR" && bd show --json "$id3" 2>/dev/null | jq -r '.[0].status // "?"' 2>/dev/null || echo "?")
        done_count=0
        [ "$s1" = "closed" ] && done_count=$((done_count + 1))
        [ "$s2" = "closed" ] && done_count=$((done_count + 1))
        [ "$s3" = "closed" ] && done_count=$((done_count + 1))
        printf "\r  Python: %-12s  Rust: %-12s  Haskell: %-12s  [%d/3 done]  " "$s1" "$s2" "$s3" "$done_count"
        if [ "$done_count" -ge 3 ]; then
            echo ""
            break
        fi
        sleep 3
    done
    trap - INT
    echo ""

    step "Final bead status"
    echo -e "  ${NARR_DIM}\$ bd list${NARR_NC}"
    bd_in_rig list
    echo ""

    step "Files created"
    show_rig_files

    pause "Part 3 complete — press Enter to wrap up..."
}

# ── Finale ───────────────────────────────────────────────────────────────

finale() {
    narrate "Demo Complete" --sub "Gas City: orchestration as configuration"

    echo "  Three capabilities demonstrated:"
    echo ""
    echo "    1. Inline sling    — text in, agent out, zero setup"
    echo "    2. Explicit beads  — create work, route it, track it"
    echo "    3. Convoy fan-out    — one sling, N parallel agents"
    echo ""
    echo "  Same city. Same pool. Progressive complexity."
    echo ""

    step "Final state"
    show_rig_files
    echo -e "  ${NARR_DIM}\$ gc rig status $RIG_NAME${NARR_NC}"
    (cd "$DEMO_CITY" && "$GC_BIN" rig status "$RIG_NAME" 2>/dev/null) || true
    echo ""
}

# ── Main ─────────────────────────────────────────────────────────────────

main() {
    preflight

    local part="${1:-all}"

    case "$part" in
        part1)
            cleanup
            part1
            ;;
        part2)
            # Assumes Part 1 already ran (city exists).
            part2
            ;;
        part3)
            # Assumes Part 1 already ran (city exists).
            part3
            ;;
        all)
            cleanup

            narrate "Gas City Onboarding Demo" --sub "Zero to orchestration in 3 minutes"
            echo "  Part 1: Inline sling    — init city, sling text, watch agent work"
            echo "  Part 2: Explicit bead   — create bead, sling it"
            echo "  Part 3: Convoy fan-out    — 3 beads → 3 parallel agents"
            echo ""
            pause "Press Enter to begin..."

            part1
            part2
            part3
            finale
            ;;
        clean)
            cleanup
            echo "Cleaned up $DEMO_CITY"
            ;;
        *)
            echo "Usage: demo-01.sh [part1|part2|part3|all|clean]" >&2
            exit 1
            ;;
    esac
}

main "$@"
