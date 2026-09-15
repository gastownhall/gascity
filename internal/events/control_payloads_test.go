package events

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestControlStalledIsAKnownEventTypeWithATypedPayload pins both halves of the
// registration. The payload half is also enforced downstream by
// TestEveryKnownEventTypeHasRegisteredPayload, but that test iterates
// KnownEventTypes — so a constant that never made it into the list would be
// invisible to it, and the SSE wire would carry control.stalled as an untyped
// envelope. Before this event there was no control.* vocabulary at all, which
// is why a control-plane brownout was unobservable from the event bus.
func TestControlStalledIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, ControlStalled) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", ControlStalled)
	}
	sample, ok := LookupPayload(ControlStalled)
	if !ok {
		t.Fatalf("%q has no registered payload", ControlStalled)
	}
	if _, ok := sample.(ControlStalledPayload); !ok {
		t.Fatalf("%q registered payload is %T, want ControlStalledPayload", ControlStalled, sample)
	}
}

func TestControlStalledPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := ControlStalledPayload{
		BeadID:     "pl-mmneh",
		Kind:       "scope-check",
		RootBeadID: "pl-afi46",
		StorePath:  "/data/cities/platform",
		ErrorClass: "semantic",
		FirstSeen:  "2026-08-11T08:00:47Z",
		Attempts:   6712,
		Error:      "cannot close blocked issue: pl-pujtf is blocked by [pl-mmneh]",
		OrderName:  "core.technical-health-patrol",
	}
	raw := ControlStalledPayloadJSON(want)

	decoded, typed, err := DecodePayload(ControlStalled, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for control.stalled")
	}
	got, ok := decoded.(ControlStalledPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want ControlStalledPayload", decoded)
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
	for _, key := range []string{"bead_id", "kind", "root_bead_id", "store_path", "error_class", "first_seen", "attempts", "error", "order_name"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}

// TestControlRootSettleFailedIsAKnownEventTypeWithATypedPayload mirrors
// TestControlStalledIsAKnownEventTypeWithATypedPayload above: pins both
// halves of the registration so a constant that never made it into
// KnownEventTypes, or a payload that never got registered, fails loudly here
// instead of shipping an untyped envelope on the SSE wire.
func TestControlRootSettleFailedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, ControlRootSettleFailed) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", ControlRootSettleFailed)
	}
	sample, ok := LookupPayload(ControlRootSettleFailed)
	if !ok {
		t.Fatalf("%q has no registered payload", ControlRootSettleFailed)
	}
	if _, ok := sample.(ControlRootSettleFailedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want ControlRootSettleFailedPayload", ControlRootSettleFailed, sample)
	}
}

func TestControlRootSettleFailedPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := ControlRootSettleFailedPayload{
		RootBeadID:      "su-d04es",
		FinalizerBeadID: "su-5e9db",
		StorePath:       "/data/cities/substrate",
		ErrorClass:      "hard",
		Error:           "cannot close blocked issue: su-d04es is blocked by [su-5e9db]",
		FollowUpBeadID:  "su-9a3fq",
	}
	raw := ControlRootSettleFailedPayloadJSON(want)

	decoded, typed, err := DecodePayload(ControlRootSettleFailed, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for control.root_settle_failed")
	}
	got, ok := decoded.(ControlRootSettleFailedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want ControlRootSettleFailedPayload", decoded)
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
	for _, key := range []string{"root_bead_id", "finalizer_bead_id", "store_path", "error_class", "error", "follow_up_bead_id"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}
