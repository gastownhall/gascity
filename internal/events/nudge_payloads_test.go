package events

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestNudgeDialogBlockedIsAKnownEventTypeWithATypedPayload mirrors
// TestControlStalledIsAKnownEventTypeWithATypedPayload: pins both halves of
// the registration so a constant that never made it into KnownEventTypes, or
// a payload that never got registered, fails loudly here instead of shipping
// an untyped envelope on the SSE wire.
func TestNudgeDialogBlockedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, NudgeDialogBlocked) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", NudgeDialogBlocked)
	}
	sample, ok := LookupPayload(NudgeDialogBlocked)
	if !ok {
		t.Fatalf("%q has no registered payload", NudgeDialogBlocked)
	}
	if _, ok := sample.(NudgeDialogBlockedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want NudgeDialogBlockedPayload", NudgeDialogBlocked, sample)
	}
}

func TestNudgeDialogBlockedPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := NudgeDialogBlockedPayload{
		SessionID:  "ses-abc123",
		DialogKind: "bug_report_draft_modal",
		BeadID:     "ga-1yqxh7",
	}
	raw := NudgeDialogBlockedPayloadJSON(want)

	decoded, typed, err := DecodePayload(NudgeDialogBlocked, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for nudge.dialog_blocked")
	}
	got, ok := decoded.(NudgeDialogBlockedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want NudgeDialogBlockedPayload", decoded)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}

	// The typed-wire invariant: every field is a named scalar, so the JSON has
	// a fixed shape rather than a free-form bag.
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range []string{"session_id", "dialog_kind", "bead_id"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}

// TestNudgeDialogBlockedPayloadOmitsEmptyBeadID locks in the deliberate
// omitempty on BeadID: the gate fires before any queued item is claimed, so
// BeadID is legitimately absent on the common path, not a bug.
func TestNudgeDialogBlockedPayloadOmitsEmptyBeadID(t *testing.T) {
	t.Parallel()

	raw := NudgeDialogBlockedPayloadJSON(NudgeDialogBlockedPayload{
		SessionID:  "ses-abc123",
		DialogKind: "bug_report_draft_modal",
	})

	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := shape["bead_id"]; ok {
		t.Fatalf("bead_id should be omitted when empty (gate fires before any item is claimed): %s", raw)
	}
}
