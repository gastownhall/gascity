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
		BlockerID:       "sr-iaq7",
		BlockerKind:     "wisp",
		BlockerTitle:    "mol-dog-stale-db",
		BlockerAgeMS:    26_760_000,
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
	for _, key := range []string{
		"order_name", "consecutive", "first_suppressed", "suppressed_for_ms",
		"blocker_id", "blocker_kind", "blocker_title", "blocker_age_ms",
	} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("payload JSON is missing %q: %s", key, raw)
		}
	}
}

// TestOrderSuppressedPayloadOmitsUnresolvedBlockerFields pins the best-effort
// half of the wire contract. Blocker resolution is a separate read that can time
// out or race the blocker closing; when it reports nothing the event still goes
// out, and its payload must be byte-identical to what a reader saw before these
// fields existed — an empty blocker_id on the wire would read as "resolved, and
// the answer is nothing".
func TestOrderSuppressedPayloadOmitsUnresolvedBlockerFields(t *testing.T) {
	t.Parallel()

	raw := OrderSuppressedPayloadJSON(OrderSuppressedPayload{
		OrderName:       "mol-dog-stale-db",
		Consecutive:     20,
		FirstSuppressed: "2026-08-24T05:00:23Z",
		SuppressedForMS: 1_200_000,
	})
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range []string{"blocker_id", "blocker_kind", "blocker_title", "blocker_age_ms"} {
		if _, ok := shape[key]; ok {
			t.Fatalf("unresolved %q must be omitted, not emitted empty: %s", key, raw)
		}
	}
	if len(shape) != 4 {
		t.Fatalf("payload without a blocker has %d keys, want the original 4: %s", len(shape), raw)
	}
}

func TestOrderSkippedIsAKnownEventTypeWithATypedPayload(t *testing.T) {
	t.Parallel()

	if !slices.Contains(KnownEventTypes, OrderSkipped) {
		t.Fatalf("%q is missing from KnownEventTypes; the SSE projection would carry it untyped", OrderSkipped)
	}
	sample, ok := LookupPayload(OrderSkipped)
	if !ok {
		t.Fatalf("%q has no registered payload", OrderSkipped)
	}
	if _, ok := sample.(OrderSkippedPayload); !ok {
		t.Fatalf("%q registered payload is %T, want OrderSkippedPayload", OrderSkipped, sample)
	}
}

func TestOrderSkippedPayloadRoundTrips(t *testing.T) {
	t.Parallel()

	want := OrderSkippedPayload{
		OrderName: "core.reaper",
		Outcome:   "partial",
		Reason:    "some scopes or steps did not run",
		Scopes:    []OrderSkippedScope{{Scope: "rig:api", Reason: "bead store unreachable"}},
	}
	raw := OrderSkippedPayloadJSON(want)

	decoded, typed, err := DecodePayload(OrderSkipped, raw)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if !typed {
		t.Fatal("DecodePayload reported no registered type for order.skipped")
	}
	got, ok := decoded.(OrderSkippedPayload)
	if !ok {
		t.Fatalf("DecodePayload returned %T, want OrderSkippedPayload", decoded)
	}
	if got.OrderName != want.OrderName || got.Outcome != want.Outcome || got.Reason != want.Reason || !slices.Equal(got.Scopes, want.Scopes) {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
}

func TestOrderSkippedPayloadJSONAlwaysCarriesAScopesArray(t *testing.T) {
	t.Parallel()

	raw := OrderSkippedPayloadJSON(OrderSkippedPayload{OrderName: "core.jsonl-export", Outcome: "skipped", Reason: "no bead scope could be exported"})
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := shape["scopes"].([]any); !ok {
		t.Fatalf("scopes must be a JSON array, got %s", raw)
	}
}
