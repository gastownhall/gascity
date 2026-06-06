package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionRawMessageFrameEscapesLiteralNewlinesInRawJSONStrings(t *testing.T) {
	// The raw frame intentionally contains literal newline bytes inside the
	// JSON string value — this is the malformed producer output we're repairing.
	rawFrame := json.RawMessage("{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"line one\n\nline two\"}]}")

	encoded, err := json.Marshal(SessionStreamRawMessageEvent{
		ID:       "mc-test",
		Template: "worker",
		Provider: "claude",
		Format:   "raw",
		Messages: []SessionRawMessageFrame{{Raw: rawFrame}},
	})
	if err != nil {
		t.Fatalf("Marshal SessionStreamRawMessageEvent: %v", err)
	}

	body := string(encoded)
	if strings.Contains(body, "\n") {
		t.Fatalf("encoded raw stream event contains physical newline: %q", body)
	}
	if !strings.Contains(body, `line one\n\nline two`) {
		t.Fatalf("encoded raw stream event did not preserve escaped text newlines: %q", body)
	}
	if !json.Valid(encoded) {
		t.Fatalf("encoded raw stream event is not valid JSON: %q", body)
	}
}
