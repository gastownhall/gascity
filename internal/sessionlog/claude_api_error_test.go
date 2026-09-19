package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Explicit provider failures after partial assistant output must remain failures.
func TestClaudeAPIErrorAfterPartialResponse(t *testing.T) {
	partial := `{"uuid":"partial","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Partial response"}],"stop_reason":"refusal"}}`
	failure := `{"uuid":"failure","parentUuid":"partial","type":"assistant","isApiErrorMessage":true,"error":"invalid_request","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: Request blocked by provider safeguards."}],"stop_reason":"refusal"}}`
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(partial+"\n"+failure+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := ReadFile(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Messages) != 2 {
		t.Fatalf("messages=%d", len(session.Messages))
	}
	if session.Messages[0].Type != "assistant" || session.Messages[0].SystemEvent != nil {
		t.Fatal("ordinary assistant refusal was reclassified")
	}
	got := session.Messages[1]
	if got.Type != "system" || got.SystemEvent == nil {
		t.Fatalf("API error classification: type=%q event=%+v", got.Type, got.SystemEvent)
	}
	if got.SystemEvent.Kind != "error" || got.SystemEvent.Category != "provider_error" || got.SystemEvent.Code != "invalid_request" || got.SystemEvent.Message != "API Error: Request blocked by provider safeguards." {
		t.Fatalf("event=%+v", got.SystemEvent)
	}
	if string(got.Raw) != failure {
		t.Fatal("raw provider record changed")
	}
}

func TestClaudeAPIErrorRequiresExplicitFlag(t *testing.T) {
	for _, flag := range []string{"false", "true"} {
		t.Run(flag, func(t *testing.T) {
			raw := `{"uuid":"entry","type":"assistant","isApiErrorMessage":` + flag + `,"message":{"role":"assistant","content":"API Error: quoted example"}}`
			p := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(p, []byte(raw+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := ReadFile(p, 0)
			if err != nil {
				t.Fatal(err)
			}
			entry := session.Messages[0]
			if (entry.SystemEvent != nil) != (flag == "true") {
				t.Fatalf("event=%+v for flag %s", entry.SystemEvent, flag)
			}
			if flag == "true" && entry.SystemEvent.Message != "API Error: quoted example" {
				t.Fatal("plain text content lost")
			}
			var original map[string]json.RawMessage
			if err := json.Unmarshal(entry.Raw, &original); err != nil {
				t.Fatal(err)
			}
		})
	}
}
