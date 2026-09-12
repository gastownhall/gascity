package events

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestOrderSuppressedIsAKnownEventTypeWithATypedPayload pins both halves of the
// registration. TestEveryKnownEventTypeHasRegisteredPayload enforces the
// payload half downstream, but it iterates KnownEventTypes — a constant that
// never made the list would be invisible to it and order.suppressed would reach
// the SSE wire as an untyped envelope.
func TestOrderSuppressedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, OrderSuppressed) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", OrderSuppressed)
	}
	sample, ok := LookupPayload(OrderSuppressed)
	if !ok {
		t.Fatalf("%q has no registered payload", OrderSuppressed)
	}
	if _, ok := sample.(OrderSuppressedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want OrderSuppressedPayload", OrderSuppressed, sample)
	}
}

func TestOrderSuppressedPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := OrderSuppressedPayload{
		OrderName:       "core.technical-health-patrol",
		Consecutive:     412,
		FirstSuppressed: "2026-08-11T08:00:47Z",
		SuppressedForMS: 12_360_000,
	}
	raw := OrderSuppressedPayloadJSON(want)

	decoded, typed, err := DecodePayload(OrderSuppressed, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for order.suppressed")
	}
	got, ok := decoded.(OrderSuppressedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want OrderSuppressedPayload", decoded)
	}
	if got != want {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}

	// The typed-wire invariant: every field is a named scalar, so the JSON has a
	// fixed shape rather than a free-form bag.
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range []string{"order_name", "consecutive", "first_suppressed", "suppressed_for_ms"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}
