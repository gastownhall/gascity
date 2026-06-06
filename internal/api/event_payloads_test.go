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

func TestSessionLifecyclePayloadRoundTrip(t *testing.T) {
	raw := SessionLifecyclePayloadJSON("sess-123", "deacon", "killed")

	got, registered, err := events.DecodePayload(events.SessionStopped, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("registered = false, want true")
	}
	payload, ok := got.(SessionLifecyclePayload)
	if !ok {
		t.Fatalf("payload = %T, want SessionLifecyclePayload", got)
	}
	if payload.SessionID != "sess-123" {
		t.Fatalf("SessionID = %q, want sess-123", payload.SessionID)
	}
	if payload.Template != "deacon" {
		t.Fatalf("Template = %q, want deacon", payload.Template)
	}
	if payload.Reason != "killed" {
		t.Fatalf("Reason = %q, want killed", payload.Reason)
	}
}

func TestSessionLifecyclePayloadOmitemptyTemplateAndReason(t *testing.T) {
	raw := SessionLifecyclePayloadJSON("sess-7", "", "")
	if got := string(raw); got != `{"session_id":"sess-7"}` {
		t.Fatalf("payload = %s, want only session_id field", got)
	}

	// Round-trip through the registry to ensure the typed shape decodes
	// cleanly even when only session_id is present on the wire.
	decoded, registered, err := events.DecodePayload(events.SessionCrashed, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !registered {
		t.Fatal("registered = false, want true")
	}
	payload, ok := decoded.(SessionLifecyclePayload)
	if !ok {
		t.Fatalf("payload = %T, want SessionLifecyclePayload", decoded)
	}
	if payload.SessionID != "sess-7" {
		t.Fatalf("SessionID = %q, want sess-7", payload.SessionID)
	}
	if payload.Template != "" {
		t.Fatalf("Template = %q, want empty", payload.Template)
	}
	if payload.Reason != "" {
		t.Fatalf("Reason = %q, want empty", payload.Reason)
	}
}

func TestSessionLifecyclePayloadRegisteredForStoppedAndCrashed(t *testing.T) {
	for _, et := range []string{events.SessionStopped, events.SessionCrashed} {
		sample, ok := events.LookupPayload(et)
		if !ok {
			t.Fatalf("event %q has no registered payload", et)
		}
		if _, ok := sample.(SessionLifecyclePayload); !ok {
			t.Fatalf("event %q payload sample = %T, want SessionLifecyclePayload", et, sample)
		}
	}
}

func TestDecodeBeadEventPayloadCoercesNonStringMetadata(t *testing.T) {
	raw := json.RawMessage(`{"bead":{"id":"bd-123","title":"test bead","status":"open","issue_type":"session","created_at":"2026-04-26T21:37:46Z","metadata":{"generation":3,"pending_create_claim":true,"wake_attempts":0}}}`)

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
	if payload.Bead.Metadata["generation"] != "3" {
		t.Fatalf("generation = %q, want 3", payload.Bead.Metadata["generation"])
	}
	if payload.Bead.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true", payload.Bead.Metadata["pending_create_claim"])
	}
	if payload.Bead.Metadata["wake_attempts"] != "0" {
		t.Fatalf("wake_attempts = %q, want 0", payload.Bead.Metadata["wake_attempts"])
	}
}
