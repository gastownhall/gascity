package exec //nolint:revive // internal package, always imported with alias

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

// TestMCPMailConformance runs the mail conformance suite against the
// gc-mail-mcp-agent-mail contrib script with a mock curl. This validates
// that the MCP bridge script conforms to the mail.Provider contract when
// run through the exec provider. Requires jq on PATH.
func TestMCPMailConformance(t *testing.T) {
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}

	// Locate the real MCP mail script relative to the module root.
	scriptPath, err := findMCPScript()
	if err != nil {
		t.Skipf("MCP mail script not found: %v", err)
	}

	mailtest.RunProviderTests(t, func(t *testing.T) mail.Provider {
		dir := t.TempDir()

		// State directory for the mock curl.
		stateDir := filepath.Join(dir, "state")
		for _, sub := range []string{"agents", "messages"} {
			if err := os.MkdirAll(filepath.Join(stateDir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(stateDir, "next_id"), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Write mock curl.
		binDir := filepath.Join(dir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(mcpMockCurl(stateDir)), 0o755); err != nil {
			t.Fatal(err)
		}

		// Write wrapper script that sets env and delegates to the real script.
		wrapperPath := filepath.Join(dir, "mail-provider")
		if err := os.WriteFile(wrapperPath, []byte(mcpWrapper(binDir, scriptPath, stateDir)), 0o755); err != nil {
			t.Fatal(err)
		}

		return NewProvider(wrapperPath)
	})
}

// TestMCPMailCrossPodNameResolution verifies that when GC_CITY is set,
// two independent provider instances (simulating separate K8s pods) share
// the name cache via the city directory, allowing the receiver to resolve
// the sender's gc name without calling mcp_agent_mail's whois API.
func TestMCPMailCrossPodNameResolution(t *testing.T) {
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	scriptPath, err := findMCPScript()
	if err != nil {
		t.Skipf("MCP mail script not found: %v", err)
	}

	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Shared mock MCP state — both "pods" talk to the same mock server.
	stateDir := filepath.Join(dir, "state")
	for _, sub := range []string{"agents", "messages"} {
		if err := os.MkdirAll(filepath.Join(stateDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "next_id"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(mcpMockCurl(stateDir)), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pod A: sends a message from "mayor" to "clerk".
	wrapperA := filepath.Join(dir, "pod-a")
	if err := os.WriteFile(wrapperA, []byte(mcpWrapperWithCity(binDir, scriptPath, stateDir, cityDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	podA := NewProvider(wrapperA)

	msg, err := podA.Send("mayor", "clerk", "hello", "test message")
	if err != nil {
		t.Fatalf("pod A Send: %v", err)
	}
	if msg.From != "mayor" {
		t.Errorf("Send.From = %q, want %q", msg.From, "mayor")
	}

	// Pod B: a SEPARATE provider instance (fresh process, no in-memory cache).
	// It shares the city dir, so the name cache is shared on disk.
	wrapperB := filepath.Join(dir, "pod-b")
	if err := os.WriteFile(wrapperB, []byte(mcpWrapperWithCity(binDir, scriptPath, stateDir, cityDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	podB := NewProvider(wrapperB)

	// Pod B reads the inbox for "clerk" — should resolve "mayor" from the
	// shared cache, not fall back to the raw mcp name.
	msgs, err := podB.Inbox("clerk")
	if err != nil {
		t.Fatalf("pod B Inbox: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("pod B Inbox returned 0 messages, want 1")
	}
	if msgs[0].From != "mayor" {
		t.Errorf("pod B Inbox[0].From = %q, want %q (shared cache should resolve gc name)", msgs[0].From, "mayor")
	}

	// Verify cache is under city dir, not /tmp.
	cacheGlob, _ := filepath.Glob(filepath.Join(cityDir, ".gc", "mail-cache", "*", "name-map", "mayor"))
	if len(cacheGlob) == 0 {
		t.Error("name cache not found under GC_CITY/.gc/mail-cache/")
	}
}

// findMCPScript locates contrib/mail-scripts/gc-mail-mcp-agent-mail by
// walking up from the working directory to find the module root.
func findMCPScript() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "contrib", "mail-scripts", "gc-mail-mcp-agent-mail")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// mcpWrapper returns a shell script that sets up the mock environment and
// delegates to the real gc-mail-mcp-agent-mail script.
func mcpWrapper(binDir, scriptPath, stateDir string) string {
	return mcpWrapperWithCity(binDir, scriptPath, stateDir, "")
}

// mcpWrapperWithCity returns a wrapper that also sets GC_CITY for shared cache.
func mcpWrapperWithCity(binDir, scriptPath, stateDir, cityDir string) string {
	// Use stateDir as the project key — unique per test, and an absolute
	// path as required by mcp_agent_mail's human_key validation.
	cityExport := ""
	if cityDir != "" {
		cityExport = `export GC_CITY="` + cityDir + `"` + "\n"
	}
	return `#!/usr/bin/env bash
set -euo pipefail
export PATH="` + binDir + `:$PATH"
export GC_MOCK_STATE_DIR="` + stateDir + `"
export GC_MCP_MAIL_URL="http://127.0.0.1:8765"
export GC_MCP_MAIL_PROJECT="` + stateDir + `"
` + cityExport + `exec "` + scriptPath + `" "$@"
`
}

// mcpMockCurl returns a mock curl script that simulates mcp_agent_mail v0.3.0.
// Matches the real API: ensure_project uses human_key, register_agent accepts
// name+program+model, send_message returns deliveries format,
// acknowledge_message requires agent_name, get_message is removed.
func mcpMockCurl(stateDir string) string {
	return `#!/usr/bin/env bash
set -euo pipefail

STATE_DIR="` + stateDir + `"

next_id() {
  local id
  id=$(cat "$STATE_DIR/next_id")
  echo $((id + 1)) > "$STATE_DIR/next_id"
  echo "$id"
}

now_ts() {
  date -u "+%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "2026-02-28T12:00:00Z"
}

# Parse curl args to extract URL and data.
url="" data=""
while [ $# -gt 0 ]; do
  case "$1" in
    -s) shift ;;
    -X) shift 2 ;;
    -H) shift 2 ;;
    -d) data="$2"; shift 2 ;;
    -o) shift 2 ;;
    -w) shift 2 ;;
    *) url="$1"; shift ;;
  esac
done

# Health check.
if [[ "$url" == */health/liveness ]]; then
  echo "OK"
  exit 0
fi

# MCP endpoint.
if [[ "$url" == */mcp ]] && [ -n "$data" ]; then
  tool=$(echo "$data" | jq -r '.params.name')
  args=$(echo "$data" | jq -c '.params.arguments')

  case "$tool" in
    register_agent)
      name=$(echo "$args" | jq -r '.name // empty')
      if [ -z "$name" ]; then
        name="AutoAgent$(next_id)"
      fi
      echo "$name" > "$STATE_DIR/agents/$name"
      jq -n --arg name "$name" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ({"id": 1, "name": $name, "program": "gc", "model": "agent"} | tojson)}] }
      }'
      ;;

    ensure_project)
      key=$(echo "$args" | jq -r '.human_key')
      jq -n --arg key "$key" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ({"id": 1, "slug": "test", "human_key": $key, "created_at": "2026-01-01T00:00:00Z"} | tojson)}] }
      }'
      ;;

    send_message)
      id=$(next_id)
      sender=$(echo "$args" | jq -r '.sender_name')
      to=$(echo "$args" | jq -r '.to[0]')
      subject=$(echo "$args" | jq -r '.subject')
      body_md=$(echo "$args" | jq -r '.body_md')
      ts=$(now_ts)

      # Store message for inbox/read.
      jq -n --argjson id "$id" --arg sender "$sender" --arg to "$to" \
        --arg subject "$subject" --arg body_md "$body_md" --arg ts "$ts" \
        '{
          id: $id,
          from: $sender,
          to: $to,
          subject: $subject,
          body_md: $body_md,
          created_ts: $ts,
          acknowledged: false
        }' > "$STATE_DIR/messages/$id.json"

      # Return deliveries format (matches mcp_agent_mail v0.3.0).
      msg=$(cat "$STATE_DIR/messages/$id.json")
      jq -n --argjson msg "$msg" --arg project "/test/conformance" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ({
          deliveries: [{project: $project, payload: $msg}],
          count: 1
        } | tojson)}] }
      }'
      ;;

    fetch_inbox)
      name=$(echo "$args" | jq -r '.agent_name')
      include_bodies=$(echo "$args" | jq -r '.include_bodies // false')
      # Return ALL messages for this recipient (the script does local
      # read/archived filtering). mcp_agent_mail returns all messages too.
      msgs="[]"
      for f in "$STATE_DIR/messages/"*.json; do
        [ -f "$f" ] || continue
        msg=$(cat "$f")
        rcpt=$(echo "$msg" | jq -r '.to')
        if [ "$rcpt" = "$name" ]; then
          if [ "$include_bodies" = "true" ]; then
            msgs=$(echo "$msgs" | jq --argjson m "$msg" '. + [$m]')
          else
            msgs=$(echo "$msgs" | jq --argjson m "$msg" '. + [($m | del(.body_md))]')
          fi
        fi
      done
      jq -n --argjson msgs "$msgs" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ($msgs | tojson)}] }
      }'
      ;;

    acknowledge_message)
      mid=$(echo "$args" | jq -r '.message_id')
      file="$STATE_DIR/messages/$mid.json"
      if [ ! -f "$file" ]; then
        jq -n '{
          jsonrpc: "2.0", id: 1,
          error: {code: -32000, message: "message not found"}
        }'
        exit 0
      fi
      # mcp_agent_mail acknowledge is idempotent — no error on re-ack.
      contents=$(jq '.acknowledged = true' "$file")
      echo "$contents" > "$file"
      ts=$(now_ts)
      jq -n --argjson mid "$mid" --arg ts "$ts" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ({"message_id": $mid, "acknowledged": true, "acknowledged_at": $ts, "read_at": $ts} | tojson)}] }
      }'
      ;;

    mark_message_read)
      mid=$(echo "$args" | jq -r '.message_id')
      file="$STATE_DIR/messages/$mid.json"
      if [ ! -f "$file" ]; then
        jq -n '{
          jsonrpc: "2.0", id: 1,
          error: {code: -32000, message: "message not found"}
        }'
        exit 0
      fi
      ts=$(now_ts)
      jq -n --argjson mid "$mid" --arg ts "$ts" '{
        jsonrpc: "2.0", id: 1,
        result: { content: [{type: "text", text: ({"message_id": $mid, "read": true, "read_at": $ts} | tojson)}] }
      }'
      ;;

    *)
      jq -n --arg tool "$tool" '{
        jsonrpc: "2.0", id: 1,
        error: {code: -32601, message: ("unknown tool: " + $tool)}
      }'
      ;;
  esac
  exit 0
fi

echo "mock curl: unhandled: $url" >&2
exit 1
`
}
