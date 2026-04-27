package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func TestDecodeBeadEventPayloadWrapped(t *testing.T) {
	raw := json.RawMessage(`{"bead":{"id":"bd-123","title":"test bead","status":"open","issue_type":"task","created_at":"2026-04-26T21:37:46Z","metadata":{"state":"awake"}}}`)

	got, registered, err := events.DecodePayload(events.BeadUpdated, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("registered = false, want true")
	}
	payload, ok := got.(BeadEventPayload)
	if !ok {
		t.Fatalf("payload = %T, want BeadEventPayload", got)
	}
	if payload.Bead.ID != "bd-123" {
		t.Fatalf("bead id = %q, want bd-123", payload.Bead.ID)
	}
	if payload.Bead.Metadata["state"] != "awake" {
		t.Fatalf("metadata state = %q, want awake", payload.Bead.Metadata["state"])
	}
	if payload.Bead.CreatedAt != time.Date(2026, 4, 26, 21, 37, 46, 0, time.UTC) {
		t.Fatalf("created_at = %s, want 2026-04-26T21:37:46Z", payload.Bead.CreatedAt.Format(time.RFC3339))
	}
}

func TestDecodeBeadEventPayloadLegacyRawBead(t *testing.T) {
	raw := json.RawMessage(`{"id":"bd-123","title":"test bead","status":"open","issue_type":"task","created_at":"2026-04-26T21:37:46Z","metadata":{"state":"awake"}}`)

	got, registered, err := events.DecodePayload(events.BeadUpdated, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("registered = false, want true")
	}
	payload, ok := got.(BeadEventPayload)
	if !ok {
		t.Fatalf("payload = %T, want BeadEventPayload", got)
	}
	if payload.Bead.ID != "bd-123" {
		t.Fatalf("bead id = %q, want bd-123", payload.Bead.ID)
	}
	if payload.Bead.Metadata["state"] != "awake" {
		t.Fatalf("metadata state = %q, want awake", payload.Bead.Metadata["state"])
	}
}
