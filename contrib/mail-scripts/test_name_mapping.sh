#!/usr/bin/env bash
# Test: mcp_to_gc_name resolves unknown names via whois when cache is cold.
#
# Simulates the cross-pod scenario: pod A registers agent "mayor" (caching
# the gc→mcp mapping locally), then pod B (cold cache) receives a message
# from the mapped mcp name and must reverse-map it back to "mayor".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0

# --- Test helpers ---

assert_eq() {
  local label="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (expected '$expected', got '$actual')"
    FAIL=$((FAIL + 1))
  fi
}

# --- Set up isolated environment ---

export CACHE_DIR
CACHE_DIR=$(mktemp -d)
trap 'rm -rf "$CACHE_DIR"' EXIT
mkdir -p "$CACHE_DIR/name-map" "$CACHE_DIR/msg-agent" "$CACHE_DIR/msg-read" "$CACHE_DIR/msg-thread"

export MCP_URL="http://test-not-real:9999"
export GC_MCP_MAIL_URL="$MCP_URL"
export GC_MCP_MAIL_PROJECT="/test"
export PROJECT="/test"

# Source the bridge script (functions only, main() is guarded).
# shellcheck source=gc-mail-mcp-agent-mail
source "$SCRIPT_DIR/gc-mail-mcp-agent-mail"

# --- Mock registry for whois ---
# Stores task_description by mcp agent name.
MOCK_REGISTRY_DIR="$CACHE_DIR/mock-registry"
mkdir -p "$MOCK_REGISTRY_DIR"

# Override mcp_call to serve whois from the mock registry.
mcp_call() {
  local tool="$1"
  local arguments="$2"
  if [ "$tool" = "whois" ]; then
    local name
    name=$(echo "$arguments" | jq -r '.agent_name')
    local desc
    desc=$(cat "$MOCK_REGISTRY_DIR/$name" 2>/dev/null || true)
    if [ -n "$desc" ]; then
      jq -n --arg name "$name" --arg desc "$desc" \
        '{name: $name, task_description: $desc, program: "gc", model: "agent"}'
    else
      echo ""
    fi
  elif [ "$tool" = "register_agent" ]; then
    # Store task_description in mock registry if provided.
    local name desc
    name=$(echo "$arguments" | jq -r '.name')
    desc=$(echo "$arguments" | jq -r '.task_description // empty')
    if [ -n "$desc" ]; then
      printf '%s' "$desc" > "$MOCK_REGISTRY_DIR/$name"
    fi
    jq -n --arg name "$name" '{name: $name, id: 1}'
  else
    echo ""
  fi
}

# ============================================================
# Test 1: warm cache — gc_to_mcp_name then mcp_to_gc_name
# ============================================================
echo "Test 1: warm cache reverse mapping"

mcp_name=$(gc_to_mcp_name "mayor")
result=$(mcp_to_gc_name "$mcp_name")
assert_eq "warm cache resolves mayor" "mayor" "$result"

# ============================================================
# Test 2: cold cache — mcp_to_gc_name with no local mapping
# ============================================================
echo "Test 2: cold cache reverse mapping (cross-pod scenario)"

# Simulate pod A: register and map "mayor".
mcp_name=$(gc_to_mcp_name "mayor")
# Simulate ensure_agent storing task_description on the server.
ensure_agent "mayor"

# Simulate pod B: clear the local cache entirely.
rm -rf "$CACHE_DIR/name-map/"*

# Pod B receives a message from mcp_name — can it resolve back to "mayor"?
result=$(mcp_to_gc_name "$mcp_name")
assert_eq "cold cache resolves mayor via whois" "mayor" "$result"

# ============================================================
# Test 3: qualified names — rig-scoped agent "corp/clerk"
# ============================================================
echo "Test 3: qualified name (rig-scoped agent)"

mcp_name=$(gc_to_mcp_name "corp/clerk")
ensure_agent "corp/clerk"

# Clear cache to simulate cross-pod.
rm -rf "$CACHE_DIR/name-map/"*

result=$(mcp_to_gc_name "$mcp_name")
assert_eq "cold cache resolves corp/clerk via whois" "corp/clerk" "$result"

# ============================================================
# Test 4: unknown agent — no registration, no cache
# ============================================================
echo "Test 4: unknown agent falls back to mcp name"

rm -rf "$CACHE_DIR/name-map/"*
result=$(mcp_to_gc_name "TotallyUnknownAgent")
assert_eq "unknown agent returns mcp name as-is" "TotallyUnknownAgent" "$result"

# ============================================================
# Results
# ============================================================
echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
