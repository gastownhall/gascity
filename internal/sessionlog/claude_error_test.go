package sessionlog

import (
	"encoding/json"
	"testing"
)

func TestClaudeRetryOverridesEarlierIdleActivity(t *testing.T) {
	entries := []*Entry{
		{Type: "assistant", Message: json.RawMessage(`{"stop_reason":"end_turn"}`)},
		{Type: "system", Subtype: "api_error"},
	}
	if got := InferActivityFromEntries(entries); got != "in-turn" {
		t.Fatalf("retry activity=%q, want in-turn", got)
	}
}
