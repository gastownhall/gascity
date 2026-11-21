package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Structured history must expose a provider failure rather than an assistant completion.
func TestClaudeAPIErrorProjectsSystemFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	raw := `{"uuid":"failure","type":"assistant","isApiErrorMessage":true,"error":"invalid_request","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: request blocked"}],"stop_reason":"refusal"}}`
	if err := os.WriteFile(path, []byte(raw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeEntry("claude", path, "session", 0, session.Messages[0])
	if got.Actor != ActorSystem || got.Kind != "system" || got.Status != ResultStatusFinal {
		t.Fatalf("actor=%s kind=%s status=%s", got.Actor, got.Kind, got.Status)
	}
	if got.SystemEvent == nil || got.SystemEvent.Kind != "error" || got.SystemEvent.Category != "provider_error" || got.SystemEvent.Code != "invalid_request" || got.SystemEvent.Message != "API Error: request blocked" {
		t.Fatalf("event=%+v", got.SystemEvent)
	}
	if string(got.Provenance.Raw) != raw {
		t.Fatal("native provider evidence changed")
	}
}
