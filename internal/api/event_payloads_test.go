package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
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

// TestDecodeBeadEventPayloadCanonicalRawBead covers the CANONICAL producer
// shape: the raw bead snapshot json.Marshal(b) emits (what notifyChange writes
// and every events.jsonl row holds). The wrapped {"bead":...} form
// (TestDecodeBeadEventPayloadWrapped) is the tolerant fallback, not the reverse.
func TestDecodeBeadEventPayloadCanonicalRawBead(t *testing.T) {
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

func TestSessionConfigDriftDeferredPayloadContract(t *testing.T) {
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		name   string
		fields []string
		want   string
	}{
		{"missing breakdown stays an empty list", nil, `[]`},
		{"field names", []string{"command", "env"}, `["command","env"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := SessionConfigDriftDeferredPayloadJSON("gc-1", "", "worker", tc.fields, "bd-9", since, 90*time.Minute+500*time.Millisecond, true)
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if got := string(wire["drifted_fields"]); got != tc.want {
				t.Errorf("drifted_fields = %s, want %s", got, tc.want)
			}
			if _, ok := wire["session_name"]; ok {
				t.Errorf("session_name present though unset: %s", raw)
			}
			got, registered, err := events.DecodePayload(events.SessionConfigDriftDeferred, raw)
			if err != nil || !registered {
				t.Fatalf("DecodePayload = %v, registered %v", err, registered)
			}
			p, ok := got.(SessionConfigDriftDeferredPayload)
			if !ok {
				t.Fatalf("payload = %T, want SessionConfigDriftDeferredPayload", got)
			}
			if p.SessionID != "gc-1" || p.Template != "worker" || p.WorkBeadID != "bd-9" || !p.Escalated {
				t.Errorf("payload = %+v", p)
			}
			if p.DeferredSince != "2026-03-04T04:06:07Z" || p.PendingSeconds != 5400 {
				t.Errorf("deferred_since = %q, pending_seconds = %d; want 2026-03-04T04:06:07Z, 5400", p.DeferredSince, p.PendingSeconds)
			}
		})
	}
	schema := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer).
		Schema(reflect.TypeOf(SessionConfigDriftDeferredPayload{}), false, "")
	if f := schema.Properties["drifted_fields"]; f == nil || f.Nullable {
		t.Errorf("drifted_fields schema = %+v, want a non-nullable array", f)
	}
	for _, name := range []string{"session_id", "template", "drifted_fields", "work_bead_id", "deferred_since", "pending_seconds", "escalated"} {
		if !slices.Contains(schema.Required, name) {
			t.Errorf("%s not required in %v", name, schema.Required)
		}
	}
	if author, ok := events.RegisteredAuthorship(events.SessionConfigDriftDeferred); !ok || author != events.AuthorController {
		t.Errorf("authorship = %v, %v; want controller", author, ok)
	}
}
